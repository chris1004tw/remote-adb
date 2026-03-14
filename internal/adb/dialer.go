package adb

import (
	"fmt"
	"log/slog"
	"net"
	"time"
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
		return nil, fmt.Errorf("connect to ADB server: %w", err)
	}

	// 切換到目標設備
	transportCmd := fmt.Sprintf("host:transport:%s", serial)
	if err := SendCommand(conn, transportCmd); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send transport command: %w", err)
	}
	if err := ReadStatus(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("transport failed: %w", err)
	}

	// 建立 TCP tunnel
	tcpCmd := fmt.Sprintf("tcp:%d", port)
	if err := SendCommand(conn, tcpCmd); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send tcp command: %w", err)
	}
	if err := ReadStatus(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("create tcp tunnel: %w", err)
	}

	return conn, nil
}

// DialService 連線到指定設備的指定服務。
// 與 DialDevice 類似，但接受任意 ADB service 字串（如 "shell:", "tcp:5555"）。
func (d *Dialer) DialService(serial string, service string) (net.Conn, error) {
	conn, err := net.Dial("tcp", d.addr)
	if err != nil {
		return nil, fmt.Errorf("connect to ADB server: %w", err)
	}

	// 切換到目標設備
	transportCmd := fmt.Sprintf("host:transport:%s", serial)
	if err := SendCommand(conn, transportCmd); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send transport command: %w", err)
	}
	if err := ReadStatus(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("transport failed: %w", err)
	}

	// 發送服務命令
	if err := SendCommand(conn, service); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send service command: %w", err)
	}
	if err := ReadStatus(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("service failed: %w", err)
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
		return fmt.Errorf("connect to ADB server: %w", err)
	}
	defer conn.Close()

	cmd := fmt.Sprintf("host:connect:%s", target)
	if err := SendCommand(conn, cmd); err != nil {
		return fmt.Errorf("send connect command: %w", err)
	}
	return ReadStatus(conn)
}

// Disconnect 告訴本機 ADB server 中斷與指定 TCP 位址的連線。
// 等同 `adb disconnect <target>`，target 格式為 "host:port"。
func (d *Dialer) Disconnect(target string) error {
	conn, err := net.Dial("tcp", d.addr)
	if err != nil {
		return fmt.Errorf("connect to ADB server: %w", err)
	}
	defer conn.Close()

	cmd := fmt.Sprintf("host:disconnect:%s", target)
	if err := SendCommand(conn, cmd); err != nil {
		return fmt.Errorf("send disconnect command: %w", err)
	}
	return ReadStatus(conn)
}

// AutoConnect 等待指定延遲後嘗試 adb connect。
// 失敗時僅記錄 debug 日誌，不回傳錯誤。適合在背景 goroutine 中呼叫。
//
// 參數：
//   - adbAddr: ADB server 地址（空字串使用預設 127.0.0.1:5037）
//   - target: 連線目標，格式為 "host:port"
//   - delay: 連線前等待時間（讓 proxy listener 就緒）
func AutoConnect(adbAddr, target string, delay time.Duration) {
	if delay > 0 {
		time.Sleep(delay)
	}
	dialer := NewDialer(adbAddr)
	if err := dialer.Connect(target); err != nil {
		slog.Debug("auto adb connect failed", "target", target, "error", err)
	} else {
		slog.Debug("auto adb connect succeeded", "target", target)
	}
}

// AutoDisconnect 嘗試 adb disconnect。失敗時靜默忽略。適合在背景 goroutine 中呼叫。
//
// 參數：
//   - adbAddr: ADB server 地址（空字串使用預設 127.0.0.1:5037）
//   - target: 中斷連線目標，格式為 "host:port"
func AutoDisconnect(adbAddr, target string) {
	dialer := NewDialer(adbAddr)
	dialer.Disconnect(target)
}

