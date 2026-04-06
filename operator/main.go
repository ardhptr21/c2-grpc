package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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

type operatorModel struct {
	stream                pb.OperatorService_ConnectClient
	historyClient         pb.HistoryServiceClient
	shellStream           pb.ShellService_OperatorShellClient
	events                chan tea.Msg
	commandInput          textinput.Model
	logViewport           viewport.Model
	historyListViewport   viewport.Model
	historyDetailViewport viewport.Model
	shellViewport         viewport.Model
	screen                string
	activeTab             string
	logContent            string
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

	vp := viewport.New(80, 20)
	vp.SetContent("Waiting for operator events...")
	historyList := viewport.New(40, 20)
	historyDetail := viewport.New(80, 20)
	shellView := viewport.New(80, 20)

	return operatorModel{
		events:                make(chan tea.Msg, 64),
		commandInput:          input,
		logViewport:           vp,
		historyListViewport:   historyList,
		historyDetailViewport: historyDetail,
		shellViewport:         shellView,
		screen:                "landing",
		activeTab:             "live",
		agents:                make(map[string]agentRow),
		historyCache:          make(map[string][]*pb.AgentHistoryEntry),
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
		m.status = "Connected. Use ↑/↓ to pick an agent and Enter to dispatch."
		return m, waitForOperatorEvent(m.events)
	case operatorDisconnectedMsg:
		m.stream = nil
		m.historyClient = nil
		m.shellStream = nil
		m.agents = make(map[string]agentRow)
		m.agentOrder = nil
		m.selected = 0
		m.historyEntries = nil
		m.historySelected = 0
		m.historyAgentID = ""
		m.historyCache = make(map[string][]*pb.AgentHistoryEntry)
		m.shellSessionID = ""
		m.shellAgentID = ""
		m.shellOutput = ""
		m.shellReady = false
		m.shellInputBuffer = ""
		m.activeTab = "live"
		if msg.message != "" {
			m.status = msg.message
		} else {
			m.status = "Disconnected from server. Reconnecting..."
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
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.stream != nil {
				_ = m.stream.CloseSend()
			}
			if m.shellStream != nil {
				_ = m.shellStream.CloseSend()
			}
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
		}

		if m.screen == "landing" {
			return m, nil
		}

		switch msg.String() {
		case "tab":
			if m.activeTab == "live" {
				if cmd := m.activateHistoryTab(); cmd != nil {
					return m, cmd
				}
			} else if m.activeTab == "history" {
				if cmd := m.activateShellTab(); cmd != nil {
					return m, cmd
				}
			} else {
				break
			}
			return m, nil
		case "shift+tab":
			if m.activeTab == "history" {
				m.activeTab = "live"
			} else if m.activeTab == "shell" {
				break
			}
			return m, nil
		}

		if m.activeTab == "shell" {
			m.handleShellKey(msg)
			return m, nil
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
		return m, nil
	}

	var cmd tea.Cmd
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
	card := panelStyle.Width(maxInt(48, m.width/2)).Render(
		titleStyle.Render("c2-grpc") + "\n" +
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
			mutedStyle.Render("Tab opens History. Shift+Tab returns to Live. ↑/↓ or Left/Right select entries. Mouse wheel, PgUp/PgDn, Home/End scroll output.") + "\n" +
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
			mutedStyle.Render("Type directly into the shell. F1 Live, F2 History, F3 Shell. Mouse wheel, PgUp/PgDn, Home/End scroll viewport.") + "\n" +
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
	case "agent_dead":
		row := m.agents[event.GetAgentId()]
		row.ID = event.GetAgentId()
		row.Summary = event.GetPayload()
		row.Status = "DEAD"
		m.agents[event.GetAgentId()] = row
		m.rebuildAgentOrder()
	case "agent_removed":
		delete(m.agents, event.GetAgentId())
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
	for id := range m.agents {
		m.agentOrder = append(m.agentOrder, id)
	}
	sort.Strings(m.agentOrder)
	if m.selected >= len(m.agentOrder) && len(m.agentOrder) > 0 {
		m.selected = len(m.agentOrder) - 1
	}
	if len(m.agentOrder) == 0 {
		m.selected = 0
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
	m.refreshHistoryViewports()
	m.refreshShellViewport()
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
	if m.activeTab == "live" {
		liveTab = tabActive.Render("Live")
	} else if m.activeTab == "history" {
		historyTab = tabActive.Render("History")
	} else {
		shellTab = tabActive.Render("Shell")
	}

	return titleStyle.Render("c2-grpc Operator") + "\n" +
		lipgloss.JoinHorizontal(lipgloss.Left, liveTab, " ", historyTab, " ", shellTab) + "\n" +
		mutedStyle.Render(subtitle)
}

func (m operatorModel) renderFooter(width int) string {
	return panelStyle.Width(width).Render(
		"Selected: " + m.selectedAgentLabel() + "\n" +
			m.commandInput.View() + "\n" +
			mutedStyle.Render("Enter dispatches. Tab cycles tabs. F1 Live, F2 History, F3 Shell. ↑/↓ select agents. PgUp/PgDn, Home/End, Ctrl+U/Ctrl+D scroll output. q quits.") + "\n" +
			m.status,
	)
}

func (m operatorModel) renderAgents() string {
	if len(m.agentOrder) == 0 {
		return "Agents\n\nNo agents connected."
	}

	rows := make([]string, 0, len(m.agentOrder)+2)
	rows = append(rows, "Agents", "")
	for i, id := range m.agentOrder {
		agent := m.agents[id]
		row := fmt.Sprintf("%s  %s", shortAgentID(id), agent.Summary)
		if agent.Status == "DEAD" {
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

		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			_ = shellStream.CloseSend()
			_ = conn.Close()
			return
		case ch <- operatorConnectedMsg{stream: stream, historyClient: historyClient, shellStream: shellStream}:
		}

		disconnectCh := make(chan struct{}, 2)

		go func() {
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
		}()

		go func() {
			for {
				event, err := shellStream.Recv()
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
		}()

		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			_ = shellStream.CloseSend()
			_ = conn.Close()
			return
		case <-disconnectCh:
		}

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

func formatEvent(event *pb.OperatorEvent) string {
	switch event.GetType() {
	case "agent_joined":
		return fmt.Sprintf("[+] agent:%s  %s", shortAgentID(event.GetAgentId()), event.GetPayload())
	case "agent_dead":
		return fmt.Sprintf("[!] agent:%s  %s", shortAgentID(event.GetAgentId()), event.GetPayload())
	case "agent_removed":
		return fmt.Sprintf("[-] agent:%s  %s", shortAgentID(event.GetAgentId()), event.GetPayload())
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
	if m.historyClient == nil {
		m.status = "History service unavailable."
		return nil
	}
	if len(m.agentOrder) == 0 {
		m.status = "No agents connected."
		return nil
	}

	agentID := m.agentOrder[m.selected]
	m.activeTab = "history"
	if cached, ok := m.historyCache[agentID]; ok {
		m.historyAgentID = agentID
		m.historyEntries = cloneHistoryEntries(cached)
		m.historySelected = 0
		m.refreshHistoryViewports()
		m.status = fmt.Sprintf("Showing cached history for agent:%s", shortAgentID(agentID))
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
}

func (m *operatorModel) applyShellEvent(event *pb.AgentShellEvent) {
	switch event.GetType() {
	case "open_ok":
		m.shellSessionID = event.GetSessionId()
		m.shellAgentID = event.GetAgentId()
		m.shellReady = true
		m.shellOutput = ""
		m.shellInputBuffer = ""
		m.refreshShellViewport()
		m.resizeShell()
		m.status = fmt.Sprintf("Shell connected to agent:%s", shortAgentID(event.GetAgentId()))
	case "open_error":
		m.shellSessionID = ""
		m.shellReady = false
		m.status = "shell open error: " + event.GetMessage()
	case "output":
		if event.GetSessionId() != m.shellSessionID {
			return
		}
		m.appendShellOutput(event.GetData())
		const maxShellBytes = 128 * 1024
		if len(m.shellOutput) > maxShellBytes {
			m.shellOutput = m.shellOutput[len(m.shellOutput)-maxShellBytes:]
		}
		m.refreshShellViewport()
	case "closed":
		if event.GetSessionId() != m.shellSessionID {
			return
		}
		m.shellReady = false
		m.shellSessionID = ""
		m.shellInputBuffer = ""
		m.status = "shell closed"
		if event.GetMessage() != "" {
			m.status = "shell closed: " + event.GetMessage()
		}
	}
}

func (m *operatorModel) refreshShellViewport() {
	if m.shellOutput == "" {
		m.shellViewport.SetContent("No shell output yet.")
		return
	}
	m.shellViewport.SetContent(m.shellOutput)
	m.shellViewport.GotoBottom()
}

func (m *operatorModel) activateShellTab() tea.Cmd {
	if m.shellStream == nil {
		m.status = "Shell service unavailable."
		return nil
	}
	if len(m.agentOrder) == 0 {
		m.status = "No agents connected."
		return nil
	}

	agentID := m.agentOrder[m.selected]
	m.activeTab = "shell"
	if m.shellReady && m.shellAgentID == agentID {
		m.status = fmt.Sprintf("Shell attached to agent:%s", shortAgentID(agentID))
		return nil
	}

	if m.shellSessionID != "" && m.shellAgentID != "" {
		_ = m.shellStream.Send(&pb.OperatorShellRequest{
			Type:      "close",
			SessionId: m.shellSessionID,
		})
	}

	m.shellSessionID = ""
	m.shellReady = false
	m.shellAgentID = agentID
	m.shellOutput = ""
	m.shellInputBuffer = ""
	m.refreshShellViewport()
	m.status = fmt.Sprintf("Opening shell for agent:%s...", shortAgentID(agentID))

	if err := m.shellStream.Send(&pb.OperatorShellRequest{
		Type:    "open",
		AgentId: agentID,
		Cols:    int32(maxInt(m.shellViewport.Width, 80)),
		Rows:    int32(maxInt(m.shellViewport.Height, 24)),
	}); err != nil {
		m.status = fmt.Sprintf("shell open send error: %v", err)
	}
	return nil
}

func (m *operatorModel) sendShellInput(data string) {
	if !m.shellReady || m.shellStream == nil || m.shellSessionID == "" {
		return
	}
	if err := m.shellStream.Send(&pb.OperatorShellRequest{
		Type:      "input",
		SessionId: m.shellSessionID,
		Data:      data,
	}); err != nil {
		m.status = fmt.Sprintf("shell input error: %v", err)
	}
}

func (m *operatorModel) handleShellKey(msg tea.KeyMsg) {
	switch msg.String() {
	case "enter":
		if strings.TrimSpace(m.shellInputBuffer) == "clear" {
			m.sendShellInput("\x15")
			m.shellOutput = ""
			m.shellInputBuffer = ""
			m.refreshShellViewport()
			m.status = "Shell pane cleared."
			return
		}
		m.sendShellInput("\r")
		m.shellInputBuffer = ""
		return
	case "backspace":
		m.sendShellInput("\x7f")
		if len(m.shellInputBuffer) > 0 {
			runes := []rune(m.shellInputBuffer)
			m.shellInputBuffer = string(runes[:len(runes)-1])
		}
		return
	case "delete":
		m.sendShellInput("\x7f")
		if len(m.shellInputBuffer) > 0 {
			runes := []rune(m.shellInputBuffer)
			m.shellInputBuffer = string(runes[:len(runes)-1])
		}
		return
	case "ctrl+u":
		m.sendShellInput("\x15")
		m.shellInputBuffer = ""
		return
	case "ctrl+w":
		m.sendShellInput("\x17")
		m.shellInputBuffer = trimLastShellWord(m.shellInputBuffer)
		return
	case "ctrl+l":
		m.sendShellInput("\x0c")
		m.shellInputBuffer = ""
		return
	case "ctrl+d":
		m.sendShellInput("\x04")
		return
	case "pgup":
		m.shellViewport.HalfViewUp()
		return
	case "pgdown":
		m.shellViewport.HalfViewDown()
		return
	case "home":
		m.shellViewport.GotoTop()
		return
	case "end":
		m.shellViewport.GotoBottom()
		return
	}

	if input, ok := shellInputForKey(msg); ok {
		m.sendShellInput(input)
		if len(msg.Runes) > 0 {
			m.shellInputBuffer += string(msg.Runes)
		}
	}
}

func (m operatorModel) renderShellInputIndicator() string {
	if !m.shellReady {
		return ""
	}
	return m.shellInputBuffer + "|"
}

func trimLastShellWord(s string) string {
	runes := []rune(s)
	for len(runes) > 0 && runes[len(runes)-1] == ' ' {
		runes = runes[:len(runes)-1]
	}
	for len(runes) > 0 && runes[len(runes)-1] != ' ' {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
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

func shellInputForKey(msg tea.KeyMsg) (string, bool) {
	switch msg.String() {
	case "enter":
		return "\r", true
	case "backspace":
		return "\x7f", true
	case "tab":
		return "\t", true
	case "space":
		return " ", true
	case "up":
		return "\x1b[A", true
	case "down":
		return "\x1b[B", true
	case "right":
		return "\x1b[C", true
	case "left":
		return "\x1b[D", true
	case "home":
		return "\x1b[H", true
	case "end":
		return "\x1b[F", true
	case "delete":
		return "\x1b[3~", true
	}
	if len(msg.Runes) > 0 {
		return string(msg.Runes), true
	}
	return "", false
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
