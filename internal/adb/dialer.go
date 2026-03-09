package adb

import (
	"fmt"
	"io"
	"net"
)

// Dialer 負責透過 ADB server 建立與指定設備的 TCP 連線。
type Dialer struct {
	addr string // ADB server 地址（預設 127.0.0.1:5037）
}

// NewDialer 建立一個新的 Dialer。
func NewDialer(addr string) *Dialer {
	if addr == "" {
		addr = "127.0.0.1:5037"
	}
	return &Dialer{addr: addr}
}

// DialDevice 連線到指定設備的指定 TCP port。
// 內部流程：
//  1. 連線到 ADB server
//  2. 發送 host:transport:<serial> 切換到目標設備
//  3. 發送 tcp:<port> 建立 TCP tunnel
//
// 回傳的 net.Conn 可直接用於雙向資料傳輸。
func (d *Dialer) DialDevice(serial string, port int) (net.Conn, error) {
	conn, err := net.Dial("tcp", d.addr)
	if err != nil {
		return nil, fmt.Errorf("連線 ADB server 失敗: %w", err)
	}

	// 切換到目標設備
	transportCmd := fmt.Sprintf("host:transport:%s", serial)
	if err := sendADBCommand(conn, transportCmd); err != nil {
		conn.Close()
		return nil, fmt.Errorf("發送 transport 指令失敗: %w", err)
	}
	if err := readOKAY(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("transport 失敗: %w", err)
	}

	// 建立 TCP tunnel
	tcpCmd := fmt.Sprintf("tcp:%d", port)
	if err := sendADBCommand(conn, tcpCmd); err != nil {
		conn.Close()
		return nil, fmt.Errorf("發送 tcp 指令失敗: %w", err)
	}
	if err := readOKAY(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tcp tunnel 建立失敗: %w", err)
	}

	return conn, nil
}

// readOKAY 讀取 ADB server 的 4-byte 回應，預期為 "OKAY"。
// 如果收到 "FAIL"，會繼續讀取錯誤訊息。
func readOKAY(conn net.Conn) error {
	status := make([]byte, 4)
	if _, err := io.ReadFull(conn, status); err != nil {
		return fmt.Errorf("讀取回應狀態失敗: %w", err)
	}

	switch string(status) {
	case "OKAY":
		return nil
	case "FAIL":
		// 讀取錯誤訊息長度 + 內容
		lenHex := make([]byte, 4)
		if _, err := io.ReadFull(conn, lenHex); err != nil {
			return fmt.Errorf("ADB FAIL（無法讀取錯誤訊息）")
		}
		length, err := parseHexLength(lenHex)
		if err != nil {
			return fmt.Errorf("ADB FAIL（無法解析錯誤長度）")
		}
		msg := make([]byte, length)
		if _, err := io.ReadFull(conn, msg); err != nil {
			return fmt.Errorf("ADB FAIL（無法讀取錯誤內容）")
		}
		return fmt.Errorf("ADB FAIL: %s", string(msg))
	default:
		return fmt.Errorf("未預期的 ADB 回應: %s", string(status))
	}
}
