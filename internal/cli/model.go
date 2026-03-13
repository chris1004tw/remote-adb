// Package cli 實作 radb 的互動式命令列介面。
//
// 本套件使用 charmbracelet/bubbletea 框架，採用 Elm Architecture（TEA）模式：
//   - Model：儲存應用程式的完整狀態（目前階段、游標位置、選取項目等）
//   - Update：接收訊息（鍵盤輸入、非同步結果）並回傳更新後的 Model
//   - View：根據 Model 狀態渲染終端機畫面（純函式，無副作用）
//
// 互動流程為一個五階段狀態機：
//
//	PhaseLoading → PhaseSelectHost → PhaseSelectDevice → PhaseBinding → PhaseResult
//	                     ↑                  │
//	                     └──── Esc 返回 ─────┘
//
// 使用者透過方向鍵選擇主機與設備，最終發送 IPC 指令給 Daemon 執行 ADB 綁定。
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
//
// 狀態轉換流程：
//   - PhaseLoading：啟動時自動透過 IPC 向 Daemon 請求主機列表
//   - PhaseSelectHost：顯示主機清單，使用者上下選擇後按 Enter 進入設備選擇
//   - PhaseSelectDevice：顯示所選主機的設備清單，按 Enter 發起綁定，按 Esc 返回主機選擇
//   - PhaseBinding：綁定進行中（非同步等待 Daemon 回應），畫面顯示等待提示
//   - PhaseResult：顯示綁定成功（含 Port 資訊）或失敗訊息，按 Enter 離開
type Phase int

const (
	PhaseLoading      Phase = iota // 載入主機列表中
	PhaseSelectHost                // 選擇主機
	PhaseSelectDevice              // 選擇設備
	PhaseBinding                   // 綁定進行中
	PhaseResult                    // 顯示結果
)

// HostData 代表一台遠端主機及其掛載的 Android 設備清單。
// 由 Daemon 的 "hosts" IPC 指令回傳的資料轉換而來。
type HostData struct {
	HostID   string       // 主機唯一識別碼（由 Signal Server 分配）
	Hostname string       // 主機顯示名稱
	Devices  []DeviceData // 該主機上的所有 Android 設備
}

// DeviceData 代表單一 Android 設備的資訊。
type DeviceData struct {
	Serial string // ADB 設備序號（如 "emulator-5554" 或 USB 序號）
	State  string // ADB 設備狀態（"device"、"offline" 等）
	Lock   string // 鎖定狀態：空字串表示可用，"locked" 表示已被其他使用者綁定
}

// IPCSender 是 IPC 指令發送函式的型別。
//
// 透過依賴注入（Dependency Injection）的方式傳入，讓 Model 不直接依賴具體的
// IPC 實作（如 TCP/Unix Socket），方便單元測試時替換為 mock 函式。
type IPCSender func(daemon.IPCCommand) daemon.IPCResponse

// Model 是 bubbletea 的主模型，儲存整個互動式 bind 流程的完整狀態。
//
// bubbletea 採用不可變模型設計：Update 方法接收舊 Model 的副本，修改後回傳新的 Model，
// 框架再以新 Model 呼叫 View 重新渲染畫面。因此所有欄位皆為值型別或 slice。
type Model struct {
	phase        Phase      // 目前所在的互動階段
	hosts        []HostData // 從 Daemon 載入的主機列表
	cursor       int        // 目前游標位置（在主機或設備清單中的索引）
	selectedHost int        // 使用者在 PhaseSelectHost 階段選中的主機索引

	result   string // 綁定成功時的結果訊息（含 Port、設備序號、使用方式）
	err      string // 錯誤訊息（IPC 失敗或綁定失敗時設定）
	quitting bool   // 是否正在離開程式（設為 true 時 View 回傳空字串以清屏）

	sendIPC IPCSender // IPC 發送函式，注入的外部依賴
}

