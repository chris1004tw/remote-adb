// forward.go 實作 ADB forward 命令的攔截、轉譯與執行。
//
// # 為什麼需要 forward 攔截？
//
// scrcpy 等工具使用 `adb forward tcp:PORT localabstract:scrcpy` 在本機建立 TCP listener，
// 讓設備端的服務可以透過 ADB 通道回連。在 P2P 場景下，adb forward 指令會送到本機的
// proxy（而非真正的 ADB server），必須在本機攔截並轉譯為 DataChannel 轉發。
//
// # HandleProxyConn 的協定識別
//
// 每個連線的前 4 bytes 決定走哪條路：
//   - "CNXN"（0x434e584e LE）→ ADB device transport（`adb connect` 建立的連線）
//   - 4 個 hex 字元（如 "001c"）→ ADB server 協定（`adb forward`、`adb devices` 等命令）
//
// # ResolveSerial 的序號映射
//
// 本機 adb 認為設備序號是 127.0.0.1:<proxyPort>，但遠端設備的真實序號不同。
// ResolveSerial 執行映射：
//   - 若 requested 與某個遠端設備序號完全匹配 → 直接使用
//   - 若只有一台設備 → 自動映射（不管 requested 是什麼）
//   - 其他情況 → 失敗
//
// # Reverse Forward
//
// reverse forward（設備 → 主機）在 P2P 架構下的處理：
//   - 被控端（伺服器端）：不支援，回傳 FAIL 讓工具回退到 forward 模式
//   - 主控端（客戶端端）：SetupReverseForward 在客戶端建立 forward listener
//   - 伺服器端 HandleADBForwardConn：接收 DataChannel 並連線到本機 ADB → 設備服務
//
// 檔案結構：
//   - forward.go（本檔）：ForwardManager 核心（設備管理 + HandleProxyConn + CloseFwdListeners）
//   - forward_protocol.go：ADB forward 協定解析工具函式（無狀態）
//   - forward_intercept.go：客戶端 forward 攔截 + reverse forward + 被控端 forward 處理
package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// ForwardManager 管理 ADB proxy 的 forward listeners、設備清單和 CNXN 等待機制。
// 統一取代 pairTab 的 forward 相關欄位，同時實作 DeviceProvider 和 ReverseForwardManager interface。
type ForwardManager struct {
	mu            sync.RWMutex
	devices       []DeviceInfo // 遠端設備清單
	deviceReadyCh chan struct{} // 設備就緒信號（close = 有設備；重建 = 設備消失）

	fwdMu        sync.Mutex
	fwdListeners map[string]*FwdListener
}

// NewForwardManager 建立新的 ForwardManager。
func NewForwardManager() *ForwardManager {
	return &ForwardManager{
		deviceReadyCh: make(chan struct{}),
	}
}

// --- DeviceProvider interface 實作 ---

// GetDevice 實作 DeviceProvider.GetDevice。
// 取得第一個可用遠端設備的 serial 和 features。
// 若目前無設備（PeerConnection 仍在建立中、或遠端尚未插入手機），
// 等待 deviceReadyCh 信號（最多 timeout）。這避免了 CNXN 到達時
// 因設備清單尚未就緒而立即拒絕，導致 ADB server 每 250ms 重試的忙碌迴圈。
//
// 回傳值：serial 為空字串表示逾時或 context 取消，呼叫方應拒絕 CNXN。
func (fm *ForwardManager) GetDevice(ctx context.Context, timeout time.Duration) (serial, features string) {
	fm.mu.RLock()
	for _, d := range fm.devices {
		if d.State == "device" {
			fm.mu.RUnlock()
			return d.Serial, d.Features
		}
	}
	readyCh := fm.deviceReadyCh
	fm.mu.RUnlock()

	// deviceReadyCh 為 nil 表示不在客戶端模式（或已清理），直接回傳
	if readyCh == nil {
		return "", ""
	}

	slog.Debug("CNXN waiting for remote device")
	select {
	case <-readyCh:
	case <-ctx.Done():
		return "", ""
	case <-time.After(timeout):
		slog.Debug("remote device wait timeout, rejecting CNXN")
		return "", ""
	}

	// deviceReadyCh 已關閉（設備就緒），重新讀取設備清單
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	for _, d := range fm.devices {
		if d.State == "device" {
			return d.Serial, d.Features
		}
	}
	return "", ""
}

// OnlineDevices 實作 DeviceProvider.OnlineDevices。
// 回傳目前所有在線設備（State=="device"）的清單。
func (fm *ForwardManager) OnlineDevices() []DeviceInfo {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	var result []DeviceInfo
	for _, d := range fm.devices {
		if d.State == "device" {
			result = append(result, d)
		}
	}
	return result
}

