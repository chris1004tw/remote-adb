package directsrv

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/chris1004tw/remote-adb/internal/bridge"
)

// TestQueryDevices_OK 驗證 QueryDevices 正確解碼成功回應。
func TestQueryDevices_OK(t *testing.T) {
	// 啟動 mock TCP server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// 讀取 request
		var req Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}

		// 回傳成功 response
		resp := Response{
			OK:       true,
			Hostname: "test-host",
			Devices: []DeviceInfo{
				{Serial: "abc123", State: "device"},
				{Serial: "def456", State: "offline"},
			},
		}
		json.NewEncoder(conn).Encode(resp)
	}()

	resp, err := QueryDevices(ln.Addr().String(), "test-token")
	if err != nil {
		t.Fatalf("QueryDevices() error = %v", err)
	}

	if resp.Hostname != "test-host" {
		t.Errorf("Hostname = %q, want %q", resp.Hostname, "test-host")
	}
	if len(resp.Devices) != 2 {
		t.Fatalf("len(Devices) = %d, want 2", len(resp.Devices))
	}
	if resp.Devices[0].Serial != "abc123" {
		t.Errorf("Devices[0].Serial = %q, want %q", resp.Devices[0].Serial, "abc123")
	}
}

// TestQueryDevices_ServerError 驗證 QueryDevices 正確處理伺服器回報的錯誤。
func TestQueryDevices_ServerError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		var req Request
		json.NewDecoder(conn).Decode(&req)

		resp := Response{OK: false, Error: "token mismatch"}
		json.NewEncoder(conn).Encode(resp)
	}()

	_, err = QueryDevices(ln.Addr().String(), "bad-token")
	if err == nil {
		t.Fatal("QueryDevices() expected error, got nil")
	}
	if got := err.Error(); got != "server: token mismatch" {
		t.Errorf("error = %q, want %q", got, "server: token mismatch")
	}
}

// TestQueryDevices_DialError 驗證 QueryDevices 在連線失敗時回傳錯誤。
func TestQueryDevices_DialError(t *testing.T) {
	_, err := QueryDevices("127.0.0.1:1", "token") // port 1 幾乎不會有服務
	if err == nil {
		t.Fatal("QueryDevices() expected dial error, got nil")
	}
}

// TestQueryDevices_TokenForwarded 驗證 QueryDevices 正確傳遞 token。
func TestQueryDevices_TokenForwarded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	receivedToken := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		var req Request
		json.NewDecoder(conn).Decode(&req)
		receivedToken <- req.Token

		json.NewEncoder(conn).Encode(Response{OK: true})
	}()

	_, err = QueryDevices(ln.Addr().String(), "my-secret")
	if err != nil {
		t.Fatalf("QueryDevices() error = %v", err)
	}

	if got := <-receivedToken; got != "my-secret" {
		t.Errorf("token = %q, want %q", got, "my-secret")
	}
}

// TestToBridgeDevices 驗證 ToBridgeDevices 正確轉換設備清單。
func TestToBridgeDevices(t *testing.T) {
	input := []DeviceInfo{
		{Serial: "abc123", State: "device", LockedBy: "user1"},
		{Serial: "def456", State: "offline"},
	}
	got := ToBridgeDevices(input)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Serial != "abc123" || got[0].State != "device" {
		t.Errorf("got[0] = %+v, want Serial=abc123 State=device", got[0])
	}
	if got[1].Serial != "def456" || got[1].State != "offline" {
		t.Errorf("got[1] = %+v, want Serial=def456 State=offline", got[1])
	}
	// Features 應為空（directsrv 不含此欄位）
	if got[0].Features != "" {
		t.Errorf("Features should be empty, got %q", got[0].Features)
	}
}

// TestToBridgeDevices_Empty 驗證空切片輸入回傳空切片（非 nil）。
func TestToBridgeDevices_Empty(t *testing.T) {
	got := ToBridgeDevices([]DeviceInfo{})
	if got == nil {
		t.Fatal("should return empty slice, not nil")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// TestPollDeviceLoop_ContextCancel 驗證 PollDeviceLoop 在 context 取消後退出。
func TestPollDeviceLoop_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	callCount := 0
	var received []bridge.DeviceInfo

	done := make(chan struct{})
	go func() {
		PollDeviceLoop(ctx, 50*time.Millisecond,
			func() []DeviceInfo {
				callCount++
				return []DeviceInfo{{Serial: "s1", State: "device"}}
			},
			func(devs []bridge.DeviceInfo) {
				received = devs
			},
		)
		close(done)
	}()

	// 等待至少一次輪詢
	time.Sleep(120 * time.Millisecond)
	cancel()
	<-done

	if callCount == 0 {
		t.Error("queryFn should have been called at least once")
	}
	if len(received) != 1 || received[0].Serial != "s1" {
		t.Errorf("unexpected received: %+v", received)
	}
}

// TestPollDeviceLoop_NilSkipsUpdate 驗證 queryFn 回傳 nil 時跳過 onUpdate。
func TestPollDeviceLoop_NilSkipsUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	updateCalled := false

	done := make(chan struct{})
	go func() {
		PollDeviceLoop(ctx, 50*time.Millisecond,
			func() []DeviceInfo { return nil }, // 模擬查詢失敗
			func(devs []bridge.DeviceInfo) { updateCalled = true },
		)
		close(done)
	}()

	time.Sleep(120 * time.Millisecond)
	cancel()
	<-done

	if updateCalled {
		t.Error("onUpdate should not be called when queryFn returns nil")
	}
}
