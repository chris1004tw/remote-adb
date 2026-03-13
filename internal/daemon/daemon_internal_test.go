package daemon

import (
	"testing"
	"time"

	"github.com/chris1004tw/remote-adb/pkg/protocol"
)

func TestUpdateHostDevices_UnknownHost(t *testing.T) {
	d := NewDaemon(Config{})

	// 先設定已知 host
	d.hostsMu.Lock()
	d.hosts = []protocol.HostInfo{
		{HostID: "host-1", Hostname: "known", Devices: nil},
	}
	d.hostsMu.Unlock()

	// 更新已知 host — 應正常更新
	d.updateHostDevices("host-1", []protocol.DeviceInfo{
		{Serial: "DEV001", State: protocol.DeviceStateOnline},
	})
	d.hostsMu.RLock()
	if len(d.hosts) != 1 || len(d.hosts[0].Devices) != 1 {
		t.Errorf("已知 host 更新失敗")
	}
	d.hostsMu.RUnlock()

	// 更新未知 host — 應新增
	d.updateHostDevices("host-2", []protocol.DeviceInfo{
		{Serial: "DEV002", State: protocol.DeviceStateOnline},
	})
	d.hostsMu.RLock()
	defer d.hostsMu.RUnlock()
	if len(d.hosts) != 2 {
		t.Fatalf("hosts 數量 = %d, 預期 2", len(d.hosts))
	}
	found := false
	for _, h := range d.hosts {
		if h.HostID == "host-2" && len(h.Devices) == 1 {
			found = true
		}
	}
	if !found {
		t.Error("未知 host 應被新增到 hosts 列表")
	}
}

func TestWaitResponse_Success(t *testing.T) {
	d := NewDaemon(Config{})

	key := "test_resp:123"
	expected := protocol.Envelope{Type: "lock_resp"}

	// 背景送出回應
	go func() {
		time.Sleep(50 * time.Millisecond)
		d.deliverResponse(key, expected)
	}()

	got, err := d.waitResponse(key, 2*time.Second)
	if err != nil {
		t.Fatalf("waitResponse returned error: %v", err)
	}
	if got.Type != expected.Type {
		t.Errorf("got type %q, want %q", got.Type, expected.Type)
	}
}

func TestWaitResponse_Timeout(t *testing.T) {
	d := NewDaemon(Config{})

	_, err := d.waitResponse("no_reply:456", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestDeliverResponse_NoWaiter(t *testing.T) {
	d := NewDaemon(Config{})

	// 無人等待時不應阻塞或 panic
	done := make(chan struct{})
	go func() {
		d.deliverResponse("orphan:789", protocol.Envelope{Type: "answer"})
		close(done)
	}()

	select {
	case <-done:
		// 正常完成
	case <-time.After(time.Second):
		t.Fatal("deliverResponse blocked when no waiter exists")
	}
}

func TestWaitResponse_Cleanup(t *testing.T) {
	d := NewDaemon(Config{})

	key := "cleanup:test"

	// 等待逾時
	d.waitResponse(key, 50*time.Millisecond)

	// 逾時後 waiters map 應已清除該 key
	d.waiterMu.Lock()
	_, exists := d.waiters[key]
	d.waiterMu.Unlock()

	if exists {
		t.Error("waiter should be cleaned up after timeout")
	}
}

func TestShutdown_Empty(t *testing.T) {
	d := NewDaemon(Config{})

	// 無資源時 shutdown 不應 panic
	if err := d.shutdown(); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}

	// maps 應被重建（非 nil）
	d.proxyMu.Lock()
	defer d.proxyMu.Unlock()
	if d.proxies == nil {
		t.Error("proxies map should be reinitialized after shutdown")
	}
	if d.peers == nil {
		t.Error("peers map should be reinitialized after shutdown")
	}
}

func TestGenerateSessionID(t *testing.T) {
	id1 := generateSessionID()
	id2 := generateSessionID()

	if len(id1) != 16 {
		t.Errorf("session ID length: got %d, want 16", len(id1))
	}
	if id1 == id2 {
		t.Error("two generated session IDs should be different")
	}
}
