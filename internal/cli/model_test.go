package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chris1004tw/remote-adb/internal/cli"
	"github.com/chris1004tw/remote-adb/internal/daemon"
)

// mockSender 建立測試用的 IPC 發送函式。
func mockSender(hosts []cli.HostData) cli.IPCSender {
	return func(cmd daemon.IPCCommand) daemon.IPCResponse {
		switch cmd.Action {
		case "hosts":
			return daemon.SuccessResponse(hosts)
		case "bind":
			return daemon.SuccessResponse(daemon.BindResult{
				LocalPort: 15555,
				Serial:    "test-device",
			})
		default:
			return daemon.ErrorResponse("unknown")
		}
	}
}

func testHosts() []cli.HostData {
	return []cli.HostData{
		{
			HostID:   "agent-1",
			Hostname: "lab-server",
			Devices: []cli.DeviceData{
				{Serial: "device-a", State: "device", Lock: "available"},
				{Serial: "device-b", State: "device", Lock: "locked"},
			},
		},
		{
			HostID:   "agent-2",
			Hostname: "office-pc",
			Devices: []cli.DeviceData{
				{Serial: "device-c", State: "device", Lock: "available"},
			},
		},
	}
}

func TestModel_InitialPhase(t *testing.T) {
	m := cli.NewModel(mockSender(nil))
	view := m.View()
	if !strings.Contains(view, "載入") {
		t.Errorf("初始畫面應顯示載入中，實際: %s", view)
	}
}

func TestModel_HostsLoaded(t *testing.T) {
	hosts := testHosts()
	m := cli.NewModel(mockSender(hosts))

	// 模擬 hostsLoadedMsg：直接透過 Init() 取得 cmd 並執行
	cmd := m.Init()
	msg := cmd()

	updated, _ := m.Update(msg)
	m = updated.(cli.Model)

	view := m.View()
	if !strings.Contains(view, "lab-server") {
		t.Errorf("應顯示主機名稱 lab-server，實際: %s", view)
	}
	if !strings.Contains(view, "office-pc") {
		t.Errorf("應顯示主機名稱 office-pc，實際: %s", view)
	}
}

func TestModel_SelectHost_EnterShowsDevices(t *testing.T) {
	hosts := testHosts()
	m := cli.NewModel(mockSender(hosts))

	// 載入主機
	cmd := m.Init()
	updated, _ := m.Update(cmd())
	m = updated.(cli.Model)

	// 按 Enter 選擇第一台主機
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(cli.Model)

	view := m.View()
	if !strings.Contains(view, "device-a") {
		t.Errorf("應顯示設備 device-a，實際: %s", view)
	}
	if !strings.Contains(view, "已鎖定") {
		t.Errorf("device-b 應顯示已鎖定，實際: %s", view)
	}
}

func TestModel_NavigateDown(t *testing.T) {
	hosts := testHosts()
	m := cli.NewModel(mockSender(hosts))

	// 載入主機
	cmd := m.Init()
	updated, _ := m.Update(cmd())
	m = updated.(cli.Model)

	// 按下方向鍵
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(cli.Model)

	// 按 Enter 應選擇第二台主機 (office-pc)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(cli.Model)

	view := m.View()
	if !strings.Contains(view, "device-c") {
		t.Errorf("應顯示 office-pc 的設備 device-c，實際: %s", view)
	}
}

func TestModel_EscGoesBack(t *testing.T) {
	hosts := testHosts()
	m := cli.NewModel(mockSender(hosts))

	// 載入主機 → 選擇主機 → Esc 回到主機列表
	cmd := m.Init()
	updated, _ := m.Update(cmd())
	m = updated.(cli.Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(cli.Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(cli.Model)

	view := m.View()
	if !strings.Contains(view, "選擇主機") {
		t.Errorf("Esc 後應回到主機選擇，實際: %s", view)
	}
}

func TestModel_LockedDeviceNotSelectable(t *testing.T) {
	hosts := testHosts()
	m := cli.NewModel(mockSender(hosts))

	// 載入 → 選主機
	cmd := m.Init()
	updated, _ := m.Update(cmd())
	m = updated.(cli.Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(cli.Model)

	// 移到 device-b（已鎖定）
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(cli.Model)

	// 按 Enter：不應進入綁定階段
	updated, retCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(cli.Model)

	if retCmd != nil {
		t.Error("已鎖定設備不應觸發綁定指令")
	}

	// 仍應在設備選擇畫面
	view := m.View()
	if !strings.Contains(view, "device-a") {
		t.Errorf("應仍在設備選擇畫面，實際: %s", view)
	}
}

func TestModel_EmptyHosts(t *testing.T) {
	m := cli.NewModel(mockSender(nil))

	// 模擬空主機列表載入
	hostsJSON, _ := json.Marshal([]cli.HostData{})
	updated, _ := m.Update(struct {
		hosts []cli.HostData
	}{})
	_ = updated

	// 直接測試 Init cmd
	cmd := m.Init()
	msg := cmd()
	updated, _ = m.Update(msg)
	m = updated.(cli.Model)

	view := m.View()
	if !strings.Contains(view, "沒有可用") {
		t.Errorf("空主機時應顯示提示，實際: %s", view)
	}
	_ = hostsJSON
}

func TestModel_QuitOnQ(t *testing.T) {
	m := cli.NewModel(mockSender(nil))

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	if cmd == nil {
		t.Error("按 q 應回傳 Quit 指令")
	}
}
