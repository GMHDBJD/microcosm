package master

import (
	"context"
	"fmt"
	"net"

	"github.com/hanfei1991/microcosm/master/cluster"
	"github.com/hanfei1991/microcosm/model"
	"github.com/hanfei1991/microcosm/pb"
	"github.com/hanfei1991/microcosm/pkg/errors"
	"github.com/hanfei1991/microcosm/pkg/etcdutils"
	"github.com/hanfei1991/microcosm/test"
	"github.com/hanfei1991/microcosm/test/mock"
	"github.com/pingcap/ticdc/dm/pkg/etcdutil"
	"github.com/pingcap/ticdc/dm/pkg/log"
	"github.com/pingcap/ticdc/pkg/p2p"
	p2ppb "github.com/pingcap/ticdc/proto/p2p"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/embed"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// Server handles PRC requests for df master.
type Server struct {
	etcd *embed.Etcd

	etcdClient *clientv3.Client
	// election *election.Election

	// sched scheduler
	executorManager *cluster.ExecutorManager
	jobManager      *JobManager
	p2pServer       *p2p.MessageServer
	//
	cfg *Config

	// mocked server for test
	mockGrpcServer mock.GrpcServer
}

// NewServer creates a new master-server.
func NewServer(cfg *Config, ctx *test.Context) (*Server, error) {
	executorNotifier := make(chan model.ExecutorID, 100)
	executorManager := cluster.NewExecutorManager(executorNotifier, cfg.KeepAliveTTL, cfg.KeepAliveInterval, ctx)

	urls, err := parseURLs(cfg.MasterAddr)
	if err != nil {
		return nil, err
	}
	masterAddrs := make([]string, 0, len(urls))
	for _, u := range urls {
		masterAddrs = append(masterAddrs, u.Host)
	}
	jobManager := NewJobManager(executorManager, executorManager, executorNotifier, masterAddrs)
	server := &Server{
		cfg:             cfg,
		executorManager: executorManager,
		jobManager:      jobManager,
	}
	return server, nil
}

// Heartbeat implements pb interface.
func (s *Server) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	return s.executorManager.HandleHeartbeat(req)
}

// SubmitJob passes request onto "JobManager".
func (s *Server) SubmitJob(ctx context.Context, req *pb.SubmitJobRequest) (*pb.SubmitJobResponse, error) {
	return s.jobManager.SubmitJob(ctx, req), nil
}

// RegisterExecutor implements grpc interface, and passes request onto executor manager.
func (s *Server) RegisterExecutor(ctx context.Context, req *pb.RegisterExecutorRequest) (*pb.RegisterExecutorResponse, error) {
	// register executor to scheduler
	// TODO: check leader, if not leader, return notLeader error.
	execInfo, err := s.executorManager.AddExecutor(req)
	if err != nil {
		log.L().Logger.Error("add executor failed", zap.Error(err))
		return &pb.RegisterExecutorResponse{
			Err: errors.ToPBError(err),
		}, nil
	}
	return &pb.RegisterExecutorResponse{
		ExecutorId: int32(execInfo.ID),
	}, nil
}

// ScheduleTask implements grpc interface. It works as follows
// - receives request from job master
// - queries resource manager to allocate resource and maps tasks to executors
// - returns scheduler response to job master
func (s *Server) ScheduleTask(ctx context.Context, req *pb.TaskSchedulerRequest) (*pb.TaskSchedulerResponse, error) {
	// TODO: support running resource manager independently, and get resource snapshot via rpc.
	snapshot := s.executorManager.GetResourceSnapshot()
	if len(snapshot.Executors) == 0 {
		return nil, errors.ErrClusterResourceNotEnough.GenWithStackByArgs()
	}
	tasks := req.GetTasks()
	success, scheduleResp := s.allocateTasksWithNaiveStrategy(snapshot, tasks)
	if !success {
		return nil, errors.ErrClusterResourceNotEnough.GenWithStackByArgs()
	}
	return scheduleResp, nil
}

// DeleteExecutor deletes an executor, but have yet implemented.
func (s *Server) DeleteExecutor() {
	// To implement
}

