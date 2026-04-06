package main

import (
	"context"
	"fmt"
	"io"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
)

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