// NewModel 建立互動式 bind 流程的模型。
//
// sendIPC 為必要的依賴，用於與背景執行的 Daemon 行程通訊。
// 初始階段設為 PhaseLoading，Init() 會自動觸發主機列表載入。
func NewModel(sendIPC IPCSender) Model {
	return Model{
		phase:   PhaseLoading,
		sendIPC: sendIPC,
	}
}

// --- Tea Messages ---
// 以下為 bubbletea 的訊息型別，用於在非同步操作完成後通知 Update 更新狀態。
// bubbletea 的 Cmd 回傳函式會在背景 goroutine 執行，完成後產生對應的 Msg。

// hostsLoadedMsg 表示主機列表載入完成，攜帶解析後的主機資料。
type hostsLoadedMsg struct {
	hosts []HostData
}

// bindResultMsg 表示設備綁定成功，攜帶本機分配的 Port 和設備序號。
type bindResultMsg struct {
	port   int
	serial string
}

// errMsg 表示操作失敗，攜帶錯誤描述。
type errMsg struct {
	err string
}

// Init 是 bubbletea Model 介面的初始化方法。
// 回傳 loadHosts 作為啟動時的第一個 Cmd，在背景載入主機列表。
func (m Model) Init() tea.Cmd {
	return m.loadHosts
}

