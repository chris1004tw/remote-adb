package adb

import (
	"fmt"
	"io"
	"net"
	"testing"
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
