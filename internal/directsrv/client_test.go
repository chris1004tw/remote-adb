package directsrv

import (
	"encoding/json"
	"net"
	"testing"
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
