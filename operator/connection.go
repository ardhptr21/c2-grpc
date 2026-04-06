package main

import (
	"context"
	"fmt"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func maintainOperatorConnection(ctx context.Context, serverAddr string, ch chan<- tea.Msg) {
	defer close(ch)

	for {
		if ctx.Err() != nil {
			return
		}

		dialCtx, cancelDial := context.WithTimeout(ctx, 5*time.Second)
		conn, err := grpc.DialContext(
			dialCtx,
			serverAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(),
		)
		cancelDial()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case ch <- operatorDisconnectedMsg{message: fmt.Sprintf("Disconnected from server. Reconnecting to %s...", serverAddr)}:
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		client := pb.NewOperatorServiceClient(conn)
		historyClient := pb.NewHistoryServiceClient(conn)
		shellClient := pb.NewShellServiceClient(conn)
		fileClient := pb.NewFileServiceClient(conn)
		stream, err := client.Connect(ctx)
		if err != nil {
			_ = conn.Close()
			select {
			case <-ctx.Done():
				return
			case ch <- operatorDisconnectedMsg{message: fmt.Sprintf("Disconnected from server. Reconnecting to %s...", serverAddr)}:
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		shellStream, err := shellClient.OperatorShell(ctx)
		if err != nil {
			_ = stream.CloseSend()
			_ = conn.Close()
			select {
			case <-ctx.Done():
				return
			case ch <- operatorDisconnectedMsg{message: fmt.Sprintf("Disconnected from server. Reconnecting to %s...", serverAddr)}:
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		fileStream, err := fileClient.OperatorTransfer(ctx)
		if err != nil {
			_ = stream.CloseSend()
			_ = shellStream.CloseSend()
			_ = conn.Close()
			select {
			case <-ctx.Done():
				return
			case ch <- operatorDisconnectedMsg{message: fmt.Sprintf("Disconnected from server. Reconnecting to %s...", serverAddr)}:
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			_ = shellStream.CloseSend()
			_ = fileStream.CloseSend()
			_ = conn.Close()
			return
		case ch <- operatorConnectedMsg{stream: stream, historyClient: historyClient, shellStream: shellStream, fileStream: fileStream}:
		}

		disconnectCh := make(chan struct{}, 2)

		go forwardOperatorEvents(ctx, stream, disconnectCh, ch)
		go forwardShellEvents(ctx, shellStream, disconnectCh, ch)
		go forwardFileEvents(ctx, fileStream, disconnectCh, ch)

		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			_ = shellStream.CloseSend()
			_ = fileStream.CloseSend()
			_ = conn.Close()
			return
		case <-disconnectCh:
		}

		_ = stream.CloseSend()
		_ = shellStream.CloseSend()
		_ = fileStream.CloseSend()
		_ = conn.Close()

		select {
		case <-ctx.Done():
			return
		case ch <- operatorDisconnectedMsg{message: fmt.Sprintf("Disconnected from server. Reconnecting to %s...", serverAddr)}:
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func forwardOperatorEvents(ctx context.Context, stream pb.OperatorService_ConnectClient, disconnectCh chan<- struct{}, ch chan<- tea.Msg) {
	for {
		event, err := stream.Recv()
		if err != nil {
			disconnectCh <- struct{}{}
			return
		}
		select {
		case <-ctx.Done():
			disconnectCh <- struct{}{}
			return
		case ch <- operatorEventMsg{event: event}:
		}
	}
}

func forwardShellEvents(ctx context.Context, stream pb.ShellService_OperatorShellClient, disconnectCh chan<- struct{}, ch chan<- tea.Msg) {
	for {
		event, err := stream.Recv()
		if err != nil {
			disconnectCh <- struct{}{}
			return
		}
		select {
		case <-ctx.Done():
			disconnectCh <- struct{}{}
			return
		case ch <- shellEventMsg{event: event}:
		}
	}
}

func forwardFileEvents(ctx context.Context, stream pb.FileService_OperatorTransferClient, disconnectCh chan<- struct{}, ch chan<- tea.Msg) {
	for {
		event, err := stream.Recv()
		if err != nil {
			disconnectCh <- struct{}{}
			return
		}
		select {
		case <-ctx.Done():
			disconnectCh <- struct{}{}
			return
		case ch <- fileEventMsg{event: event}:
		}
	}
}

func fetchAgentHistory(client pb.HistoryServiceClient, agentID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resp, err := client.ListAgentHistory(ctx, &pb.AgentHistoryRequest{
			AgentId: agentID,
			Limit:   50,
		})
		if err != nil {
			return historyErrorMsg{message: fmt.Sprintf("history load error: %v", err)}
		}
		return historyLoadedMsg{agentID: agentID, entries: resp.GetEntries()}
	}
}
