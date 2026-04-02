package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"sort"
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
	stream pb.OperatorService_ConnectClient
}

type operatorDisconnectedMsg struct {
	message string
}

type operatorModel struct {
	stream       pb.OperatorService_ConnectClient
	events       chan tea.Msg
	commandInput textinput.Model
	logViewport  viewport.Model
	logContent   string
	agents       map[string]agentRow
	agentOrder   []string
	selected     int
	width        int
	height       int
	status       string
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
	serverAddr := flag.String("server", "localhost:50051", "gRPC server address")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newOperatorModel()
	go maintainOperatorConnection(ctx, *serverAddr, m.events)

	p := tea.NewProgram(m, tea.WithAltScreen())
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

	return operatorModel{
		events:       make(chan tea.Msg, 64),
		commandInput: input,
		logViewport:  vp,
		agents:       make(map[string]agentRow),
		status:       "Connecting to server...",
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
		m.applyEvent(msg.event)
		m.refreshLogs()
		return m, waitForOperatorEvent(m.events)
	case operatorConnectedMsg:
		m.stream = msg.stream
		m.status = "Connected. Use ↑/↓ to pick an agent and Enter to dispatch."
		return m, waitForOperatorEvent(m.events)
	case operatorDisconnectedMsg:
		m.stream = nil
		m.agents = make(map[string]agentRow)
		m.agentOrder = nil
		m.selected = 0
		if msg.message != "" {
			m.status = msg.message
		} else {
			m.status = "Disconnected from server. Reconnecting..."
		}
		return m, waitForOperatorEvent(m.events)
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.stream != nil {
				_ = m.stream.CloseSend()
			}
			return m, tea.Quit
		case "pgup", "b":
			m.logViewport.HalfViewUp()
			return m, nil
		case "pgdown", "f":
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
	}

	var cmd tea.Cmd
	m.commandInput, cmd = m.commandInput.Update(msg)
	return m, cmd
}

func (m operatorModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
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

func (m *operatorModel) applyEvent(event *pb.OperatorEvent) {
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
	}

	m.appendEvent(event)
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
	return titleStyle.Render("c2-grpc Operator") + "\n" + mutedStyle.Render("Live command execution with a remote agent roster.")
}

func (m operatorModel) renderFooter(width int) string {
	return panelStyle.Width(width).Render(
		"Selected: " + m.selectedAgentLabel() + "\n" +
			m.commandInput.View() + "\n" +
			mutedStyle.Render("Enter dispatches. ↑/↓ select agents. PgUp/PgDn, Home/End, Ctrl+U/Ctrl+D scroll output. q quits.") + "\n" +
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
		row := fmt.Sprintf("%s  %s", id, agent.Summary)
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
	return "agent:" + m.agentOrder[m.selected]
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

		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			_ = conn.Close()
			return
		case ch <- operatorConnectedMsg{stream: stream}:
		}

		disconnected := false
		for !disconnected {
			event, err := stream.Recv()
			if err == io.EOF {
				disconnected = true
				break
			}
			if err != nil {
				disconnected = true
				break
			}
			select {
			case <-ctx.Done():
				_ = stream.CloseSend()
				_ = conn.Close()
				return
			case ch <- operatorEventMsg{event: event}:
			}
		}

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
	}
}

func formatEvent(event *pb.OperatorEvent) string {
	switch event.GetType() {
	case "agent_joined":
		return fmt.Sprintf("[+] agent:%s  %s", event.GetAgentId(), event.GetPayload())
	case "agent_dead":
		return fmt.Sprintf("[!] agent:%s  %s", event.GetAgentId(), event.GetPayload())
	case "agent_removed":
		return fmt.Sprintf("[-] agent:%s  %s", event.GetAgentId(), event.GetPayload())
	case "output":
		return event.GetPayload()
	case "ack":
		return fmt.Sprintf("[~] agent:%s  %s", event.GetAgentId(), event.GetPayload())
	case "error":
		return fmt.Sprintf("[x] agent:%s  %s", event.GetAgentId(), event.GetPayload())
	default:
		return fmt.Sprintf("[?] %s agent:%s  %s", event.GetType(), event.GetAgentId(), event.GetPayload())
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
