package adb

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestNewDialer_DefaultAddr(t *testing.T) {
	d := NewDialer("")
	if d.addr != "127.0.0.1:5037" {
		t.Errorf("default addr: got %q, want %q", d.addr, "127.0.0.1:5037")
	}
}

func TestNewDialer_CustomAddr(t *testing.T) {
	d := NewDialer("192.168.1.100:5038")
	if d.addr != "192.168.1.100:5038" {
		t.Errorf("custom addr: got %q, want %q", d.addr, "192.168.1.100:5038")
	}
}

func TestDialer_Addr(t *testing.T) {
	d := NewDialer("10.0.0.1:5037")
	if d.Addr() != "10.0.0.1:5037" {
		t.Errorf("Addr(): got %q, want %q", d.Addr(), "10.0.0.1:5037")
	}
}

// mockADBServer 啟動一個模擬 ADB server，對每個連線依序讀取命令並回應。
// handler 接收每個命令字串，回傳要寫回的完整 response bytes。
func mockADBServer(t *testing.T, handler func(cmd string) []byte) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				for {
					// 讀取 4 bytes hex length
					lenBuf := make([]byte, 4)
					if _, err := io.ReadFull(c, lenBuf); err != nil {
						return
					}
					n, err := parseHexLength(lenBuf)
					if err != nil {
						return
					}
					cmdBuf := make([]byte, n)
					if _, err := io.ReadFull(c, cmdBuf); err != nil {
						return
					}
					resp := handler(string(cmdBuf))
					if _, err := c.Write(resp); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return ln.Addr().String(), func() { ln.Close() }
}

func TestDialService_Success(t *testing.T) {
	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		// 對所有命令回應 OKAY
		return []byte("OKAY")
	})
	defer cleanup()

	d := NewDialer(addr)
	conn, err := d.DialService("SN123", "shell:ls")
	if err != nil {
		t.Fatalf("DialService error: %v", err)
	}
	conn.Close()
}

func TestDialService_TransportFail(t *testing.T) {
	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		if cmd == "host:transport:SN123" {
			msg := "device not found"
			return []byte(fmt.Sprintf("FAIL%04x%s", len(msg), msg))
		}
		return []byte("OKAY")
	})
	defer cleanup()

	d := NewDialer(addr)
	_, err := d.DialService("SN123", "shell:ls")
	if err == nil {
		t.Fatal("expected error for transport FAIL")
	}
}

func TestConnect_Success(t *testing.T) {
	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		return []byte("OKAY")
	})
	defer cleanup()

	d := NewDialer(addr)
	if err := d.Connect("192.168.1.50:5555"); err != nil {
		t.Fatalf("Connect error: %v", err)
	}
}

func TestDisconnect_Success(t *testing.T) {
	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		return []byte("OKAY")
	})
	defer cleanup()

	d := NewDialer(addr)
	if err := d.Disconnect("192.168.1.50:5555"); err != nil {
		t.Fatalf("Disconnect error: %v", err)
	}
}

func TestConnect_ServerFail(t *testing.T) {
	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		msg := "connection refused"
		return []byte(fmt.Sprintf("FAIL%04x%s", len(msg), msg))
	})
	defer cleanup()

	d := NewDialer(addr)
	err := d.Connect("192.168.1.50:5555")
	if err == nil {
		t.Fatal("expected error for FAIL response")
	}
}

func TestDialDevice_Success(t *testing.T) {
	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		return []byte("OKAY")
	})
	defer cleanup()

	d := NewDialer(addr)
	conn, err := d.DialDevice("SN123", 5555)
	if err != nil {
		t.Fatalf("DialDevice error: %v", err)
	}
	conn.Close()
}

