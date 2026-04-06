package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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
