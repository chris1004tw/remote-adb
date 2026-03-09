// Package cli 實作 radb 的互動式命令列介面。
package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chris1004tw/remote-adb/internal/daemon"
)

// Phase 代表互動流程的階段。
type Phase int

const (
	PhaseLoading      Phase = iota // 載入主機列表中
	PhaseSelectHost                // 選擇主機
	PhaseSelectDevice              // 選擇設備
	PhaseBinding                   // 綁定進行中
	PhaseResult                    // 顯示結果
)

// HostData 代表主機及其設備。
type HostData struct {
	HostID   string
	Hostname string
	Devices  []DeviceData
}

// DeviceData 代表設備資訊。
type DeviceData struct {
	Serial string
	State  string
	Lock   string
}

// IPCSender 是 IPC 指令發送函式的型別。
type IPCSender func(daemon.IPCCommand) daemon.IPCResponse

// Model 是 bubbletea 的主模型。
type Model struct {
	phase        Phase
	hosts        []HostData
	cursor       int
	selectedHost int // 選中的主機索引

	result   string
	err      string
	quitting bool

	sendIPC IPCSender
}

// NewModel 建立互動式 bind 流程的模型。
func NewModel(sendIPC IPCSender) Model {
	return Model{
		phase:   PhaseLoading,
		sendIPC: sendIPC,
	}
}

// --- Tea Messages ---

type hostsLoadedMsg struct {
	hosts []HostData
}

type bindResultMsg struct {
	port   int
	serial string
}

type errMsg struct {
	err string
}

// Init 初始化模型，載入主機列表。
func (m Model) Init() tea.Cmd {
	return m.loadHosts
}

func (m Model) loadHosts() tea.Msg {
	resp := m.sendIPC(daemon.IPCCommand{Action: "hosts"})
	if !resp.Success {
		return errMsg{err: resp.Error}
	}

	var rawHosts []struct {
		HostID   string `json:"host_id"`
		Hostname string `json:"hostname"`
		Devices  []struct {
			Serial string `json:"serial"`
			State  string `json:"state"`
			Lock   string `json:"lock"`
		} `json:"devices"`
	}
	json.Unmarshal(resp.Data, &rawHosts)

	hosts := make([]HostData, 0, len(rawHosts))
	for _, h := range rawHosts {
		hd := HostData{HostID: h.HostID, Hostname: h.Hostname}
		for _, d := range h.Devices {
			hd.Devices = append(hd.Devices, DeviceData{
				Serial: d.Serial,
				State:  d.State,
				Lock:   d.Lock,
			})
		}
		hosts = append(hosts, hd)
	}

	return hostsLoadedMsg{hosts: hosts}
}

// Update 處理訊息並更新模型狀態。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case hostsLoadedMsg:
		m.hosts = msg.hosts
		m.phase = PhaseSelectHost
		m.cursor = 0
	case bindResultMsg:
		m.phase = PhaseResult
		m.result = fmt.Sprintf(
			"綁定成功！\n本機 Port: %d\n設備: %s\n\n使用方式: adb -s 127.0.0.1:%d shell",
			msg.port, msg.serial, msg.port,
		)
	case errMsg:
		m.phase = PhaseResult
		m.err = msg.err
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		max := m.maxCursor()
		if m.cursor < max {
			m.cursor++
		}
	case "enter":
		return m.handleEnter()
	case "esc":
		return m.handleEsc()
	}
	return m, nil
}

func (m Model) maxCursor() int {
	switch m.phase {
	case PhaseSelectHost:
		if len(m.hosts) == 0 {
			return 0
		}
		return len(m.hosts) - 1
	case PhaseSelectDevice:
		if m.selectedHost < 0 || m.selectedHost >= len(m.hosts) {
			return 0
		}
		devices := m.hosts[m.selectedHost].Devices
		if len(devices) == 0 {
			return 0
		}
		return len(devices) - 1
	default:
		return 0
	}
}

