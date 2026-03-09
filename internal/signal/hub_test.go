package signal_test

import (
	"testing"

	"github.com/chris1004tw/remote-adb/internal/signal"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
)

// mockConn 建立一個用於測試的 Conn（需要真實的 websocket.Conn，所以這裡測 Hub 的邏輯）。
// Hub 的 Register/Unregister/Agents 不需要實際 WebSocket。

func TestHub_RegisterAgent_AppearsInList(t *testing.T) {
	hub := signal.NewHub()

	info := protocol.HostInfo{
		HostID:   "agent-001",
		Hostname: "lab-pc-01",
		Devices: []protocol.DeviceInfo{
			{Serial: "DEV001", State: protocol.DeviceStateOnline, Lock: protocol.LockAvailable},
		},
	}
	hub.RegisterAgent("agent-001", info)

	agents := hub.Agents()
	if len(agents) != 1 {
		t.Fatalf("Agents 數量 = %d, 預期 1", len(agents))
	}
	if agents[0].HostID != "agent-001" {
		t.Errorf("HostID = %q, 預期 %q", agents[0].HostID, "agent-001")
	}
	if agents[0].Hostname != "lab-pc-01" {
		t.Errorf("Hostname = %q, 預期 %q", agents[0].Hostname, "lab-pc-01")
	}
	if len(agents[0].Devices) != 1 {
		t.Errorf("Devices 數量 = %d, 預期 1", len(agents[0].Devices))
	}
}

func TestHub_Unregister_RemovesAgent(t *testing.T) {
	hub := signal.NewHub()

	info := protocol.HostInfo{HostID: "agent-001", Hostname: "lab-pc"}
	hub.RegisterAgent("agent-001", info)

	if len(hub.Agents()) != 1 {
		t.Fatal("註冊後應有 1 個 Agent")
	}

	hub.Unregister("agent-001")

	if len(hub.Agents()) != 0 {
		t.Error("移除後 Agent 列表應為空")
	}
}

func TestHub_UpdateAgentDevices(t *testing.T) {
	hub := signal.NewHub()

	info := protocol.HostInfo{
		HostID:   "agent-001",
		Hostname: "lab-pc",
		Devices:  []protocol.DeviceInfo{},
	}
	hub.RegisterAgent("agent-001", info)

	// 更新設備列表
	newDevices := []protocol.DeviceInfo{
		{Serial: "AAA", State: protocol.DeviceStateOnline, Lock: protocol.LockAvailable},
		{Serial: "BBB", State: protocol.DeviceStateOnline, Lock: protocol.LockLocked, LockedBy: "client-1"},
	}
	hub.UpdateAgentDevices("agent-001", newDevices)

	agents := hub.Agents()
	if len(agents[0].Devices) != 2 {
		t.Errorf("更新後 Devices 數量 = %d, 預期 2", len(agents[0].Devices))
	}
	if agents[0].Devices[1].LockedBy != "client-1" {
		t.Errorf("LockedBy = %q, 預期 %q", agents[0].Devices[1].LockedBy, "client-1")
	}
}

func TestHub_ConnCount(t *testing.T) {
	hub := signal.NewHub()
	if hub.ConnCount() != 0 {
		t.Errorf("初始 ConnCount = %d, 預期 0", hub.ConnCount())
	}
}

func TestHub_Route_NoTargetReturnsFalse(t *testing.T) {
	hub := signal.NewHub()
	env := protocol.Envelope{TargetID: ""}
	if hub.Route(env) {
		t.Error("空 TargetID 應回傳 false")
	}
}

func TestHub_Route_UnknownTargetReturnsFalse(t *testing.T) {
	hub := signal.NewHub()
	env := protocol.Envelope{TargetID: "nonexistent"}
	if hub.Route(env) {
		t.Error("不存在的目標應回傳 false")
	}
}

func TestHub_MultipleAgents(t *testing.T) {
	hub := signal.NewHub()

	hub.RegisterAgent("agent-1", protocol.HostInfo{HostID: "agent-1", Hostname: "pc-1"})
	hub.RegisterAgent("agent-2", protocol.HostInfo{HostID: "agent-2", Hostname: "pc-2"})

	agents := hub.Agents()
	if len(agents) != 2 {
		t.Errorf("Agents 數量 = %d, 預期 2", len(agents))
	}
}