// loadHosts 透過 IPC 向 Daemon 發送 "hosts" 指令，取得所有已連線主機及其設備清單。
// 此函式作為 tea.Cmd 在背景 goroutine 執行，回傳 hostsLoadedMsg 或 errMsg。
func (m Model) loadHosts() tea.Msg {
	resp := m.sendIPC(daemon.IPCCommand{Action: "hosts"})
	if !resp.Success {
		return errMsg{err: resp.Error}
	}

	// 將 IPC 回傳的 JSON 資料反序列化為內部結構
	var rawHosts []struct {
		HostID   string `json:"host_id"`
		Hostname string `json:"hostname"`
		Devices  []struct {
			Serial string `json:"serial"`
			State  string `json:"state"`
			Lock   string `json:"lock"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(resp.Data, &rawHosts); err != nil {
		return errMsg{err: fmt.Sprintf("解碼主機清單失敗: %v", err)}
	}

	// 轉換為 CLI 層的 HostData/DeviceData，與 daemon 層的 JSON 結構解耦
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

// Update 是 bubbletea Model 介面的核心方法，處理所有訊息並更新模型狀態。
//
// 訊息來源有兩種：
//   - tea.KeyMsg：使用者的鍵盤輸入，委派給 handleKey 處理
//   - 自訂 Msg（hostsLoadedMsg、bindResultMsg、errMsg）：非同步操作的結果回報
//
// 回傳值為更新後的 Model 與下一個要執行的 Cmd（nil 表示無後續操作）。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case hostsLoadedMsg:
		// 主機列表載入完成，切換到主機選擇階段
		m.hosts = msg.hosts
		m.phase = PhaseSelectHost
		m.cursor = 0
	case bindResultMsg:
		// 綁定成功，切換到結果顯示階段，組裝使用者友善的提示訊息
		m.phase = PhaseResult
		m.result = fmt.Sprintf(
			"綁定成功！\n本機 Port: %d\n設備: %s\n\n使用方式: adb -s 127.0.0.1:%d shell",
			msg.port, msg.serial, msg.port,
		)
	case errMsg:
		// 任何階段的錯誤都直接跳到結果頁顯示
		m.phase = PhaseResult
		m.err = msg.err
	}
	return m, nil
}

// handleKey 處理鍵盤輸入，根據目前階段執行對應的導航或操作邏輯。
//
// 按鍵對應：
//   - q / Ctrl+C：任何階段皆可直接離開程式
//   - ↑/k、↓/j：在清單中移動游標（受 maxCursor 限制不超出範圍）
//   - Enter：確認選擇（選主機→進設備頁、選設備→發起綁定、結果頁→離開）
//   - Esc：返回上一層（設備選擇→返回主機選擇，其餘階段→離開）
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

// maxCursor 回傳目前階段游標的最大允許值（清單長度 - 1）。
// 用於防止游標超出清單範圍。不同階段對應不同的清單（主機或設備）。
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

// handleEnter 處理 Enter 鍵，依據目前階段執行不同的確認動作。
//
//   - PhaseSelectHost：記錄選中的主機索引，切換到 PhaseSelectDevice，游標歸零
//   - PhaseSelectDevice：檢查設備是否被鎖定（locked），未鎖定才發起綁定；
//     已鎖定的設備按 Enter 無反應，避免使用者誤操作搶佔他人設備
//   - PhaseResult：按 Enter 離開程式
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
		// 已鎖定的設備不可選擇：lock 欄位由 Agent 端設定，
		// 表示該設備已被其他 Client 綁定，需等待釋放後才能使用
		if device.Lock == "locked" {
			return m, nil
		}
		m.phase = PhaseBinding
		return m, m.bindDevice(host.HostID, device.Serial)

	case PhaseResult:
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

// handleEsc 處理 Esc 鍵的返回邏輯。
//
//   - PhaseSelectDevice：返回 PhaseSelectHost（重設游標與選中主機索引）
//   - 其他階段：直接離開程式（PhaseSelectHost 按 Esc 等同於放棄操作）
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

// bindDevice 建立一個 tea.Cmd，在背景透過 IPC 向 Daemon 發送 "bind" 指令。
//
// 綁定流程：Client → IPC "bind" → Daemon → Signal Server → Agent → ADB 設備
// 成功後 Daemon 會分配一個本機 TCP Port，Client 可透過該 Port 使用 ADB。
func (m Model) bindDevice(hostID, serial string) tea.Cmd {
	return func() tea.Msg {
		payload, _ := json.Marshal(daemon.BindRequest{HostID: hostID, Serial: serial})
		resp := m.sendIPC(daemon.IPCCommand{Action: "bind", Payload: payload})
		if !resp.Success {
			return errMsg{err: resp.Error}
		}

		var result daemon.BindResult
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			return errMsg{err: fmt.Sprintf("解碼綁定結果失敗: %v", err)}
		}
		return bindResultMsg{port: result.LocalPort, serial: result.Serial}
	}
}

// --- 樣式 ---
// 以下使用 lipgloss 定義終端機文字樣式，統一管理 CLI 的視覺呈現。
// lipgloss 會自動偵測終端機的色彩能力並降級處理。

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))  // 標題：藍色粗體
	cursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))             // 游標指示符 ">"：綠色
	selectedStyle = lipgloss.NewStyle().Bold(true)                                   // 選中項目：粗體
	dimStyle      = lipgloss.NewStyle().Faint(true)                                  // 淡化文字：用於鎖定設備與空狀態提示
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))              // 錯誤訊息：紅色
	successStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))             // 成功訊息：綠色
	helpStyle     = lipgloss.NewStyle().Faint(true)                                  // 底部操作提示：淡化
)

// View 是 bubbletea Model 介面的渲染方法，根據目前階段回傳對應的終端機畫面字串。
//
// 此方法為純函式（無副作用），bubbletea 框架會在每次 Update 後自動呼叫 View 重繪畫面。
// 當 quitting 為 true 時回傳空字串，讓 bubbletea 清除畫面後退出。
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

// viewSelectHost 渲染主機選擇頁面。
// 顯示所有主機名稱及其設備數量，游標所在項目以綠色 ">" 標示並加粗。
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

// viewSelectDevice 渲染設備選擇頁面。
// 已鎖定的設備以淡色顯示並標註「(已鎖定)」，提示使用者該設備不可選擇。
// 未鎖定的設備在游標選中時以粗體高亮顯示。
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
				// 已鎖定設備：無論游標是否在此項目，文字一律淡色顯示
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

// viewResult 渲染結果頁面。
// 根據 err 是否為空決定顯示錯誤訊息（紅色）或成功訊息（綠色）。
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