// ResolveSerial 實作 DeviceProvider.ResolveSerial。
// 將 adb forward 命令中的 serial 映射為遠端真實裝置 serial。
//
// 映射邏輯：
//  1. 若 requested 完全匹配某個在線設備的 serial → 直接使用
//  2. 若只有一台在線設備 → 自動映射（不管 requested 是什麼值）
//  3. 其他情況 → 失敗（無法確定目標設備）
//
// 典型案例：本機 adb 認為設備序號是 127.0.0.1:<proxyPort>（因為是 adb connect 建立的），
// 但遠端設備的真實序號可能是 "emulator-5554" 或 USB 序號。
// 在只有一台設備的常見場景下，自動映射讓使用者無需手動指定。
func (fm *ForwardManager) ResolveSerial(requested string) (string, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	devs := make([]DeviceInfo, 0, len(fm.devices))
	for _, d := range fm.devices {
		if d.State == "device" {
			devs = append(devs, d)
		}
	}
	if len(devs) == 0 {
		return "", false
	}

	if requested != "" {
		for _, d := range devs {
			if d.Serial == requested {
				return requested, true
			}
		}
	}

	// requested 未命中時，若只有一台設備，允許自動映射（包含 127.0.0.1:15037 這類本機序號）
	if len(devs) == 1 {
		if requested != "" && requested != devs[0].Serial {
			slog.Debug("forward serial mapping", "from", requested, "to", devs[0].Serial)
		}
		return devs[0].Serial, true
	}

	return "", false
}

// UpdateDevices 更新設備清單並管理 deviceReadyCh 信號。
// 當首次出現在線設備時 close deviceReadyCh（解除 CNXN 等待），
// 當設備全部消失時重建 channel（讓後續 CNXN 等待）。
func (fm *ForwardManager) UpdateDevices(devices []DeviceInfo) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	fm.devices = devices

	hasDevice := false
	for _, d := range devices {
		if d.State == "device" {
			hasDevice = true
			break
		}
	}

	if hasDevice {
		select {
		case <-fm.deviceReadyCh:
			// 已關閉，不需要再次關閉
		default:
			close(fm.deviceReadyCh)
		}
	} else {
		select {
		case <-fm.deviceReadyCh:
			// 已關閉，需要重建
			fm.deviceReadyCh = make(chan struct{})
		default:
			// 尚未關閉，保持原狀
		}
	}
}

// --- HandleProxyConn ---

// HandleProxyConn 處理單一 proxy TCP 連線。
// 協定識別：先讀取前 4 bytes 判斷是 device transport 還是 server 協定。
//   - "CNXN"（二進位 0x43, 0x4e, 0x58, 0x4e）→ ADB device transport（`adb connect` 觸發）
//   - 4 hex 字元（如 "001c"）→ ADB server 協定（`adb forward`、`adb shell` 等命令）
//
// 對 server 協定命令的處理：
//   - forward/killforward/list-forward → 攔截並在本機處理
//   - 其他命令 → 建立 DataChannel（label=adb-server/{id}）轉發到遠端 ADB server
func (fm *ForwardManager) HandleProxyConn(ctx context.Context, conn net.Conn, openCh OpenChannelFunc, id int64) {
	defer conn.Close()

	// 讀取前 4 bytes 判斷協定類型
	var peek [4]byte
	if _, err := io.ReadFull(conn, peek[:]); err != nil {
		slog.Debug("failed to read first 4 bytes", "id", id, "error", err)
		return
	}

	// ADB device transport（來自 `adb connect`）
	if string(peek[:]) == "CNXN" {
		serial, features := fm.GetDevice(ctx, 30*time.Second)
		StartDeviceTransport(ctx, conn, peek[:], openCh, serial, features, fm)
		return
	}

	// ADB server 協定：前 4 bytes 是 hex 長度
	n, err := strconv.ParseInt(string(peek[:]), 16, 32)
	if err != nil {
		slog.Debug("invalid ADB request", "id", id, "first4", string(peek[:]))
		return
	}
	cmdBuf := make([]byte, n)
	if _, err := io.ReadFull(conn, cmdBuf); err != nil {
		slog.Debug("failed to read ADB command", "id", id, "error", err)
		return
	}
	raw := append(peek[:], cmdBuf...)
	cmd := string(cmdBuf)

	slog.Debug("proxy ← smart socket", "id", id, "cmd", cmd)

	// 檢查 forward 相關命令
	if fm.HandleForwardInterception(ctx, conn, cmd, openCh) {
		return
	}

	// 非 forward 命令：建立 DataChannel 轉發
	ch, err := openCh(fmt.Sprintf("adb-server/%d", id))
	if err != nil {
		slog.Debug("failed to create DataChannel", "id", id, "error", err)
		return
	}
	defer ch.Close()

	// 先寫入已讀取的命令，讓遠端 ADB server 收到完整請求
	if _, err := ch.Write(raw); err != nil {
		slog.Debug("failed to write command to DataChannel", "id", id, "error", err)
		return
	}

	BiCopy(ctx, ch, conn)
}

// CloseFwdListeners 關閉所有 forward listeners 並清空 map。
// 在 disconnect 時呼叫，確保釋放所有 port。
func (fm *ForwardManager) CloseFwdListeners() {
	fm.fwdMu.Lock()
	for key, fl := range fm.fwdListeners {
		fl.cancel()
		fl.ln.Close()
		delete(fm.fwdListeners, key)
	}
	fm.fwdListeners = nil
	fm.fwdMu.Unlock()
}
