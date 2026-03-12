package bridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// mockOpenCh 建立一個 mock OpenChannelFunc，重用 bridge 套件內的 nopRWC。
func mockOpenCh() OpenChannelFunc {
	return func(label string) (io.ReadWriteCloser, error) {
		return nopRWC{}, nil
	}
}

// freePort 取得一個可用的 port（用於測試中的 portStart）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// TestDeviceProxyManager_AddDevice 新增一台設備，驗證 Entries 回傳正確的 serial + port。
func TestDeviceProxyManager_AddDevice(t *testing.T) {
	port := freePort(t)
	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
	})
	defer dpm.Close()

	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "ABC123", State: "device"},
	})

	entries := dpm.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Serial != "ABC123" {
		t.Errorf("expected serial ABC123, got %s", entries[0].Serial)
	}
	if entries[0].Port < port {
		t.Errorf("expected port >= %d, got %d", port, entries[0].Port)
	}
}

// TestDeviceProxyManager_RemoveDevice 新增後移除設備，驗證 Entries 為空且 port 被釋放。
func TestDeviceProxyManager_RemoveDevice(t *testing.T) {
	port := freePort(t)
	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
	})
	defer dpm.Close()

	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "ABC123", State: "device"},
	})
	if len(dpm.Entries()) != 1 {
		t.Fatalf("expected 1 entry after add")
	}

	// 移除設備（傳空清單）
	dpm.UpdateDevices(nil)

	entries := dpm.Entries()
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries after remove, got %d", len(entries))
	}
}

// TestDeviceProxyManager_MultipleDevices 新增多台設備，驗證每台有不同 port。
func TestDeviceProxyManager_MultipleDevices(t *testing.T) {
	port := freePort(t)
	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
	})
	defer dpm.Close()

	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "DEV1", State: "device"},
		{Serial: "DEV2", State: "device"},
		{Serial: "DEV3", State: "device"},
	})

	entries := dpm.Entries()
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// 驗證 port 都不同
	ports := make(map[int]string)
	for _, e := range entries {
		if prev, ok := ports[e.Port]; ok {
			t.Errorf("port %d used by both %s and %s", e.Port, prev, e.Serial)
		}
		ports[e.Port] = e.Serial
	}
}

// TestDeviceProxyManager_DiffUpdate 測試 diff 更新：[A,B] → [B,C]，A 被移除、B 保留、C 新增。
func TestDeviceProxyManager_DiffUpdate(t *testing.T) {
	port := freePort(t)
	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
	})
	defer dpm.Close()

	// 第一次更新：A + B
	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "A", State: "device"},
		{Serial: "B", State: "device"},
	})

	entries1 := dpm.Entries()
	if len(entries1) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries1))
	}

	// 記錄 B 的 port
	var bPort int
	for _, e := range entries1 {
		if e.Serial == "B" {
			bPort = e.Port
		}
	}

	// 第二次更新：B + C（A 消失，C 新增）
	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "B", State: "device"},
		{Serial: "C", State: "device"},
	})

	entries2 := dpm.Entries()
	if len(entries2) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries2))
	}

	serials := make(map[string]int)
	for _, e := range entries2 {
		serials[e.Serial] = e.Port
	}

	if _, ok := serials["A"]; ok {
		t.Error("A should have been removed")
	}
	if _, ok := serials["C"]; !ok {
		t.Error("C should have been added")
	}
	if p, ok := serials["B"]; !ok {
		t.Error("B should still exist")
	} else if p != bPort {
		t.Errorf("B's port should be preserved: expected %d, got %d", bPort, p)
	}
}

// TestDeviceProxyManager_OnReadyCallback 驗證新增設備時 OnReady callback 被呼叫。
func TestDeviceProxyManager_OnReadyCallback(t *testing.T) {
	port := freePort(t)

	var mu sync.Mutex
	var readyEvents []DeviceEntry

	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
		OnReady: func(serial string, port int) {
			mu.Lock()
			readyEvents = append(readyEvents, DeviceEntry{Serial: serial, Port: port})
			mu.Unlock()
		},
	})
	defer dpm.Close()

	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "DEV1", State: "device"},
		{Serial: "DEV2", State: "device"},
	})

	mu.Lock()
	defer mu.Unlock()
	if len(readyEvents) != 2 {
		t.Fatalf("expected 2 ready events, got %d", len(readyEvents))
	}
}

// TestDeviceProxyManager_OnRemovedCallback 驗證移除設備時 OnRemoved callback 被呼叫。
func TestDeviceProxyManager_OnRemovedCallback(t *testing.T) {
	port := freePort(t)

	var mu sync.Mutex
	var removedEvents []DeviceEntry

	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
		OnRemoved: func(serial string, port int) {
			mu.Lock()
			removedEvents = append(removedEvents, DeviceEntry{Serial: serial, Port: port})
			mu.Unlock()
		},
	})
	defer dpm.Close()

	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "DEV1", State: "device"},
	})

	// 移除設備
	dpm.UpdateDevices(nil)

	mu.Lock()
	defer mu.Unlock()
	if len(removedEvents) != 1 {
		t.Fatalf("expected 1 removed event, got %d", len(removedEvents))
	}
	if removedEvents[0].Serial != "DEV1" {
		t.Errorf("expected serial DEV1, got %s", removedEvents[0].Serial)
	}
}