func (m Model) handleEnter() (tea.Model, tea.Cmd) {
	switch m.phase {
	case PhaseSelectHost:
		if len(m.hosts) == 0 {
			return m, nil
		}
		m.selectedHost = m.cursor
		m.phase = PhaseSelectDevice
		m.cursor = 0
		return m, nil

	case PhaseSelectDevice:
		host := m.hosts[m.selectedHost]
		if m.cursor >= len(host.Devices) {
			return m, nil
		}
		device := host.Devices[m.cursor]
		if device.Lock == "locked" {
			return m, nil // 已鎖定的設備不可選
		}
		m.phase = PhaseBinding
		return m, m.bindDevice(host.HostID, device.Serial)

	case PhaseResult:
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) handleEsc() (tea.Model, tea.Cmd) {
	switch m.phase {
	case PhaseSelectDevice:
		m.phase = PhaseSelectHost
		m.selectedHost = 0
		m.cursor = 0
	default:
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) bindDevice(hostID, serial string) tea.Cmd {
	return func() tea.Msg {
		payload, _ := json.Marshal(daemon.BindRequest{HostID: hostID, Serial: serial})
		resp := m.sendIPC(daemon.IPCCommand{Action: "bind", Payload: payload})
		if !resp.Success {
			return errMsg{err: resp.Error}
		}

		var result daemon.BindResult
		json.Unmarshal(resp.Data, &result)
		return bindResultMsg{port: result.LocalPort, serial: result.Serial}
	}
}

// --- 樣式 ---

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	cursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	selectedStyle = lipgloss.NewStyle().Bold(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	successStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	helpStyle     = lipgloss.NewStyle().Faint(true)
)

// View 渲染目前的畫面。
func (m Model) View() string {
	if m.quitting {
		return ""
	}

	switch m.phase {
	case PhaseLoading:
		return titleStyle.Render("radb bind") + "\n\n載入主機列表中...\n"
	case PhaseSelectHost:
		return m.viewSelectHost()
	case PhaseSelectDevice:
		return m.viewSelectDevice()
	case PhaseBinding:
		return titleStyle.Render("radb bind") + "\n\n綁定中，請稍候...\n"
	case PhaseResult:
		return m.viewResult()
	}
	return ""
}

func (m Model) viewSelectHost() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("radb bind — 選擇主機"))
	b.WriteString("\n\n")

	if len(m.hosts) == 0 {
		b.WriteString(dimStyle.Render("目前沒有可用的主機"))
		b.WriteString("\n")
	} else {
		for i, h := range m.hosts {
			deviceCount := len(h.Devices)
			label := fmt.Sprintf("%s (%d 個設備)", h.Hostname, deviceCount)

			if i == m.cursor {
				b.WriteString(cursorStyle.Render("> "))
				b.WriteString(selectedStyle.Render(label))
			} else {
				b.WriteString("  ")
				b.WriteString(label)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓ 選擇 • Enter 確認 • q 離開"))
	return b.String()
}

func (m Model) viewSelectDevice() string {
	var b strings.Builder
	host := m.hosts[m.selectedHost]

	b.WriteString(titleStyle.Render(fmt.Sprintf("radb bind — %s 的設備", host.Hostname)))
	b.WriteString("\n\n")

	if len(host.Devices) == 0 {
		b.WriteString(dimStyle.Render("此主機沒有設備"))
		b.WriteString("\n")
	} else {
		for i, d := range host.Devices {
			stateTag := fmt.Sprintf("[%s]", d.State)
			lockTag := ""
			if d.Lock == "locked" {
				lockTag = " (已鎖定)"
			}

			label := fmt.Sprintf("%s %s%s", d.Serial, stateTag, lockTag)

			if d.Lock == "locked" {
				// 已鎖定設備：淡色顯示
				if i == m.cursor {
					b.WriteString(cursorStyle.Render("> "))
				} else {
					b.WriteString("  ")
				}
				b.WriteString(dimStyle.Render(label))
			} else if i == m.cursor {
				b.WriteString(cursorStyle.Render("> "))
				b.WriteString(selectedStyle.Render(label))
			} else {
				b.WriteString("  ")
				b.WriteString(label)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓ 選擇 • Enter 綁定 • Esc 返回 • q 離開"))
	return b.String()
}

func (m Model) viewResult() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("radb bind"))
	b.WriteString("\n\n")

	if m.err != "" {
		b.WriteString(errorStyle.Render("錯誤: " + m.err))
	} else {
		b.WriteString(successStyle.Render(m.result))
	}

	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("Enter 離開"))
	return b.String()
}
