package adb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"
)

// autoConnectRetryDelays 是 AutoConnect 在 ADB server 不可達時的重試退避排程。
// 總等待時間約 90 秒，涵蓋使用者稍後啟動 scrcpy/uiauto.dev 等工具才帶起 ADB server 的場景。
var autoConnectRetryDelays = []time.Duration{
	1 * time.Second, 2 * time.Second, 4 * time.Second,
	8 * time.Second, 15 * time.Second, 30 * time.Second, 30 * time.Second,
}

var (
	isADBServerRunningFunc = IsADBServerRunning
	ensureADBFunc          = EnsureADB
)

// scrcpyLocalAbstractRetryDelays 是 scrcpy localabstract socket 尚未 ready 時的短暫重試排程。
// 背景：scrcpy server 啟動後，control/video/audio socket 常晚於 shell process 幾百毫秒才建立。
// 若第一次 DialService 命中 "ADB FAIL: closed"，在同一條 DataChannel 內短暫重試可避免
// 客戶端連續重建多條短命 DataChannel，降低 SCTP/GC 壓力。
var scrcpyLocalAbstractRetryDelays = []time.Duration{
	40 * time.Millisecond,
	80 * time.Millisecond,
	120 * time.Millisecond,
	160 * time.Millisecond,
	200 * time.Millisecond,
	250 * time.Millisecond,
	250 * time.Millisecond,
	250 * time.Millisecond,
}

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
// 內部委派給 DialService，service 為 "tcp:<port>"。
// 回傳的 net.Conn 可直接用於雙向資料傳輸。
func (d *Dialer) DialDevice(serial string, port int) (net.Conn, error) {
	return d.DialService(serial, fmt.Sprintf("tcp:%d", port))
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

// DialServiceWithRetry 連線到指定設備服務，必要時對 scrcpy localabstract 啟動競態做短暫重試。
//
// 目前僅在 service 為 "localabstract:scrcpy_*" 且 ADB 回覆 "FAIL closed" 時重試。
// 這代表 scrcpy server process 已啟動，但 socket 尚未 ready；在同一條 DataChannel 內等待
// 幾百毫秒通常就能成功，可大幅減少短命 DataChannel 重建風暴。
func (d *Dialer) DialServiceWithRetry(ctx context.Context, serial string, service string) (net.Conn, error) {
	conn, err := d.DialService(serial, service)
	if !shouldRetryDialService(service, err) {
		return conn, err
	}

	start := time.Now()
	for attempt, delay := range scrcpyLocalAbstractRetryDelays {
		retryTimer := time.NewTimer(delay)
		select {
		case <-retryTimer.C:
		case <-ctx.Done():
			retryTimer.Stop()
			return nil, ctx.Err()
		}

		conn, err = d.DialService(serial, service)
		if err == nil {
			slog.Debug("DialService retry succeeded",
				"serial", serial,
				"service", service,
				"attempt", attempt+2,
				"elapsed_ms", time.Since(start).Milliseconds(),
			)
			return conn, nil
		}
		if !shouldRetryDialService(service, err) {
			return nil, err
		}
	}

	slog.Debug("DialService retry exhausted",
		"serial", serial,
		"service", service,
		"attempts", len(scrcpyLocalAbstractRetryDelays)+1,
		"elapsed_ms", time.Since(start).Milliseconds(),
		"error", err,
	)
	return nil, err
}

func shouldRetryDialService(service string, err error) bool {
	return err != nil &&
		strings.HasPrefix(service, "localabstract:scrcpy_") &&
		strings.Contains(err.Error(), "ADB FAIL: closed")
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

// KillServer 要求本機 ADB server 立刻結束。
// ADB server 在收到 host:kill 後可能先回 OKAY，也可能直接關閉連線；
// 後者屬於預期行為，因此將 EOF/connection reset 視為成功。
func (d *Dialer) KillServer() error {
	conn, err := net.Dial("tcp", d.addr)
	if err != nil {
		return fmt.Errorf("connect to ADB server: %w", err)
	}
	defer conn.Close()

	if err := SendCommand(conn, "host:kill"); err != nil {
		return fmt.Errorf("send kill command: %w", err)
	}
	if err := ReadStatus(conn); err != nil && !isExpectedKillServerError(err) {
		return err
	}
	return nil
}

func isExpectedKillServerError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// AutoConnect 等待指定延遲後嘗試 adb connect，失敗時以指數退避重試。
// 僅在 ADB server 不可達（net.Dial 失敗）時重試，協定層錯誤（FAIL 回應）不重試。
// 適合在背景 goroutine 中呼叫，context 取消時立即停止重試。
//
// 參數：
//   - ctx: 控制重試生命週期的 context（設備移除或連線斷開時取消）
//   - adbAddr: ADB server 地址（空字串使用預設 127.0.0.1:5037）
//   - target: 連線目標，格式為 "host:port"
//   - delay: 連線前等待時間（讓 proxy listener 就緒）
func AutoConnect(ctx context.Context, adbAddr, target string, delay time.Duration) {
	if delay > 0 {
		delayTimer := time.NewTimer(delay)
		select {
		case <-delayTimer.C:
		case <-ctx.Done():
			delayTimer.Stop()
			return
		}
	}

	dialer := NewDialer(adbAddr)

	for attempt := 0; attempt <= len(autoConnectRetryDelays); attempt++ {
		err := dialer.Connect(target)
		if err == nil {
			if attempt > 0 {
				slog.Info("auto adb connect succeeded after retry",
					"target", target, "attempts", attempt+1)
			} else {
				slog.Debug("auto adb connect succeeded", "target", target)
			}
			return
		}

		slog.Debug("auto adb connect failed",
			"target", target, "attempt", attempt+1, "error", err)

		// 僅在 ADB server 不可達時重試（connection refused 等網路錯誤）。
		// 若 ADB server 有回應但回傳 FAIL（如 already connected），不重試。
		if !isDialError(err) {
			return
		}

		// 最後一次嘗試失敗，不再重試
		if attempt >= len(autoConnectRetryDelays) {
			return
		}

		retryTimer := time.NewTimer(autoConnectRetryDelays[attempt])
		select {
		case <-retryTimer.C:
		case <-ctx.Done():
			retryTimer.Stop()
			return
		}
	}
}

// isDialError 判斷錯誤是否為 TCP 連線層錯誤（ADB server 不可達）。
// 用於區分「ADB server 未啟動」（應重試）和「ADB server 回應 FAIL」（不重試）。
func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr)
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

// reconnectDelay 是 Reconnect 中 disconnect 後等待 ADB server 清理 transport 的時間。
const reconnectDelay = 200 * time.Millisecond

const adbServerRestartTimeout = 5 * time.Second

// Reconnect 先中斷再重新連線，確保清除 ADB server 中的陳舊 transport。
//
// 背景：當 ADB server 已有既存 transport 時，adb connect 只會回應 "already connected"
// 而不建立新連線。若既存 transport 實際已失效（如遠端 DataChannel 斷開），
// 設備會一直處於不可用狀態。先 disconnect 清除陳舊 transport，再 connect 建立
// 全新連線可解決此問題。
//
// 參數：
//   - ctx: 控制生命週期的 context（DPM 關閉時取消）
//   - adbAddr: ADB server 地址（空字串使用預設 127.0.0.1:5037）
//   - target: 連線目標，格式為 "host:port"
func Reconnect(ctx context.Context, adbAddr, target string) {
	dialer := NewDialer(adbAddr)

	// 先中斷（忽略錯誤：目標可能本就未連線）
	_ = dialer.Disconnect(target)

	// 短暫等待讓 ADB server 完成 transport 清理
	delayTimer := time.NewTimer(reconnectDelay)
	select {
	case <-delayTimer.C:
	case <-ctx.Done():
		delayTimer.Stop()
		return
	}

	// 重新連線
	if err := dialer.Connect(target); err != nil {
		slog.Debug("reconnect failed", "target", target, "error", err)
	} else {
		slog.Debug("reconnect succeeded", "target", target)
	}
}

// RestartServer 重啟本機 ADB server，清除所有現有 transport。
// 適合用於 UI 工具卡住、出現殘留裝置項目時的手動清場。
func RestartServer(ctx context.Context, adbAddr string) error {
	dialer := NewDialer(adbAddr)

	if isADBServerRunningFunc(dialer.addr) {
		if err := dialer.KillServer(); err != nil {
			return err
		}

		deadline := time.NewTimer(adbServerRestartTimeout)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer deadline.Stop()
		defer ticker.Stop()

		for isADBServerRunningFunc(dialer.addr) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				return fmt.Errorf("ADB server did not stop within %v", adbServerRestartTimeout)
			case <-ticker.C:
			}
		}
	}

	return ensureADBFunc(ctx, dialer.addr, nil)
}

// RefreshServerAndReconnect 先重啟本機 ADB server，再將指定 targets 重新掛回。
// 用於 GUI 的「重新添加遠端 ADB 設備到本機」按鈕，專門處理卡住或殘留 transport。
func RefreshServerAndReconnect(ctx context.Context, adbAddr string, targets []string) error {
	if err := RestartServer(ctx, adbAddr); err != nil {
		return err
	}

	dialer := NewDialer(adbAddr)
	var firstErr error
	for _, target := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := dialer.Connect(target); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			slog.Debug("refresh reconnect failed", "target", target, "error", err)
			continue
		}
		slog.Debug("refresh reconnect succeeded", "target", target)
	}
	return firstErr
}
