package adb

import (
	"fmt"
	"io"
	"net"
)

// Dialer 負責透過本機 ADB server 建立與指定設備的 TCP 連線。
//
// 工作原理：ADB server 是一個 multiplexer，管理所有 USB/TCP 連接的裝置。
// 要與特定設備通訊，需先透過 host:transport:<serial> 指令「切換」到該設備，
// 然後發送 tcp:<port> 指令建立到設備上指定 port 的 TCP tunnel。
// 此 tunnel 的 net.Conn 可直接用於 ADB protocol 或其他 TCP 流量。
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

// DialService 連線到指定設備的指定服務。
// 與 DialDevice 類似，但接受任意 ADB service 字串（如 "shell:", "tcp:5555"）。
func (d *Dialer) DialService(serial string, service string) (net.Conn, error) {
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

	// 發送服務命令
	if err := sendADBCommand(conn, service); err != nil {
		conn.Close()
		return nil, fmt.Errorf("發送 service 指令失敗: %w", err)
	}
	if err := readOKAY(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("service 失敗: %w", err)
	}

	return conn, nil
}

// Addr 回傳 ADB server 地址。
func (d *Dialer) Addr() string {
	return d.addr
}

// Connect 告訴本機 ADB server 連線到指定的 TCP 位址。
// 等同 `adb connect <target>`，target 格式為 "host:port"。
func (d *Dialer) Connect(target string) error {
	conn, err := net.Dial("tcp", d.addr)
	if err != nil {
		return fmt.Errorf("連線 ADB server 失敗: %w", err)
	}
	defer conn.Close()

	cmd := fmt.Sprintf("host:connect:%s", target)
	if err := sendADBCommand(conn, cmd); err != nil {
		return fmt.Errorf("發送 connect 指令失敗: %w", err)
	}
	return readOKAY(conn)
}

// Disconnect 告訴本機 ADB server 中斷與指定 TCP 位址的連線。
// 等同 `adb disconnect <target>`，target 格式為 "host:port"。
func (d *Dialer) Disconnect(target string) error {
	conn, err := net.Dial("tcp", d.addr)
	if err != nil {
		return fmt.Errorf("連線 ADB server 失敗: %w", err)
	}
	defer conn.Close()

	cmd := fmt.Sprintf("host:disconnect:%s", target)
	if err := sendADBCommand(conn, cmd); err != nil {
		return fmt.Errorf("發送 disconnect 指令失敗: %w", err)
	}
	return readOKAY(conn)
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
