package dm

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hanfei1991/microcosm/jobmaster/dm/config"
	"github.com/hanfei1991/microcosm/jobmaster/dm/metadata"
	"github.com/hanfei1991/microcosm/lib"
	libModel "github.com/hanfei1991/microcosm/lib/model"
	"github.com/hanfei1991/microcosm/model"
	dmpkg "github.com/hanfei1991/microcosm/pkg/dm"
)

func TestUpdateWorkerHandle(t *testing.T) {
	messageAgent := NewMessageAgent("mock-jobmaster", nil)
	require.Equal(t, lenSyncMap(messageAgent.senders), 0)
	workerHandle := lib.NewTombstoneWorkerHandle("", libModel.WorkerStatus{}, nil)

	// add worker handle
	messageAgent.UpdateWorkerHandle("task1", workerHandle)
	require.Equal(t, lenSyncMap(messageAgent.senders), 1)
	w, ok := messageAgent.senders.Load("task1")
	require.True(t, ok)
	require.Equal(t, w, workerHandle)

	// remove worker handle
	messageAgent.UpdateWorkerHandle("task2", nil)
	require.Equal(t, lenSyncMap(messageAgent.senders), 1)
	w, ok = messageAgent.senders.Load("task1")
	require.True(t, ok)
	require.Equal(t, w, workerHandle)
	messageAgent.UpdateWorkerHandle("task1", nil)
	require.Equal(t, lenSyncMap(messageAgent.senders), 0)
}

func TestOperateWorker(t *testing.T) {
	mockMasterImpl := &MockMasterImpl{}
	messageAgent := NewMessageAgent("mock-jobmaster", mockMasterImpl)
	task1 := "task1"
	worker1 := "worker1"

	// create worker
	_, err := messageAgent.CreateWorker(context.Background(), task1, lib.WorkerDMDump, &config.TaskCfg{})
	require.NoError(t, err)
	// create again
	_, err = messageAgent.CreateWorker(context.Background(), task1, lib.WorkerDMDump, &config.TaskCfg{})
	require.NoError(t, err)
	// create again
	workerHandle := lib.NewTombstoneWorkerHandle(worker1, libModel.WorkerStatus{}, nil)
	messageAgent.UpdateWorkerHandle(task1, workerHandle)
	_, err = messageAgent.CreateWorker(context.Background(), task1, lib.WorkerDMDump, &config.TaskCfg{})
	require.EqualError(t, err, fmt.Sprintf("worker for task %s already exist", task1))

	// destory worker
	require.EqualError(t, messageAgent.DestroyWorker(context.Background(), "task-not-exist", "worker-not-exist"), fmt.Sprintf("worker for task %s not exist", "task-not-exist"))
	require.EqualError(t, messageAgent.DestroyWorker(context.Background(), task1, "worker-not-exist"), fmt.Sprintf("worker for task %s mismatch: want %s, get %s", task1, "worker-not-exist", worker1))
	// worker offline
	require.Error(t, messageAgent.DestroyWorker(context.Background(), task1, worker1))
	// worker normal
	messageAgent.UpdateWorkerHandle(task1, &MockSender{id: worker1})
	require.NoError(t, messageAgent.DestroyWorker(context.Background(), task1, worker1))
}

func TestOperateTask(t *testing.T) {
	mockMasterImpl := &MockMasterImpl{}
	messageAgent := NewMessageAgent("mock-jobmaster", mockMasterImpl)
	task1 := "task1"
	worker1 := "worker1"

	workerHandle := lib.NewTombstoneWorkerHandle(worker1, libModel.WorkerStatus{}, nil)
	messageAgent.UpdateWorkerHandle(task1, workerHandle)
	// worker offline
	require.Error(t, messageAgent.OperateTask(context.Background(), task1, metadata.StagePaused))
	// worker normal
	messageAgent.UpdateWorkerHandle(task1, &MockSender{id: worker1})
	require.NoError(t, messageAgent.OperateTask(context.Background(), task1, metadata.StagePaused))
	// task not exist
	require.EqualError(t, messageAgent.OperateTask(context.Background(), "task-not-exist", metadata.StagePaused), fmt.Sprintf("worker for task %s not exist", "task-not-exist"))
}

func TestOnWorkerMessage(t *testing.T) {
	messageAgent := NewMessageAgent("", nil)
	require.EqualError(t, messageAgent.OnWorkerMessage(dmpkg.MessageWithID{ID: 0, Message: "response"}), "request 0 not found")
}

func lenSyncMap(m sync.Map) int {
	var i int
	m.Range(func(k, v interface{}) bool {
		i++
		return true
	})
	return i
}

type MockMasterImpl struct{}

func (m *MockMasterImpl) CreateWorker(workerType lib.WorkerType, config lib.WorkerConfig, cost model.RescUnit) (lib.WorkerID, error) {
	return "mock-worker", nil
}

func (m *MockMasterImpl) CurrentEpoch() lib.Epoch {
	return 0
}

func (m *MockMasterImpl) JobMasterID() lib.MasterID {
	return "mock-jobmaster"
}

type MockSender struct {
	id lib.WorkerID
}

func (s *MockSender) ID() lib.WorkerID {
	return s.id
}

func (s *MockSender) SendMessage(ctx context.Context, topic string, message interface{}, nonblocking bool) error {
	return nil
}