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
package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chris1004tw/remote-adb/internal/adb"
)

// FwdListener 追蹤一個 adb forward 的本機 TCP listener。
// 每當有連線進來，會建立 DataChannel 轉發到遠端設備的指定服務。
type FwdListener struct {
	ln         net.Listener
	serial     string
	localSpec  string
	remoteSpec string
	cancel     context.CancelFunc
}

// FwdCmd 表示解析後的 ADB forward 命令。
// 例如 `adb forward tcp:27183 localabstract:scrcpy` 解析為：
// Serial=""（未指定），LocalSpec="tcp:27183"，RemoteSpec="localabstract:scrcpy"。
type FwdCmd struct {
	Serial     string // 目標設備序號（可能為空，由 ResolveSerial 映射）
	LocalSpec  string // 本機 spec (e.g., "tcp:27183")
	RemoteSpec string // 遠端 spec (e.g., "localabstract:scrcpy")
}

// ForwardManager 管理 ADB proxy 的 forward listeners、設備清單和 CNXN 等待機制。
// 統一取代 pairTab 的 forward 相關欄位，同時實作 DeviceProvider 和 ReverseForwardManager interface。
type ForwardManager struct {
	mu            sync.Mutex
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

// --- ADB Server 協定輔助函式 ---
// ADB server 使用文字協定：每個命令/回應以 4 字元 hex 長度前綴 + 內容。
// 例如發送 "host:version" → "000chost:version"。
// 回應以 "OKAY" 或 "FAIL" + 4 字元 hex 長度 + 錯誤訊息。
// 命令發送與狀態讀取統一使用 adb.SendCommand / adb.ReadStatus。

// WriteADBOkay 寫入 ADB OKAY 回應。
func WriteADBOkay(w io.Writer) error {
	_, err := w.Write([]byte("OKAY"))
	return err
}

// WriteADBFail 寫入 ADB FAIL + 訊息。
func WriteADBFail(w io.Writer, msg string) error {
	resp := fmt.Sprintf("FAIL%04x%s", len(msg), msg)
	_, err := w.Write([]byte(resp))
	return err
}

// QueryDeviceFeatures 透過 ADB server 協定查詢指定設備的 feature 清單。
// 回傳逗號分隔的 feature 字串（如 "shell_v2,cmd,stat_v2,..."），
// 用於 CNXN 回應的 banner，讓遠端 adb client 知道設備支援哪些功能。
// 連線與讀寫皆有 5 秒逾時保護，避免 ADB server 無回應時無限阻塞。
func QueryDeviceFeatures(adbAddr, serial string) (string, error) {
	conn, err := net.DialTimeout("tcp", adbAddr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	cmd := fmt.Sprintf("host-serial:%s:features", serial)
	if err := adb.SendCommand(conn, cmd); err != nil {
		return "", err
	}
	if err := adb.ReadStatus(conn); err != nil {
		return "", err
	}

	// 讀取 hex-length-prefixed 回應
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseInt(string(lenBuf[:]), 16, 32)
	if err != nil {
		return "", err
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(conn, data); err != nil {
		return "", err
	}
	return string(data), nil
}

// --- Forward 命令解析 ---
// 以下函式解析 ADB server 協定中的 forward 相關命令。
// ADB 的 forward 命令有多種格式（帶/不帶 serial、帶/不帶 norebind），
// 這些解析器統一處理各種變體。

// ParseForwardCmd 解析 ADB forward 命令為 FwdCmd 結構。
// 支援格式：host:forward:、host:forward:norebind:、host-serial:<serial>:forward: 等。
// LocalSpec 和 RemoteSpec 以分號（;）分隔。
func ParseForwardCmd(cmd string) *FwdCmd {
	var rest, serial string

	switch {
	case strings.HasPrefix(cmd, "host-serial:"):
		after := cmd[len("host-serial:"):]
		idx := strings.Index(after, ":forward:")
		if idx < 0 {
			return nil
		}
		serial = after[:idx]
		rest = after[idx+len(":forward:"):]
	case strings.HasPrefix(cmd, "host:forward:"):
		rest = cmd[len("host:forward:"):]
	default:
		return nil
	}

	rest = strings.TrimPrefix(rest, "norebind:")

	parts := strings.SplitN(rest, ";", 2)
	if len(parts) != 2 {
		return nil
	}

	return &FwdCmd{Serial: serial, LocalSpec: parts[0], RemoteSpec: parts[1]}
}

// ParseKillForwardCmd 解析 killforward 命令，回傳 localSpec。
func ParseKillForwardCmd(cmd string) (string, bool) {
	switch {
	case strings.HasPrefix(cmd, "host-serial:"):
		after := cmd[len("host-serial:"):]
		idx := strings.Index(after, ":killforward:")
		if idx < 0 {
			return "", false
		}
		return after[idx+len(":killforward:"):], true
	case strings.HasPrefix(cmd, "host:killforward:"):
		return cmd[len("host:killforward:"):], true
	}
	return "", false
}

// IsKillForwardAll 判斷是否為 killforward-all 命令。
func IsKillForwardAll(cmd string) bool {
	if cmd == "host:killforward-all" {
		return true
	}
	return strings.HasPrefix(cmd, "host-serial:") && strings.HasSuffix(cmd, ":killforward-all")
}

// IsListForward 判斷是否為 list-forward 命令。
func IsListForward(cmd string) bool {
	if cmd == "host:list-forward" {
		return true
	}
	return strings.HasPrefix(cmd, "host-serial:") && strings.HasSuffix(cmd, ":list-forward")
}

// ParseLocalSpec 解析 forward 的 local spec（如 "tcp:27183"），回傳 port 數值。
// 目前僅支援 tcp: 格式，不支援 localabstract: 等其他 spec。
func ParseLocalSpec(spec string) (int, error) {
	if !strings.HasPrefix(spec, "tcp:") {
		return 0, fmt.Errorf("unsupported local spec: %s", spec)
	}
	port, err := strconv.Atoi(spec[4:])
	if err != nil {
		return 0, fmt.Errorf("invalid port: %s", spec)
	}
	return port, nil
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
	fm.mu.Lock()
	for _, d := range fm.devices {
		if d.State == "device" {
			fm.mu.Unlock()
			return d.Serial, d.Features
		}
	}
	readyCh := fm.deviceReadyCh
	fm.mu.Unlock()

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
	fm.mu.Lock()
	defer fm.mu.Unlock()
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
	fm.mu.Lock()
	defer fm.mu.Unlock()
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
	fm.mu.Lock()
	defer fm.mu.Unlock()

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

// --- 客戶端 Forward 攔截 ---
// 以下函式在客戶端（主控端）的 ADB proxy listener 上運作，
// 攔截 forward 相關命令並在本機建立 listener，非 forward 命令則透過 DataChannel 轉發。

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

// HandleForwardInterception 檢查並處理 forward 相關命令。回傳 true 表示已處理。
func (fm *ForwardManager) HandleForwardInterception(ctx context.Context, conn net.Conn, cmd string, openCh OpenChannelFunc) bool {
	if fc := ParseForwardCmd(cmd); fc != nil {
		fm.HandleForward(ctx, conn, fc, openCh)
		return true
	}

	if spec, ok := ParseKillForwardCmd(cmd); ok {
		fm.HandleKillForward(conn, spec)
		return true
	}

	if IsKillForwardAll(cmd) {
		fm.HandleKillForwardAll(conn)
		return true
	}

	if IsListForward(cmd) {
		fm.HandleListForward(conn)
		return true
	}

	return false
}

// HandleForward 攔截 adb forward 命令：在本機建立 TCP listener，
// 每個進入的連線透過 DataChannel 轉發到遠端設備的指定服務。
// 支援 tcp:0（自動分配 port，回傳實際 port 給 adb client）。
// 相同 LocalSpec 的舊 listener 會被自動替換（除非指定 norebind）。
func (fm *ForwardManager) HandleForward(ctx context.Context, conn net.Conn, fc *FwdCmd, openCh OpenChannelFunc) {
	port, err := ParseLocalSpec(fc.LocalSpec)
	if err != nil {
		WriteADBOkay(conn)
		WriteADBFail(conn, err.Error())
		return
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		WriteADBOkay(conn)
		WriteADBFail(conn, fmt.Sprintf("cannot bind: %v", err))
		return
	}

	actualPort := ln.Addr().(*net.TCPAddr).Port

	serial, ok := fm.ResolveSerial(fc.Serial)
	if !ok {
		ln.Close()
		WriteADBOkay(conn)
		WriteADBFail(conn, "cannot resolve target device serial")
		return
	}

	fwdCtx, fwdCancel := context.WithCancel(ctx)

	fl := &FwdListener{
		ln:         ln,
		serial:     serial,
		localSpec:  fc.LocalSpec,
		remoteSpec: fc.RemoteSpec,
		cancel:     fwdCancel,
	}

	fm.fwdMu.Lock()
	if fm.fwdListeners == nil {
		fm.fwdListeners = make(map[string]*FwdListener)
	}
	// 關閉相同 LocalSpec 的舊 listener
	if old, ok := fm.fwdListeners[fc.LocalSpec]; ok {
		old.cancel()
		old.ln.Close()
	}
	// tcp:0 要用實際 port 作為 key
	key := fc.LocalSpec
	if fc.LocalSpec == "tcp:0" {
		key = fmt.Sprintf("tcp:%d", actualPort)
		fl.localSpec = key
	}
	fm.fwdListeners[key] = fl
	fm.fwdMu.Unlock()

	go fm.fwdAcceptLoop(fwdCtx, fl, openCh)

	// 回應：OKAY (host ack) + OKAY (forward success)
	WriteADBOkay(conn)
	if fc.LocalSpec == "tcp:0" {
		// tcp:0 額外回傳分配的 port
		portStr := fmt.Sprintf("%d", actualPort)
		WriteADBOkay(conn)
		fmt.Fprintf(conn, "%04x%s", len(portStr), portStr)
	} else {
		WriteADBOkay(conn)
	}

	slog.Debug("forward established", "local", key, "remote", fc.RemoteSpec, "serial", serial, "port", actualPort)
}

// fwdAcceptLoop 持續接受 forward listener 的 TCP 連線，
// 每個連線由 handleFwdConn 在獨立 goroutine 中處理。
func (fm *ForwardManager) fwdAcceptLoop(ctx context.Context, fl *FwdListener, openCh OpenChannelFunc) {
	var connID atomic.Int64
	for {
		conn, err := fl.ln.Accept()
		if err != nil {
			return
		}
		id := connID.Add(1)
		go fm.handleFwdConn(ctx, conn, fl, openCh, id)
	}
}

// handleFwdConn 處理單一 forward 連線。
// 建立 DataChannel（label=adb-fwd/{id}/{serial}/{remoteSpec}），
// 遠端的 HandleADBForwardConn 會連線到 ADB server → 目標設備 → 指定服務，
// 然後雙向橋接 DataChannel ↔ 本機 TCP 連線。
func (fm *ForwardManager) handleFwdConn(ctx context.Context, conn net.Conn, fl *FwdListener, openCh OpenChannelFunc, id int64) {
	defer conn.Close()

	// DataChannel label: adb-fwd/{id}/{serial}/{remoteSpec}
	label := fmt.Sprintf("adb-fwd/%d/%s/%s", id, fl.serial, fl.remoteSpec)
	slog.Debug("forward connection", "id", id, "local", fl.localSpec, "remote", fl.remoteSpec)
	ch, err := openCh(label)
	if err != nil {
		slog.Debug("forward DataChannel creation failed", "label", label, "error", err)
		return
	}
	defer ch.Close()

	BiCopy(ctx, ch, conn)
}

// HandleKillForward 處理 killforward 命令。
func (fm *ForwardManager) HandleKillForward(conn net.Conn, localSpec string) {
	fm.fwdMu.Lock()
	fl, ok := fm.fwdListeners[localSpec]
	if ok {
		fl.cancel()
		fl.ln.Close()
		delete(fm.fwdListeners, localSpec)
	}
	fm.fwdMu.Unlock()

	WriteADBOkay(conn) // host ack
	if ok {
		WriteADBOkay(conn)
	} else {
		WriteADBFail(conn, fmt.Sprintf("listener '%s' not found", localSpec))
	}
}

// HandleKillForwardAll 處理 killforward-all 命令。
func (fm *ForwardManager) HandleKillForwardAll(conn net.Conn) {
	fm.fwdMu.Lock()
	for key, fl := range fm.fwdListeners {
		fl.cancel()
		fl.ln.Close()
		delete(fm.fwdListeners, key)
	}
	fm.fwdMu.Unlock()

	WriteADBOkay(conn)
	WriteADBOkay(conn)
}

// HandleListForward 處理 list-forward 命令。
func (fm *ForwardManager) HandleListForward(conn net.Conn) {
	fm.fwdMu.Lock()
	var lines []string
	for _, fl := range fm.fwdListeners {
		lines = append(lines, fmt.Sprintf("%s %s %s", fl.serial, fl.localSpec, fl.remoteSpec))
	}
	fm.fwdMu.Unlock()

	list := strings.Join(lines, "\n")
	if len(lines) > 0 {
		list += "\n"
	}

	WriteADBOkay(conn)
	fmt.Fprintf(conn, "%04x%s", len(list), list)
}

// --- Reverse Forward 管理（由 transport bridge 的 handleReverseOPEN 呼叫）---
// 注意：目前 reverse:forward: 回傳 FAIL 讓工具回退到 forward 模式（見 adb_transport.go），
// 因此 SetupReverseForward 僅在未來支援 reverse forward 時使用。

// SetupReverseForward 在客戶端建立 forward listener 來模擬 reverse forward。
// 原始的 reverse forward（設備 → 主機）在 P2P 架構下無法運作，
// 因為設備端的連線會到達遠端機器而非客戶端。
// 本函式將 reverse forward 轉換為等效的 forward（客戶端 → 設備），
// remoteSpec 是設備端的目標（如 localabstract:scrcpy），
// localSpec 是客戶端的本機 port（如 tcp:27183 或 tcp:0）。
func (fm *ForwardManager) SetupReverseForward(ctx context.Context, serial, localSpec, remoteSpec string, openCh OpenChannelFunc) (int, error) {
	port, err := ParseLocalSpec(localSpec)
	if err != nil {
		return 0, err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return 0, fmt.Errorf("cannot bind: %v", err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	fwdCtx, fwdCancel := context.WithCancel(ctx)
	fl := &FwdListener{
		ln:         ln,
		serial:     serial,
		localSpec:  localSpec,
		remoteSpec: remoteSpec,
		cancel:     fwdCancel,
	}

	fm.fwdMu.Lock()
	if fm.fwdListeners == nil {
		fm.fwdListeners = make(map[string]*FwdListener)
	}
	key := localSpec
	if localSpec == "tcp:0" {
		key = fmt.Sprintf("tcp:%d", actualPort)
		fl.localSpec = key
	}
	if old, ok := fm.fwdListeners[key]; ok {
		old.cancel()
		old.ln.Close()
	}
	fm.fwdListeners[key] = fl
	fm.fwdMu.Unlock()

	go fm.fwdAcceptLoop(fwdCtx, fl, openCh)

	slog.Debug("reverse forward established (converted to client forward)",
		"local", key, "remote", remoteSpec, "serial", serial, "port", actualPort)
	return actualPort, nil
}

// KillReverseForward 移除指定 remoteSpec 的 reverse forward。
// 實作 ReverseForwardManager.KillReverseForward。
func (fm *ForwardManager) KillReverseForward(remoteSpec string) bool {
	fm.fwdMu.Lock()
	defer fm.fwdMu.Unlock()

	for key, fl := range fm.fwdListeners {
		if fl.remoteSpec == remoteSpec {
			fl.cancel()
			fl.ln.Close()
			delete(fm.fwdListeners, key)
			return true
		}
	}
	return false
}

// KillReverseForwardAll 移除所有 reverse forward listeners。
// 實作 ReverseForwardManager.KillReverseForwardAll。
func (fm *ForwardManager) KillReverseForwardAll() {
	fm.fwdMu.Lock()
	defer fm.fwdMu.Unlock()

	for key, fl := range fm.fwdListeners {
		fl.cancel()
		fl.ln.Close()
		delete(fm.fwdListeners, key)
	}
}

// ListReverseForwards 回傳 reverse forward 清單（ADB 格式）。
// 實作 ReverseForwardManager.ListReverseForwards。
func (fm *ForwardManager) ListReverseForwards() string {
	fm.fwdMu.Lock()
	defer fm.fwdMu.Unlock()

	var lines []string
	for _, fl := range fm.fwdListeners {
		lines = append(lines, fmt.Sprintf("%s %s %s", fl.serial, fl.remoteSpec, fl.localSpec))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
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

// --- 被控端（伺服器端）Forward 處理 ---
// 被控端收到 adb-fwd DataChannel 後，連線到本機 ADB server 並轉發到設備服務。

// HandleADBForwardConn 處理來自客戶端的 forward DataChannel 連線。
// 流程：連線本機 ADB server → host:transport:<serial>（切換到目標設備）→
// 發送 remoteSpec（如 localabstract:scrcpy）→ 雙向橋接 DataChannel ↔ ADB 連線。
func (fm *ForwardManager) HandleADBForwardConn(ctx context.Context, rwc io.ReadWriteCloser, adbAddr, serial, remoteSpec string) {
	defer rwc.Close()

	conn, err := net.Dial("tcp", adbAddr)
	if err != nil {
		slog.Debug("forward: failed to connect ADB server", "error", err)
		return
	}
	defer conn.Close()

	// 切換到目標設備
	if err := adb.SendCommand(conn, fmt.Sprintf("host:transport:%s", serial)); err != nil {
		slog.Debug("forward: failed to send transport", "error", err)
		return
	}
	if err := adb.ReadStatus(conn); err != nil {
		slog.Debug("forward: transport failed", "serial", serial, "error", err)
		return
	}

	// 連線到 remote spec
	if err := adb.SendCommand(conn, remoteSpec); err != nil {
		slog.Debug("forward: failed to send remote spec", "error", err)
		return
	}
	if err := adb.ReadStatus(conn); err != nil {
		slog.Debug("forward: remote spec failed", "remoteSpec", remoteSpec, "error", err)
		return
	}

	// 雙向轉發（BiCopy 結束時關閉雙方，避免死鎖）
	BiCopy(ctx, rwc, conn)
}
