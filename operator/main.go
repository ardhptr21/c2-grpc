package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

type operatorDisconnectedMsg struct{}

type operatorModel struct {
	stream       pb.OperatorService_ConnectClient
	events       chan tea.Msg
	commandInput textinput.Model
	logViewport  viewport.Model
	logs         []string
	agents       map[string]agentRow
	agentOrder   []string
	selected     int
	width        int
	height       int
	status       string
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

	conn, err := grpc.Dial(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial error: %v", err)
	}
	defer conn.Close()

	client := pb.NewOperatorServiceClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		log.Fatalf("connect error: %v", err)
	}

	m := newOperatorModel(stream)
	go receiveEvents(stream, m.events)

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatalf("operator ui error: %v", err)
	}
}

func newOperatorModel(stream pb.OperatorService_ConnectClient) operatorModel {
	input := textinput.New()
	input.Placeholder = "Type a command, e.g. ls, whoami, or sysinfo"
	input.Focus()
	input.CharLimit = 256
	input.Prompt = "cmd> "

	vp := viewport.New(80, 20)
	vp.SetContent("Waiting for operator events...")

	return operatorModel{
		stream:       stream,
		events:       make(chan tea.Msg, 64),
		commandInput: input,
		logViewport:  vp,
		agents:       make(map[string]agentRow),
		status:       "Connected. Use ↑/↓ to pick an agent and Enter to dispatch.",
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
	case operatorDisconnectedMsg:
		m.status = "Disconnected from server."
		return m, tea.Quit
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			_ = m.stream.CloseSend()
			return m, tea.Quit
		case "up":
			if len(m.agentOrder) > 0 && m.selected > 0 {
				m.selected--
			}
			return m, nil
		case "down":
			if len(m.agentOrder) > 0 && m.selected < len(m.agentOrder)-1 {
				m.selected++
			}
			return m, nil
		case "enter":
			commandLine := strings.TrimSpace(m.commandInput.Value())
			if commandLine == "" {
				m.status = "Enter a command first."
				return m, nil
			}
			if len(m.agentOrder) == 0 {
				m.status = "No agents connected."
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
	header := titleStyle.Render("ShadowNet Operator") + "\n" + mutedStyle.Render("Live command execution with a remote agent roster.")
	agentsPane := panelStyle.Width(maxInt(28, m.width/3)).Render(m.renderAgents())
	logPaneWidth := maxInt(48, m.width-maxInt(28, m.width/3)-8)
	logPane := panelStyle.Width(logPaneWidth).Height(maxInt(12, m.height-11)).Render(m.logViewport.View())
	footer := panelStyle.Render(
		"Selected: " + m.selectedAgentLabel() + "\n" +
			m.commandInput.View() + "\n" +
			mutedStyle.Render("Enter dispatches the command to the selected agent. q quits.") + "\n" +
			m.status,
	)

	return appStyle.Render(lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		lipgloss.JoinHorizontal(lipgloss.Top, agentsPane, logPane),
		footer,
	))
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
	}

	m.logs = append(m.logs, formatEvent(event))
	if len(m.logs) > 400 {
		m.logs = m.logs[len(m.logs)-400:]
	}
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
	m.logViewport.SetContent(strings.Join(m.logs, "\n"))
	m.logViewport.GotoBottom()
}

func (m *operatorModel) resize() {
	leftWidth := maxInt(28, m.width/3)
	rightWidth := maxInt(48, m.width-leftWidth-8)
	logHeight := maxInt(12, m.height-13)
	m.logViewport.Width = rightWidth - 4
	m.logViewport.Height = logHeight - 2
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

func receiveEvents(stream pb.OperatorService_ConnectClient, ch chan<- tea.Msg) {
	defer close(ch)
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			ch <- operatorDisconnectedMsg{}
			return
		}
		ch <- operatorEventMsg{event: event}
	}
}

func formatEvent(event *pb.OperatorEvent) string {
	switch event.GetType() {
	case "agent_joined":
		return fmt.Sprintf("[+] agent:%s  %s", event.GetAgentId(), event.GetPayload())
	case "agent_dead":
		return fmt.Sprintf("[!] agent:%s  %s", event.GetAgentId(), event.GetPayload())
	case "output":
		return fmt.Sprintf("[>] agent:%s  %s", event.GetAgentId(), event.GetPayload())
	case "ack":
		return fmt.Sprintf("[~] agent:%s  %s", event.GetAgentId(), event.GetPayload())
	case "error":
		return fmt.Sprintf("[x] agent:%s  %s", event.GetAgentId(), event.GetPayload())
	default:
		return fmt.Sprintf("[?] %s agent:%s  %s", event.GetType(), event.GetAgentId(), event.GetPayload())
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
