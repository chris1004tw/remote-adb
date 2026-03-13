package signal_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chris1004tw/remote-adb/internal/signal"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
	"github.com/coder/websocket"
)

// newTestConn 建立一個帶有真實 WebSocket 連線的測試用 Conn。
func newTestConn(t *testing.T) (*signal.Conn, func()) {
	t.Helper()

	// 啟動 httptest server 接受 WebSocket 升級
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// 保持連線直到 server 關閉
		<-r.Context().Done()
		c.CloseNow()
	}))

	// 建立 WebSocket 客戶端連線
	ws, _, err := websocket.Dial(t.Context(), srv.URL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("websocket.Dial: %v", err)
	}

	conn := signal.NewConn("test-1", protocol.RoleClient, "test-host", ws)
	return conn, func() {
		conn.Close()
		srv.Close()
	}
}

func TestConn_CloseIdempotent(t *testing.T) {
	conn, cleanup := newTestConn(t)
	defer cleanup()

	// 多次呼叫 Close 不應 panic
	conn.Close()
	conn.Close()
	conn.Close()
}

func TestConn_DoneClosedAfterClose(t *testing.T) {
	conn, cleanup := newTestConn(t)
	defer cleanup()

	conn.Close()

	select {
	case <-conn.Done():
		// Done channel 已關閉
	case <-time.After(time.Second):
		t.Error("Done channel should be closed after Close()")
	}
}

func TestConn_SendAfterClose(t *testing.T) {
	conn, cleanup := newTestConn(t)
	defer cleanup()

	conn.Close()

	// Close 後 Send 不應阻塞或 panic
	done := make(chan struct{})
	go func() {
		conn.Send(protocol.Envelope{Type: "test"})
		close(done)
	}()

	select {
	case <-done:
		// 正常完成
	case <-time.After(time.Second):
		t.Error("Send after Close should not block")
	}
}

func TestConn_Properties(t *testing.T) {
	conn, cleanup := newTestConn(t)
	defer cleanup()

	if conn.ID() != "test-1" {
		t.Errorf("ID: got %q, want %q", conn.ID(), "test-1")
	}
	if conn.Role() != protocol.RoleClient {
		t.Errorf("Role: got %v, want %v", conn.Role(), protocol.RoleClient)
	}
	if conn.Hostname() != "test-host" {
		t.Errorf("Hostname: got %q, want %q", conn.Hostname(), "test-host")
	}
}
