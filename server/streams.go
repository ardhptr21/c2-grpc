package main

import (
	"io"

	pb "github.com/ardhptr21/c2-grpc/pb"
)

func (s *shellServer) AgentShell(stream pb.ShellService_AgentShellServer) error {
	var agentID string

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			if agentID != "" {
				agentShellsMu.Lock()
				delete(agentShells, agentID)
				agentShellsMu.Unlock()
				closeShellSessionsForAgent(agentID)
			}
			return nil
		}
		if err != nil {
			if agentID != "" {
				agentShellsMu.Lock()
				delete(agentShells, agentID)
				agentShellsMu.Unlock()
				closeShellSessionsForAgent(agentID)
			}
			return err
		}

		if event.GetType() == "register" {
			agentID = event.GetAgentId()
			agentShellsMu.Lock()
			agentShells[agentID] = &agentShellConn{stream: stream}
			agentShellsMu.Unlock()
			continue
		}

		shellSessionsMu.Lock()
		session, ok := shellSessions[event.GetSessionId()]
		shellSessionsMu.Unlock()
		if !ok {
			continue
		}
		_ = sendToOperatorShell(session.OperatorID, event)
		if event.GetType() == "closed" || event.GetType() == "open_error" {
			shellSessionsMu.Lock()
			delete(shellSessions, event.GetSessionId())
			shellSessionsMu.Unlock()
		}
	}
}

func (s *shellServer) OperatorShell(stream pb.ShellService_OperatorShellServer) error {
	operatorID := shortUUID(8)

	operatorShellsMu.Lock()
	operatorShells[operatorID] = &operatorShellConn{stream: stream}
	operatorShellsMu.Unlock()
	defer func() {
		operatorShellsMu.Lock()
		delete(operatorShells, operatorID)
		operatorShellsMu.Unlock()
		closeShellSessionsForOperator(operatorID)
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF || stream.Context().Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}

		switch req.GetType() {
		case "open":
			if req.GetAgentId() == "" {
				_ = sendToOperatorShell(operatorID, &pb.AgentShellEvent{
					Type:    "open_error",
					AgentId: req.GetAgentId(),
					Message: "agent_id is required",
				})
				continue
			}

			sessionID := shortUUID(12)
			shellSessionsMu.Lock()
			shellSessions[sessionID] = shellSession{
				AgentID:    req.GetAgentId(),
				OperatorID: operatorID,
			}
			shellSessionsMu.Unlock()

			forward := &pb.OperatorShellRequest{
				Type:      "open",
				SessionId: sessionID,
				AgentId:   req.GetAgentId(),
				Cols:      req.GetCols(),
				Rows:      req.GetRows(),
			}
			if err := sendToAgentShell(req.GetAgentId(), forward); err != nil {
				shellSessionsMu.Lock()
				delete(shellSessions, sessionID)
				shellSessionsMu.Unlock()
				_ = sendToOperatorShell(operatorID, &pb.AgentShellEvent{
					Type:      "open_error",
					SessionId: sessionID,
					AgentId:   req.GetAgentId(),
					Message:   err.Error(),
				})
			}
		case "input", "resize", "close":
			shellSessionsMu.Lock()
			session, ok := shellSessions[req.GetSessionId()]
			shellSessionsMu.Unlock()
			if !ok {
				continue
			}
			req.AgentId = session.AgentID
			if err := sendToAgentShell(session.AgentID, req); err != nil {
				_ = sendToOperatorShell(operatorID, &pb.AgentShellEvent{
					Type:      "closed",
					SessionId: req.GetSessionId(),
					AgentId:   session.AgentID,
					Message:   err.Error(),
				})
				shellSessionsMu.Lock()
				delete(shellSessions, req.GetSessionId())
				shellSessionsMu.Unlock()
			}
			if req.GetType() == "close" {
				shellSessionsMu.Lock()
				delete(shellSessions, req.GetSessionId())
				shellSessionsMu.Unlock()
			}
		}
	}
}

func (s *fileServer) AgentTransfer(stream pb.FileService_AgentTransferServer) error {
	var agentID string

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			if agentID != "" {
				agentFilesMu.Lock()
				delete(agentFiles, agentID)
				agentFilesMu.Unlock()
				closeFileTransfersForAgent(agentID)
			}
			return nil
		}
		if err != nil {
			if agentID != "" {
				agentFilesMu.Lock()
				delete(agentFiles, agentID)
				agentFilesMu.Unlock()
				closeFileTransfersForAgent(agentID)
			}
			return err
		}

		if event.GetType() == "register" {
			agentID = event.GetAgentId()
			agentFilesMu.Lock()
			agentFiles[agentID] = &agentFileConn{stream: stream}
			agentFilesMu.Unlock()
			continue
		}

		fileTransfersMu.Lock()
		transfer, ok := fileTransfers[event.GetTransferId()]
		fileTransfersMu.Unlock()
		if !ok {
			continue
		}

		_ = sendToOperatorFile(transfer.OperatorID, event)
		switch event.GetType() {
		case "upload_done", "download_done", "list_done", "error":
			fileTransfersMu.Lock()
			delete(fileTransfers, event.GetTransferId())
			fileTransfersMu.Unlock()
		}
	}
}

func (s *fileServer) OperatorTransfer(stream pb.FileService_OperatorTransferServer) error {
	operatorID := shortUUID(8)

	operatorFilesMu.Lock()
	operatorFiles[operatorID] = &operatorFileConn{stream: stream}
	operatorFilesMu.Unlock()
	defer func() {
		operatorFilesMu.Lock()
		delete(operatorFiles, operatorID)
		operatorFilesMu.Unlock()
		closeFileTransfersForOperator(operatorID)
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF || stream.Context().Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}

		switch req.GetType() {
		case "upload_start", "download", "list":
			if req.GetAgentId() == "" || req.GetTransferId() == "" {
				_ = sendToOperatorFile(operatorID, &pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    req.GetAgentId(),
					Message:    "agent_id and transfer_id are required",
				})
				continue
			}

			fileTransfersMu.Lock()
			fileTransfers[req.GetTransferId()] = fileTransfer{
				AgentID:    req.GetAgentId(),
				OperatorID: operatorID,
			}
			fileTransfersMu.Unlock()

			if err := sendToAgentFile(req.GetAgentId(), req); err != nil {
				fileTransfersMu.Lock()
				delete(fileTransfers, req.GetTransferId())
				fileTransfersMu.Unlock()
				_ = sendToOperatorFile(operatorID, &pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    req.GetAgentId(),
					Path:       req.GetPath(),
					Message:    err.Error(),
				})
			}
		case "upload_chunk", "upload_end", "cancel":
			fileTransfersMu.Lock()
			transfer, ok := fileTransfers[req.GetTransferId()]
			fileTransfersMu.Unlock()
			if !ok {
				continue
			}
			req.AgentId = transfer.AgentID
			if err := sendToAgentFile(transfer.AgentID, req); err != nil {
				_ = sendToOperatorFile(operatorID, &pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    transfer.AgentID,
					Path:       req.GetPath(),
					Message:    err.Error(),
				})
				fileTransfersMu.Lock()
				delete(fileTransfers, req.GetTransferId())
				fileTransfersMu.Unlock()
			}
			if req.GetType() == "cancel" {
				fileTransfersMu.Lock()
				delete(fileTransfers, req.GetTransferId())
				fileTransfersMu.Unlock()
			}
		}
	}
}
