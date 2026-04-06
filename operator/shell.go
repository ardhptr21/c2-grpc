package main

import (
	"fmt"
	"strings"

	pb "github.com/ardhptr21/c2-grpc/pb"
	tea "github.com/charmbracelet/bubbletea"
)

func (m *operatorModel) applyShellEvent(event *pb.AgentShellEvent) {
	switch event.GetType() {
	case "open_ok":
		m.shellSessionID = event.GetSessionId()
		m.shellAgentID = event.GetAgentId()
		m.shellReady = true
		m.shellOutput = ""
		m.shellInputBuffer = ""
		m.shellPendingEcho = ""
		m.shellSkipEchoNewline = false
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
		m.shellPendingEcho = ""
		m.shellSkipEchoNewline = false
		m.status = "shell closed"
		if event.GetMessage() != "" {
			m.status = "shell closed: " + event.GetMessage()
		}
	}
}

func (m *operatorModel) activateShellTab() tea.Cmd {
	if m.shellStream == nil {
		m.status = "Shell service unavailable."
		return nil
	}
	if len(m.agentOrder) == 0 {
		m.status = "No agent selected."
		return nil
	}

	agentID := m.agentOrder[m.selected]
	agent, ok := m.agents[agentID]
	if !ok || agent.Status == "OFFLINE" || agent.Status == "DEAD" {
		m.status = "Shell tab requires a live selected agent."
		return nil
	}
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
			m.shellOutput = ""
			m.shellInputBuffer = ""
			m.shellPendingEcho = ""
			m.shellSkipEchoNewline = false
			m.refreshShellViewport()
			m.status = "Shell pane cleared."
			return
		}
		if m.shellInputBuffer != "" {
			m.shellOutput += m.shellInputBuffer + "\n"
		}
		m.shellPendingEcho = m.shellInputBuffer
		m.shellSkipEchoNewline = false
		m.sendShellInput(m.shellInputBuffer + "\r")
		m.shellInputBuffer = ""
		m.refreshShellViewport()
		return
	case "backspace":
		if len(m.shellInputBuffer) > 0 {
			runes := []rune(m.shellInputBuffer)
			m.shellInputBuffer = string(runes[:len(runes)-1])
		}
		m.refreshShellViewport()
		return
	case "delete":
		if len(m.shellInputBuffer) > 0 {
			runes := []rune(m.shellInputBuffer)
			m.shellInputBuffer = string(runes[:len(runes)-1])
		}
		m.refreshShellViewport()
		return
	case "ctrl+u":
		m.shellInputBuffer = ""
		m.refreshShellViewport()
		return
	case "ctrl+w":
		m.shellInputBuffer = trimLastShellWord(m.shellInputBuffer)
		m.refreshShellViewport()
		return
	case "ctrl+l":
		m.shellOutput = ""
		m.refreshShellViewport()
		m.shellInputBuffer = ""
		m.shellPendingEcho = ""
		m.shellSkipEchoNewline = false
		m.status = "Shell pane cleared."
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
	case "tab":
		m.shellInputBuffer += "\t"
		m.refreshShellViewport()
		return
	}

	if len(msg.Runes) > 0 {
		m.shellInputBuffer += string(msg.Runes)
		m.refreshShellViewport()
		return
	}
	if input, ok := shellInputForKey(msg); ok {
		m.sendShellInput(input)
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