// TestDeviceProxyManager_Close 驗證 Close 後所有 listener 關閉、Entries 為空。
func TestDeviceProxyManager_Close(t *testing.T) {
	port := freePort(t)
	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
	})

	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "DEV1", State: "device"},
		{Serial: "DEV2", State: "device"},
	})

	// 取得分配的 port
	entries := dpm.Entries()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	dpm.Close()

	// Close 後 Entries 應為空
	if len(dpm.Entries()) != 0 {
		t.Errorf("expected 0 entries after Close, got %d", len(dpm.Entries()))
	}

	// Close 後 listener 應已關閉（port 應可再用）
	for _, e := range entries {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", e.Port))
		if err != nil {
			t.Errorf("port %d should be free after Close, but got: %v", e.Port, err)
		} else {
			ln.Close()
		}
	}
}

// TestDeviceProxyManager_SkipOfflineDevices 驗證 State 不是 "device" 的設備不分配 proxy。
func TestDeviceProxyManager_SkipOfflineDevices(t *testing.T) {
	port := freePort(t)
	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
	})
	defer dpm.Close()

	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "ONLINE", State: "device"},
		{Serial: "OFFLINE", State: "offline"},
		{Serial: "NODEV", State: "no device"},
	})

	entries := dpm.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (only online device), got %d", len(entries))
	}
	if entries[0].Serial != "ONLINE" {
		t.Errorf("expected serial ONLINE, got %s", entries[0].Serial)
	}
}

// TestDeviceProxyManager_PortReuse 驗證設備移除後 port 可被新設備重用。
func TestDeviceProxyManager_PortReuse(t *testing.T) {
	port := freePort(t)
	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
	})
	defer dpm.Close()

	// 新增設備 A
	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "A", State: "device"},
	})
	entriesA := dpm.Entries()
	portA := entriesA[0].Port

	// 移除 A
	dpm.UpdateDevices(nil)

	// 新增設備 B — 應該重用 A 的 port
	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "B", State: "device"},
	})
	entriesB := dpm.Entries()
	if len(entriesB) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entriesB))
	}
	if entriesB[0].Port != portA {
		t.Errorf("expected port %d to be reused, got %d", portA, entriesB[0].Port)
	}
}

// TestDeviceProxyManager_IdempotentUpdate 驗證相同清單重複呼叫不產生副作用。
func TestDeviceProxyManager_IdempotentUpdate(t *testing.T) {
	port := freePort(t)

	var readyCount int
	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
		OnReady: func(serial string, port int) {
			readyCount++
		},
	})
	defer dpm.Close()

	devices := []DeviceInfo{
		{Serial: "DEV1", State: "device"},
		{Serial: "DEV2", State: "device"},
	}

	// 呼叫三次相同清單
	dpm.UpdateDevices(devices)
	dpm.UpdateDevices(devices)
	dpm.UpdateDevices(devices)

	// OnReady 應只被呼叫 2 次（首次新增 2 台）
	if readyCount != 2 {
		t.Errorf("expected 2 ready events (first call only), got %d", readyCount)
	}

	// Entries 應為 2 台
	if len(dpm.Entries()) != 2 {
		t.Errorf("expected 2 entries, got %d", len(dpm.Entries()))
	}
}

// TestDeviceProxyManager_ProxyAcceptsConnection 驗證新增設備後 proxy port 可接受 TCP 連線。
func TestDeviceProxyManager_ProxyAcceptsConnection(t *testing.T) {
	port := freePort(t)
	dpm := NewDeviceProxyManager(DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
	})
	defer dpm.Close()

	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "DEV1", State: "device"},
	})

	entries := dpm.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	// 嘗試連線到分配的 port
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", entries[0].Port), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to proxy port %d: %v", entries[0].Port, err)
	}
	conn.Close()
}

// TestDeviceProxyManager_ContextCancel 驗證 context 取消時 manager 正確清理。
func TestDeviceProxyManager_ContextCancel(t *testing.T) {
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())

	dpm := newDeviceProxyManagerWithCtx(ctx, DeviceProxyConfig{
		PortStart: port,
		OpenCh:    mockOpenCh(),
	})

	dpm.UpdateDevices([]DeviceInfo{
		{Serial: "DEV1", State: "device"},
	})

	entries := dpm.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	// 取消 context
	cancel()

	// 等一下讓 goroutine 清理
	time.Sleep(50 * time.Millisecond)

	// listener 應已關閉（port 應可再用）
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", entries[0].Port))
	if err != nil {
		t.Errorf("port should be free after context cancel: %v", err)
	} else {
		ln.Close()
	}
}