func TestDialServiceWithRetry_ScrcpyClosedThenSuccess(t *testing.T) {
	origDelays := scrcpyLocalAbstractRetryDelays
	scrcpyLocalAbstractRetryDelays = []time.Duration{0, 0, 0}
	defer func() { scrcpyLocalAbstractRetryDelays = origDelays }()

	service := "localabstract:scrcpy_deadbeef"
	var mu sync.Mutex
	var serviceAttempts int

	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		switch cmd {
		case "host:transport:SN123":
			return []byte("OKAY")
		case service:
			mu.Lock()
			serviceAttempts++
			attempt := serviceAttempts
			mu.Unlock()
			if attempt < 3 {
				msg := "closed"
				return []byte(fmt.Sprintf("FAIL%04x%s", len(msg), msg))
			}
			return []byte("OKAY")
		default:
			return []byte("OKAY")
		}
	})
	defer cleanup()

	d := NewDialer(addr)
	conn, err := d.DialServiceWithRetry(context.Background(), "SN123", service)
	if err != nil {
		t.Fatalf("DialServiceWithRetry error: %v", err)
	}
	conn.Close()

	mu.Lock()
	defer mu.Unlock()
	if serviceAttempts != 3 {
		t.Fatalf("expected 3 service attempts, got %d", serviceAttempts)
	}
}

func TestDialServiceWithRetry_NonScrcpyClosedDoesNotRetry(t *testing.T) {
	origDelays := scrcpyLocalAbstractRetryDelays
	scrcpyLocalAbstractRetryDelays = []time.Duration{0, 0, 0}
	defer func() { scrcpyLocalAbstractRetryDelays = origDelays }()

	service := "localabstract:not-scrcpy"
	var mu sync.Mutex
	var serviceAttempts int

	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		switch cmd {
		case "host:transport:SN123":
			return []byte("OKAY")
		case service:
			mu.Lock()
			serviceAttempts++
			mu.Unlock()
			msg := "closed"
			return []byte(fmt.Sprintf("FAIL%04x%s", len(msg), msg))
		default:
			return []byte("OKAY")
		}
	})
	defer cleanup()

	d := NewDialer(addr)
	_, err := d.DialServiceWithRetry(context.Background(), "SN123", service)
	if err == nil {
		t.Fatal("expected error for non-scrcpy closed response")
	}

	mu.Lock()
	defer mu.Unlock()
	if serviceAttempts != 1 {
		t.Fatalf("expected 1 service attempt, got %d", serviceAttempts)
	}
}

func TestAutoConnect_SendsCorrectCommand(t *testing.T) {
	// 記錄 mock server 收到的命令
	var mu sync.Mutex
	var received []string

	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		mu.Lock()
		received = append(received, cmd)
		mu.Unlock()
		return []byte("OKAY")
	})
	defer cleanup()

	// AutoConnect 使用 delay=0 立即執行，不阻塞測試
	AutoConnect(context.Background(), addr, "127.0.0.1:5555", 0)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 command, got %d", len(received))
	}
	want := "host:connect:127.0.0.1:5555"
	if received[0] != want {
		t.Errorf("command: got %q, want %q", received[0], want)
	}
}

func TestAutoConnect_WithDelay(t *testing.T) {
	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		return []byte("OKAY")
	})
	defer cleanup()

	start := time.Now()
	AutoConnect(context.Background(), addr, "127.0.0.1:5555", 100*time.Millisecond)
	elapsed := time.Since(start)

	// 確認至少等待了指定的延遲時間
	if elapsed < 100*time.Millisecond {
		t.Errorf("expected delay >= 100ms, got %v", elapsed)
	}
}

func TestAutoConnect_ServerFail_NoRetry(t *testing.T) {
	// ADB server 有回應但 FAIL → 不應重試（協定層錯誤）
	var mu sync.Mutex
	var attempts int

	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		mu.Lock()
		attempts++
		mu.Unlock()
		msg := "already connected"
		return []byte(fmt.Sprintf("FAIL%04x%s", len(msg), msg))
	})
	defer cleanup()

	AutoConnect(context.Background(), addr, "127.0.0.1:5555", 0)

	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("expected 1 attempt (no retry on protocol FAIL), got %d", attempts)
	}
}

