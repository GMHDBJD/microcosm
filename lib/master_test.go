package lib

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/clientv3"

	"github.com/hanfei1991/microcosm/pkg/adapter"
	derror "github.com/hanfei1991/microcosm/pkg/errors"
	"github.com/hanfei1991/microcosm/pkg/metadata"
	"github.com/hanfei1991/microcosm/pkg/uuid"
)

const (
	masterName            = "my-master"
	masterNodeName        = "node-1"
	executorNodeID1       = "node-exec-1"
	executorNodeID2       = "node-exec-2"
	executorNodeID3       = "node-exec-3"
	workerTypePlaceholder = 999
	workerID1             = WorkerID("worker-1")
	workerID2             = WorkerID("worker-2")
	workerID3             = WorkerID("worker-3")
)

type dummyConfig struct {
	param int
}

func prepareMeta(ctx context.Context, t *testing.T, metaclient metadata.MetaKV) {
	masterKey := adapter.MasterMetaKey.Encode(masterName)
	masterInfo := &MasterMetaKVData{
		ID:     masterName,
		NodeID: masterNodeName,
	}
	masterInfoBytes, err := json.Marshal(masterInfo)
	require.NoError(t, err)
	_, err = metaclient.Put(ctx, masterKey, string(masterInfoBytes))
	require.NoError(t, err)
}

func TestMasterInit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	master := NewMockMasterImpl("", masterName)
	prepareMeta(ctx, t, master.metaKVClient)

	master.On("InitImpl", mock.Anything).Return(nil)
	err := master.Init(ctx)
	require.NoError(t, err)

	rawResp, err := master.metaKVClient.Get(ctx, adapter.MasterMetaKey.Encode(masterName))
	require.NoError(t, err)
	resp := rawResp.(*clientv3.GetResponse)
	require.Len(t, resp.Kvs, 1)

	var masterData MasterMetaKVData
	err = json.Unmarshal(resp.Kvs[0].Value, &masterData)
	require.NoError(t, err)
	require.True(t, masterData.Initialized)

	master.On("CloseImpl", mock.Anything).Return(nil)
	err = master.Close(ctx)
	require.NoError(t, err)

	// Restart the master
	master.Reset()
	master.On("OnMasterRecovered", mock.Anything).Return(nil)
	err = master.Init(ctx)
	require.NoError(t, err)

	master.On("CloseImpl", mock.Anything).Return(nil)
	err = master.Close(ctx)
	require.NoError(t, err)
}

func TestMasterPollAndClose(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	master := NewMockMasterImpl("", masterName)
	prepareMeta(ctx, t, master.metaKVClient)

	master.On("InitImpl", mock.Anything).Return(nil)
	err := master.Init(ctx)
	require.NoError(t, err)

	master.On("Tick", mock.Anything).Return(nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			err := master.Poll(ctx)
			if err != nil {
				if derror.ErrMasterClosed.Equal(err) {
					return
				}
			}
			require.NoError(t, err)
		}
	}()

	require.Eventually(t, func() bool {
		return master.TickCount() > 10
	}, time.Millisecond*2000, time.Millisecond*10)

	master.On("CloseImpl", mock.Anything).Return(nil)
	err = master.Close(ctx)
	require.NoError(t, err)

	wg.Wait()
}

func TestMasterCreateWorker(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	master := NewMockMasterImpl("", masterName)
	master.timeoutConfig.masterHeartbeatCheckLoopInterval = time.Millisecond * 10
	master.uuidGen = uuid.NewMock()
	prepareMeta(ctx, t, master.metaKVClient)

	master.On("InitImpl", mock.Anything).Return(nil)
	err := master.Init(ctx)
	require.NoError(t, err)

	MockBaseMasterCreateWorker(
		t,
		master.DefaultBaseMaster,
		workerTypePlaceholder,
		&dummyConfig{param: 1},
		100,
		masterName,
		workerID1,
		executorNodeID1)

	workerID, err := master.CreateWorker(workerTypePlaceholder, &dummyConfig{param: 1}, 100)
	require.NoError(t, err)
	require.Equal(t, workerID1, workerID)

	master.On("OnWorkerDispatched", mock.AnythingOfType("*lib.workerHandleImpl"), nil).Return(nil)
	<-master.dispatchedWorkers

	master.On("OnWorkerOnline", mock.AnythingOfType("*lib.workerHandleImpl")).Return(nil)

	MockBaseMasterWorkerHeartbeat(t, master.DefaultBaseMaster, masterName, workerID1, executorNodeID1)

	master.On("Tick", mock.Anything).Return(nil)
	err = master.Poll(ctx)
	require.NoError(t, err)

	require.Eventuallyf(t, func() bool {
		return master.onlineWorkerCount.Load() == 1
	}, time.Second*1, time.Millisecond*10, "final worker count %d", master.onlineWorkerCount.Load())

	workerList := master.GetWorkers()
	require.Len(t, workerList, 1)
	require.Contains(t, workerList, workerID)

	err = master.messageHandlerManager.InvokeHandler(t, StatusUpdateTopic(masterName, workerID1), masterName, &StatusUpdateMessage{
		WorkerID: workerID1,
		Status: WorkerStatus{
			Code:     WorkerStatusNormal,
			ExtBytes: []byte(`{"Val":4}`),
		},
	})
	require.NoError(t, err)
	status := master.GetWorkers()[workerID1].Status()
	require.Equal(t, &WorkerStatus{
		Code: WorkerStatusNormal,
		Ext:  &dummyStatus{Val: 4},
	}, status)
}