// RegisterMetaStore registers backend metastore to server master,
// but have not implemented yet.
func (s *Server) RegisterMetaStore(
	ctx context.Context, req *pb.RegisterMetaStoreRequest,
) (*pb.RegisterMetaStoreResponse, error) {
	return nil, nil
}

// QueryMetaStore implements gRPC interface
func (s *Server) QueryMetaStore(
	ctx context.Context, req *pb.QueryMetaStoreRequest,
) (*pb.QueryMetaStoreResponse, error) {
	switch req.Tp {
	case pb.StoreType_ServiceDiscovery:
		return &pb.QueryMetaStoreResponse{
			Address: s.cfg.AdvertiseAddr,
		}, nil
	case pb.StoreType_SystemMetaStore:
		// TODO: independent system metastore
		return &pb.QueryMetaStoreResponse{
			Address: s.cfg.AdvertiseAddr,
		}, nil
	default:
		return &pb.QueryMetaStoreResponse{
			Err: &pb.Error{
				Code:    pb.ErrorCode_InvalidMetaStoreType,
				Message: fmt.Sprintf("store type: %s", req.Tp),
			},
		}, nil
	}
}

func (s *Server) startForTest(ctx context.Context) (err error) {
	// TODO: implement mock-etcd and leader election

	s.mockGrpcServer, err = mock.NewMasterServer(s.cfg.MasterAddr, s)
	if err != nil {
		return err
	}

	s.executorManager.Start(ctx)
	s.jobManager.Start(ctx, nil)

	return nil
}

// Stop and clean resources.
// TODO: implement stop gracefully.
func (s *Server) Stop() {
	if s.mockGrpcServer != nil {
		s.mockGrpcServer.Stop()
	}
}

// Start the master-server.
func (s *Server) Start(ctx context.Context) (err error) {
	if test.GlobalTestFlag {
		return s.startForTest(ctx)
	}
	etcdCfg := etcdutils.GenEmbedEtcdConfigWithLogger(s.cfg.LogLevel)
	// prepare to join an existing etcd cluster.
	//err = prepareJoinEtcd(s.cfg)
	//if err != nil {
	//	return
	//}
	log.L().Info("config after join prepared", zap.Stringer("config", s.cfg))

	// generates embed etcd config before any concurrent gRPC calls.
	// potential concurrent gRPC calls:
	//   - workerrpc.NewGRPCClient
	//   - getHTTPAPIHandler
	// no `String` method exists for embed.Config, and can not marshal it to join too.
	// but when starting embed etcd server, the etcd pkg will log the config.
	// https://github.com/etcd-io/etcd/blob/3cf2f69b5738fb702ba1a935590f36b52b18979b/embed/etcd.go#L299
	etcdCfg, err = etcdutils.GenEmbedEtcdConfig(etcdCfg, s.cfg.MasterAddr, s.cfg.AdvertiseAddr, s.cfg.Etcd)
	if err != nil {
		return
	}

	p2pConfig := new(p2p.MessageServerConfig)
	s.p2pServer = p2p.NewMessageServer("master", p2pConfig)

	gRPCSvr := func(gs *grpc.Server) {
		pb.RegisterMasterServer(gs, s)
		p2ppb.RegisterCDCPeerToPeerServer(gs, s.p2pServer)
	}

	// TODO: implement http api/
	//apiHandler, err := getHTTPAPIHandler(ctx, s.cfg.AdvertiseAddr, tls2.ToGRPCDialOption())
	//if err != nil {
	//	return
	//}

	// generate grpcServer
	s.etcd, err = startEtcd(etcdCfg, gRPCSvr, nil, etcdStartTimeout)
	if err != nil {
		return
	}

	log.L().Logger.Info("start etcd successfully")

	// start grpc server

	s.etcdClient, err = etcdutil.CreateClient([]string{withHost(s.cfg.MasterAddr)}, nil)
	if err != nil {
		return
	}

	// start leader election
	// TODO: Consider election. And Notify workers when leader changes.
	// s.election, err = election.NewElection(ctx, )

	// start keep alive
	s.executorManager.Start(ctx)
	s.jobManager.Start(ctx, s.p2pServer)
	return nil
}

func withHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// do nothing
		return addr
	}
	if len(host) == 0 {
		return fmt.Sprintf("127.0.0.1:%s", port)
	}

	return addr
}