// unusedPort 取得一個未使用的 port：先 Listen 佔住，記錄 port，再關閉。
// 短時間內幾乎不會被其他程式搶佔，比寫死 port 號可靠。
func unusedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find unused port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestAutoConnect_RetriesOnServerUnavailable(t *testing.T) {
	// ADB server 不存在 → 應以退避重試直到 context 逾時
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	fakeAddr := unusedPort(t)

	start := time.Now()
	AutoConnect(ctx, fakeAddr, "127.0.0.1:5555", 0)
	elapsed := time.Since(start)

	// 應重試到 context 逾時（~4 秒），而非首次失敗就立即返回
	if elapsed < 3*time.Second {
		t.Errorf("AutoConnect returned too quickly: %v, expected >= 3s (should retry until ctx timeout)", elapsed)
	}
}

func TestAutoConnect_RetriesUntilSuccess(t *testing.T) {
	// 模擬 ADB server 延遲啟動：前 N 次連線失敗，之後成功
	var mu sync.Mutex
	var attempts int

	// 先佔住 port 再釋放，確保延遲啟動的 server 能綁到同一個 port
	addr := unusedPort(t)

	go func() {
		time.Sleep(2 * time.Second)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// port 被搶佔（極低概率），測試會因 context 逾時而失敗
			return
		}
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				lenBuf := make([]byte, 4)
				if _, err := io.ReadFull(c, lenBuf); err != nil {
					return
				}
				n, _ := parseHexLength(lenBuf)
				cmdBuf := make([]byte, n)
				if _, err := io.ReadFull(c, cmdBuf); err != nil {
					return
				}
				mu.Lock()
				attempts++
				mu.Unlock()
				c.Write([]byte("OKAY"))
			}(conn)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	AutoConnect(ctx, addr, "127.0.0.1:5555", 0)
	elapsed := time.Since(start)

	// 應在 server 啟動後成功連線（~2-4 秒之間）
	if elapsed < 2*time.Second {
		t.Errorf("AutoConnect succeeded too quickly: %v, server starts at 2s", elapsed)
	}
	if elapsed > 8*time.Second {
		t.Errorf("AutoConnect took too long: %v, expected < 8s", elapsed)
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts < 1 {
		t.Error("expected at least 1 successful attempt")
	}
}

func TestAutoConnect_Cancellation(t *testing.T) {
	// context 取消後應迅速停止重試
	ctx, cancel := context.WithCancel(context.Background())
	fakeAddr := unusedPort(t)

	done := make(chan struct{})
	go func() {
		AutoConnect(ctx, fakeAddr, "127.0.0.1:5555", 0)
		close(done)
	}()

	// 1.5 秒後取消
	time.Sleep(1500 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// 取消後迅速停止 — 正確
	case <-time.After(3 * time.Second):
		t.Fatal("AutoConnect did not stop within 3s after context cancellation")
	}
}

func TestAutoConnect_DelayCancellation(t *testing.T) {
	// 在 delay 等待階段就取消 context
	ctx, cancel := context.WithCancel(context.Background())
	fakeAddr := unusedPort(t)

	done := make(chan struct{})
	go func() {
		AutoConnect(ctx, fakeAddr, "127.0.0.1:5555", 10*time.Second)
		close(done)
	}()

	// 500ms 後取消（delay 還在等）
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// delay 被取消，迅速返回 — 正確
	case <-time.After(2 * time.Second):
		t.Fatal("AutoConnect did not stop during delay after context cancellation")
	}
}

func TestAutoDisconnect_SendsCorrectCommand(t *testing.T) {
	var mu sync.Mutex
	var received []string

	addr, cleanup := mockADBServer(t, func(cmd string) []byte {
		mu.Lock()
		received = append(received, cmd)
		mu.Unlock()
		return []byte("OKAY")
	})
	defer cleanup()

	AutoDisconnect(addr, "127.0.0.1:5555")

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 command, got %d", len(received))
	}
	want := "host:disconnect:127.0.0.1:5555"
	if received[0] != want {
		t.Errorf("command: got %q, want %q", received[0], want)
	}
}
