// Package adb 實作 ADB server protocol 通訊、設備追蹤與狀態管理。
//
// ADB（Android Debug Bridge）protocol 使用簡單的文字格式與 TCP 傳輸：
//   - 所有指令以「4 位 hex 長度 + 指令字串」格式發送（例如 "000chost:version"）
//   - 回應以 4 byte 狀態碼開頭："OKAY" 或 "FAIL"
//   - "FAIL" 後接 4 位 hex 長度 + 錯誤訊息
//
// 本 package 提供以下功能：
//   - Tracker：透過 host:track-devices 長連線即時監聽設備插拔
//   - DeviceTable：執行緒安全的設備狀態表，支援鎖定機制防止多用戶端競爭
//   - Dialer：透過 ADB server 建立與指定設備的 TCP tunnel
//   - EnsureADB：自動偵測/下載/啟動 ADB 環境
package adb

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"
)

// DeviceEvent 表示一次設備狀態快照（來自 track-devices）。
type DeviceEvent struct {
	Serial string
	State  string // "device", "offline", "unauthorized", "no permissions"
}

// Tracker 使用 ADB server 的 host:track-devices 指令，
// 透過長連線即時追蹤設備插拔與狀態變更。
type Tracker struct {
	addr       string        // ADB server 地址（預設 127.0.0.1:5037）
	retryCap   time.Duration // 最大重試間隔
	retryBase  time.Duration // 初始重試間隔
}

// NewTracker 建立一個新的 Tracker。
func NewTracker(addr string) *Tracker {
	if addr == "" {
		addr = "127.0.0.1:5037"
	}
	return &Tracker{
		addr:      addr,
		retryCap:  30 * time.Second,
		retryBase: 1 * time.Second,
	}
}

// Track 開始追蹤 ADB server 上的設備變化。
// 每當設備列表變動，會透過回傳的 channel 發送完整的設備列表快照。
// 會自動處理斷線重連（指數退避）。
// 呼叫者應透過取消 ctx 來停止追蹤。
func (t *Tracker) Track(ctx context.Context) <-chan []DeviceEvent {
	ch := make(chan []DeviceEvent, 8)
	go t.trackLoop(ctx, ch)
	return ch
}

func (t *Tracker) trackLoop(ctx context.Context, ch chan<- []DeviceEvent) {
	defer close(ch)

	retryDelay := t.retryBase
	for {
		if ctx.Err() != nil {
			return
		}

		err := t.connectAndTrack(ctx, ch)
		if ctx.Err() != nil {
			return
		}

		slog.Warn("ADB 追蹤連線中斷，準備重連",
			"error", err,
			"retry_delay", retryDelay,
		)

		select {
		case <-time.After(retryDelay):
			// 指數退避，上限 30 秒
			retryDelay = retryDelay * 2
			if retryDelay > t.retryCap {
				retryDelay = t.retryCap
			}
		case <-ctx.Done():
			return
		}
	}
}

func (t *Tracker) connectAndTrack(ctx context.Context, ch chan<- []DeviceEvent) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return fmt.Errorf("連線 ADB server 失敗: %w", err)
	}
	defer conn.Close()

	// 發送 host:track-devices 指令
	if err := sendADBCommand(conn, "host:track-devices"); err != nil {
		return fmt.Errorf("發送 track-devices 失敗: %w", err)
	}

	// 讀取 OKAY 回應
	status := make([]byte, 4)
	if _, err := io.ReadFull(conn, status); err != nil {
		return fmt.Errorf("讀取回應狀態失敗: %w", err)
	}
	if string(status) != "OKAY" {
		return fmt.Errorf("ADB server 回傳非 OKAY: %s", string(status))
	}

	slog.Info("已連接 ADB server，開始追蹤設備", "addr", t.addr)

	// host:track-devices 是長連線：ADB server 在每次設備列表變動時
	// 會主動推送完整的設備清單（而非差異）。連線保持直到任一端關閉。
	reader := bufio.NewReader(conn)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		devices, err := readDeviceList(reader)
		if err != nil {
			return fmt.Errorf("讀取設備列表失敗: %w", err)
		}

		select {
		case ch <- devices:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sendADBCommand 發送 ADB protocol 格式的指令。
// ADB wire format：4 位 hex 長度前綴 + ASCII 指令字串。
// 例如 "host:version"（長度 12）→ "000chost:version"。
// 這是 ADB server 原生採用的編碼方式（非 radb 自訂），所有 ADB client 都使用此格式。
func sendADBCommand(conn net.Conn, command string) error {
	msg := fmt.Sprintf("%04x%s", len(command), command)
	_, err := conn.Write([]byte(msg))
	return err
}

// readDeviceList 讀取一筆 ADB server 的設備列表回應。
// 格式：4 位 hex 長度 + payload（每行 serial\tstate\n）。
func readDeviceList(reader *bufio.Reader) ([]DeviceEvent, error) {
	// 讀取 4 位 hex 長度
	lenHex := make([]byte, 4)
	if _, err := io.ReadFull(reader, lenHex); err != nil {
		return nil, fmt.Errorf("讀取長度前綴失敗: %w", err)
	}

	length, err := parseHexLength(lenHex)
	if err != nil {
		return nil, err
	}

	if length == 0 {
		return []DeviceEvent{}, nil
	}

	// 讀取 payload
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, fmt.Errorf("讀取 payload 失敗: %w", err)
	}

	return ParseDeviceList(string(payload)), nil
}

// parseHexLength 解析 4 位 hex 長度字串。
func parseHexLength(h []byte) (int, error) {
	decoded, err := hex.DecodeString(string(h))
	if err != nil {
		return 0, fmt.Errorf("無效的 hex 長度: %s", string(h))
	}
	// 2 bytes -> uint16
	if len(decoded) != 2 {
		return 0, fmt.Errorf("hex 長度解碼後應為 2 bytes: got %d", len(decoded))
	}
	return int(decoded[0])<<8 | int(decoded[1]), nil
}

// ParseDeviceList 解析 ADB track-devices 回應的 payload。
// 每行格式：serial\tstate
func ParseDeviceList(payload string) []DeviceEvent {
	var devices []DeviceEvent
	lines := strings.Split(strings.TrimSpace(payload), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		devices = append(devices, DeviceEvent{
			Serial: parts[0],
			State:  parts[1],
		})
	}
	return devices
}
