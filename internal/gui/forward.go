// forward.go 實作 ADB forward 命令的攔截、轉譯與執行。
//
// # 為什麼需要 forward 攔截？
//
// scrcpy 等工具使用 `adb forward tcp:PORT localabstract:scrcpy` 在本機建立 TCP listener，
// 讓設備端的服務可以透過 ADB 通道回連。在 P2P 場景下，adb forward 指令會送到本機的
// proxy（而非真正的 ADB server），必須在本機攔截並轉譯為 DataChannel 轉發。
//
// # handleProxyConn 的協定識別
//
// 每個連線的前 4 bytes 決定走哪條路：
//   - "CNXN"（0x434e584e LE）→ ADB device transport（`adb connect` 建立的連線）
//   - 4 個 hex 字元（如 "001c"）→ ADB server 協定（`adb forward`、`adb devices` 等命令）
//
// # resolveForwardSerial 的序號映射
//
// 本機 adb 認為設備序號是 127.0.0.1:<proxyPort>，但遠端設備的真實序號不同。
// resolveForwardSerial 執行映射：
//   - 若 requested 與某個遠端設備序號完全匹配 → 直接使用
//   - 若只有一台設備 → 自動映射（不管 requested 是什麼）
//   - 其他情況 → 失敗
//
// # Reverse Forward
//
// reverse forward（設備 → 主機）在 P2P 架構下的處理：
//   - 被控端（伺服器端）：不支援，回傳 FAIL 讓工具回退到 forward 模式
//   - 主控端（客戶端端）：setupReverseForward 在客戶端建立 forward listener
//   - 伺服器端 handleADBForwardConn：接收 DataChannel 並連線到本機 ADB → 設備服務
package gui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
)

// fwdListener 追蹤一個 adb forward 的本機 TCP listener。
// 每當有連線進來，會建立 DataChannel 轉發到遠端設備的指定服務。
type fwdListener struct {
	ln         net.Listener
	serial     string
	localSpec  string
	remoteSpec string
	cancel     context.CancelFunc
}

// fwdCmd 表示解析後的 ADB forward 命令。
// 例如 `adb forward tcp:27183 localabstract:scrcpy` 解析為：
// serial=""（未指定），localSpec="tcp:27183"，remoteSpec="localabstract:scrcpy"。
type fwdCmd struct {
	serial     string // 目標設備序號（可能為空，由 resolveForwardSerial 映射）
	localSpec  string // 本機 spec (e.g., "tcp:27183")
	remoteSpec string // 遠端 spec (e.g., "localabstract:scrcpy")
}

// --- ADB Server 協定輔助函式 ---
// ADB server 使用文字協定：每個命令/回應以 4 字元 hex 長度前綴 + 內容。
// 例如發送 "host:version" → "000chost:version"。
// 回應以 "OKAY" 或 "FAIL" + 4 字元 hex 長度 + 錯誤訊息。

// sendADBCmd 發送 ADB 協定命令（4 hex chars 長度 + 命令字串）。
func sendADBCmd(w io.Writer, cmd string) error {
	msg := fmt.Sprintf("%04x%s", len(cmd), cmd)
	_, err := io.WriteString(w, msg)
	return err
}

// writeADBOkay 寫入 ADB OKAY 回應。
func writeADBOkay(w io.Writer) error {
	_, err := w.Write([]byte("OKAY"))
	return err
}

// writeADBFail 寫入 ADB FAIL + 訊息。
func writeADBFail(w io.Writer, msg string) error {
	resp := fmt.Sprintf("FAIL%04x%s", len(msg), msg)
	_, err := w.Write([]byte(resp))
	return err
}

// readADBStatus 讀取 ADB 4-byte 狀態回應。
func readADBStatus(r io.Reader) error {
	status := make([]byte, 4)
	if _, err := io.ReadFull(r, status); err != nil {
		return err
	}
	if string(status) != "OKAY" {
		return fmt.Errorf("expected OKAY, got %s", string(status))
	}
	return nil
}

// queryDeviceFeatures 透過 ADB server 協定查詢指定設備的 feature 清單。
// 回傳逗號分隔的 feature 字串（如 "shell_v2,cmd,stat_v2,..."），
// 用於 CNXN 回應的 banner，讓遠端 adb client 知道設備支援哪些功能。
func queryDeviceFeatures(adbAddr, serial string) (string, error) {
	conn, err := net.Dial("tcp", adbAddr)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	cmd := fmt.Sprintf("host-serial:%s:features", serial)
	if err := sendADBCmd(conn, cmd); err != nil {
		return "", err
	}
	if err := readADBStatus(conn); err != nil {
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

// parseForwardCmd 解析 ADB forward 命令為 fwdCmd 結構。
// 支援格式：host:forward:、host:forward:norebind:、host-serial:<serial>:forward: 等。
// localSpec 和 remoteSpec 以分號（;）分隔。
func parseForwardCmd(cmd string) *fwdCmd {
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

	return &fwdCmd{serial: serial, localSpec: parts[0], remoteSpec: parts[1]}
}

// parseKillForwardCmd 解析 killforward 命令，回傳 localSpec。
func parseKillForwardCmd(cmd string) (string, bool) {
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

// isKillForwardAll 判斷是否為 killforward-all 命令。
func isKillForwardAll(cmd string) bool {
	if cmd == "host:killforward-all" {
		return true
	}
	return strings.HasPrefix(cmd, "host-serial:") && strings.HasSuffix(cmd, ":killforward-all")
}

// isListForward 判斷是否為 list-forward 命令。
func isListForward(cmd string) bool {
	if cmd == "host:list-forward" {
		return true
	}
	return strings.HasPrefix(cmd, "host-serial:") && strings.HasSuffix(cmd, ":list-forward")
}

// parseLocalSpec 解析 forward 的 local spec（如 "tcp:27183"），回傳 port 數值。
// 目前僅支援 tcp: 格式，不支援 localabstract: 等其他 spec。
func parseLocalSpec(spec string) (int, error) {
	if !strings.HasPrefix(spec, "tcp:") {
		return 0, fmt.Errorf("unsupported local spec: %s", spec)
	}
	port, err := strconv.Atoi(spec[4:])
	if err != nil {
		return 0, fmt.Errorf("invalid port: %s", spec)
	}
	return port, nil
}

// --- 客戶端 Forward 攔截 ---
// 以下函式在客戶端（主控端）的 ADB proxy listener 上運作，
// 攔截 forward 相關命令並在本機建立 listener，非 forward 命令則透過 DataChannel 轉發。

// handleProxyConn 處理單一 proxy TCP 連線。
// 協定識別：先讀取前 4 bytes 判斷是 device transport 還是 server 協定。
//   - "CNXN"（二進位 0x43, 0x4e, 0x58, 0x4e）→ ADB device transport（`adb connect` 觸發）
//   - 4 hex 字元（如 "001c"）→ ADB server 協定（`adb forward`、`adb shell` 等命令）
//
// 對 server 協定命令的處理：
//   - forward/killforward/list-forward → 攔截並在本機處理
//   - 其他命令 → 建立 DataChannel（label=adb-server/{id}）轉發到遠端 ADB server
func (t *pairTab) handleProxyConn(ctx context.Context, conn net.Conn, openCh openChannelFunc, id int64) {
	defer conn.Close()

	// 讀取前 4 bytes 判斷協定類型
	var peek [4]byte
	if _, err := io.ReadFull(conn, peek[:]); err != nil {
		slog.Debug("讀取前 4 bytes 失敗", "id", id, "error", err)
		return
	}

	// ADB device transport（來自 `adb connect`）
	if string(peek[:]) == "CNXN" {
		// 從 cliDevices 取得設備資訊
		t.mu.Lock()
		var serial, features string
		for _, d := range t.cliDevices {
			if d.State == "device" {
				serial = d.Serial
				features = d.Features
				break
			}
		}
		t.mu.Unlock()
		startDeviceTransport(ctx, conn, peek[:], openCh, serial, features, t)
		return
	}

	// ADB server 協定：前 4 bytes 是 hex 長度
	n, err := strconv.ParseInt(string(peek[:]), 16, 32)
	if err != nil {
		slog.Debug("無效的 ADB 請求", "id", id, "first4", string(peek[:]))
		return
	}
	cmdBuf := make([]byte, n)
	if _, err := io.ReadFull(conn, cmdBuf); err != nil {
		slog.Debug("讀取 ADB 命令失敗", "id", id, "error", err)
		return
	}
	raw := append(peek[:], cmdBuf...)
	cmd := string(cmdBuf)

	slog.Debug("proxy ← smart socket", "id", id, "cmd", cmd)

	// 檢查 forward 相關命令
	if t.handleForwardInterception(ctx, conn, cmd, openCh) {
		return
	}

	// 非 forward 命令：建立 DataChannel 轉發
	ch, err := openCh(fmt.Sprintf("adb-server/%d", id))
	if err != nil {
		slog.Debug("建立 DataChannel 失敗", "id", id, "error", err)
		return
	}
	defer ch.Close()

	// 先寫入已讀取的命令，讓遠端 ADB server 收到完整請求
	if _, err := ch.Write(raw); err != nil {
		slog.Debug("寫入命令到 DataChannel 失敗", "id", id, "error", err)
		return
	}

	biCopy(ctx, ch, conn)
}

// handleForwardInterception 檢查並處理 forward 相關命令。回傳 true 表示已處理。
func (t *pairTab) handleForwardInterception(ctx context.Context, conn net.Conn, cmd string, openCh openChannelFunc) bool {
	if fc := parseForwardCmd(cmd); fc != nil {
		t.handleForward(ctx, conn, fc, openCh)
		return true
	}

	if spec, ok := parseKillForwardCmd(cmd); ok {
		t.handleKillForward(conn, spec)
		return true
	}

	if isKillForwardAll(cmd) {
		t.handleKillForwardAll(conn)
		return true
	}

	if isListForward(cmd) {
		t.handleListForward(conn)
		return true
	}

	return false
}

// handleForward 攔截 adb forward 命令：在本機建立 TCP listener，
// 每個進入的連線透過 DataChannel 轉發到遠端設備的指定服務。
// 支援 tcp:0（自動分配 port，回傳實際 port 給 adb client）。
// 相同 localSpec 的舊 listener 會被自動替換（除非指定 norebind）。
func (t *pairTab) handleForward(ctx context.Context, conn net.Conn, fc *fwdCmd, openCh openChannelFunc) {
	port, err := parseLocalSpec(fc.localSpec)
	if err != nil {
		writeADBOkay(conn)
		writeADBFail(conn, err.Error())
		return
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		writeADBOkay(conn)
		writeADBFail(conn, fmt.Sprintf("cannot bind: %v", err))
		return
	}

	actualPort := ln.Addr().(*net.TCPAddr).Port

	serial, ok := t.resolveForwardSerial(fc.serial)
	if !ok {
		ln.Close()
		writeADBOkay(conn)
		writeADBFail(conn, "cannot resolve target device serial")
		return
	}

	fwdCtx, fwdCancel := context.WithCancel(ctx)

	fl := &fwdListener{
		ln:         ln,
		serial:     serial,
		localSpec:  fc.localSpec,
		remoteSpec: fc.remoteSpec,
		cancel:     fwdCancel,
	}

	t.fwdMu.Lock()
	if t.fwdListeners == nil {
		t.fwdListeners = make(map[string]*fwdListener)
	}
	// 關閉相同 localSpec 的舊 listener
	if old, ok := t.fwdListeners[fc.localSpec]; ok {
		old.cancel()
		old.ln.Close()
	}
	// tcp:0 要用實際 port 作為 key
	key := fc.localSpec
	if fc.localSpec == "tcp:0" {
		key = fmt.Sprintf("tcp:%d", actualPort)
		fl.localSpec = key
	}
	t.fwdListeners[key] = fl
	t.fwdMu.Unlock()

	go t.fwdAcceptLoop(fwdCtx, fl, openCh)

	// 回應：OKAY (host ack) + OKAY (forward success)
	writeADBOkay(conn)
	if fc.localSpec == "tcp:0" {
		// tcp:0 額外回傳分配的 port
		portStr := fmt.Sprintf("%d", actualPort)
		writeADBOkay(conn)
		fmt.Fprintf(conn, "%04x%s", len(portStr), portStr)
	} else {
		writeADBOkay(conn)
	}

	slog.Debug("forward 已建立", "local", key, "remote", fc.remoteSpec, "serial", serial, "port", actualPort)
}

// resolveForwardSerial 將 adb forward 命令中的 serial 映射為遠端真實裝置 serial。
//
// 映射邏輯：
//  1. 若 requested 完全匹配某個在線設備的 serial → 直接使用
//  2. 若只有一台在線設備 → 自動映射（不管 requested 是什麼值）
//  3. 其他情況 → 失敗（無法確定目標設備）
//
// 典型案例：本機 adb 認為設備序號是 127.0.0.1:<proxyPort>（因為是 adb connect 建立的），
// 但遠端設備的真實序號可能是 "emulator-5554" 或 USB 序號。
// 在只有一台設備的常見場景下，自動映射讓使用者無需手動指定。
func (t *pairTab) resolveForwardSerial(requested string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	devs := make([]ctrlDevice, 0, len(t.cliDevices))
	for _, d := range t.cliDevices {
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
			slog.Debug("forward serial 映射", "from", requested, "to", devs[0].Serial)
		}
		return devs[0].Serial, true
	}

	return "", false
}

// fwdAcceptLoop 持續接受 forward listener 的 TCP 連線，
// 每個連線由 handleFwdConn 在獨立 goroutine 中處理。
func (t *pairTab) fwdAcceptLoop(ctx context.Context, fl *fwdListener, openCh openChannelFunc) {
	var connID atomic.Int64
	for {
		conn, err := fl.ln.Accept()
		if err != nil {
			return
		}
		id := connID.Add(1)
		go t.handleFwdConn(ctx, conn, fl, openCh, id)
	}
}

// handleFwdConn 處理單一 forward 連線。
// 建立 DataChannel（label=adb-fwd/{id}/{serial}/{remoteSpec}），
// 遠端的 handleADBForwardConn 會連線到 ADB server → 目標設備 → 指定服務，
// 然後雙向橋接 DataChannel ↔ 本機 TCP 連線。
func (t *pairTab) handleFwdConn(ctx context.Context, conn net.Conn, fl *fwdListener, openCh openChannelFunc, id int64) {
	defer conn.Close()

	// DataChannel label: adb-fwd/{id}/{serial}/{remoteSpec}
	label := fmt.Sprintf("adb-fwd/%d/%s/%s", id, fl.serial, fl.remoteSpec)
	slog.Debug("forward 連線", "id", id, "local", fl.localSpec, "remote", fl.remoteSpec)
	ch, err := openCh(label)
	if err != nil {
		slog.Debug("forward DataChannel 建立失敗", "label", label, "error", err)
		return
	}
	defer ch.Close()

	biCopy(ctx, ch, conn)
}

// handleKillForward 處理 killforward 命令。
func (t *pairTab) handleKillForward(conn net.Conn, localSpec string) {
	t.fwdMu.Lock()
	fl, ok := t.fwdListeners[localSpec]
	if ok {
		fl.cancel()
		fl.ln.Close()
		delete(t.fwdListeners, localSpec)
	}
	t.fwdMu.Unlock()

	writeADBOkay(conn) // host ack
	if ok {
		writeADBOkay(conn)
	} else {
		writeADBFail(conn, fmt.Sprintf("listener '%s' not found", localSpec))
	}
}

// handleKillForwardAll 處理 killforward-all 命令。
func (t *pairTab) handleKillForwardAll(conn net.Conn) {
	t.fwdMu.Lock()
	for key, fl := range t.fwdListeners {
		fl.cancel()
		fl.ln.Close()
		delete(t.fwdListeners, key)
	}
	t.fwdMu.Unlock()

	writeADBOkay(conn)
	writeADBOkay(conn)
}

// handleListForward 處理 list-forward 命令。
func (t *pairTab) handleListForward(conn net.Conn) {
	t.fwdMu.Lock()
	var lines []string
	for _, fl := range t.fwdListeners {
		lines = append(lines, fmt.Sprintf("%s %s %s", fl.serial, fl.localSpec, fl.remoteSpec))
	}
	t.fwdMu.Unlock()

	list := strings.Join(lines, "\n")
	if len(lines) > 0 {
		list += "\n"
	}

	writeADBOkay(conn)
	fmt.Fprintf(conn, "%04x%s", len(list), list)
}

// --- Reverse Forward 管理（由 transport bridge 的 handleReverseOPEN 呼叫）---
// 注意：目前 reverse:forward: 回傳 FAIL 讓工具回退到 forward 模式（見 adb_transport.go），
// 因此 setupReverseForward 僅在未來支援 reverse forward 時使用。

// setupReverseForward 在客戶端建立 forward listener 來模擬 reverse forward。
// 原始的 reverse forward（設備 → 主機）在 P2P 架構下無法運作，
// 因為設備端的連線會到達遠端機器而非客戶端。
// 本函式將 reverse forward 轉換為等效的 forward（客戶端 → 設備），
// remoteSpec 是設備端的目標（如 localabstract:scrcpy），
// localSpec 是客戶端的本機 port（如 tcp:27183 或 tcp:0）。
func (t *pairTab) setupReverseForward(ctx context.Context, serial, localSpec, remoteSpec string, openCh openChannelFunc) (int, error) {
	port, err := parseLocalSpec(localSpec)
	if err != nil {
		return 0, err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return 0, fmt.Errorf("cannot bind: %v", err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	fwdCtx, fwdCancel := context.WithCancel(ctx)
	fl := &fwdListener{
		ln:         ln,
		serial:     serial,
		localSpec:  localSpec,
		remoteSpec: remoteSpec,
		cancel:     fwdCancel,
	}

	t.fwdMu.Lock()
	if t.fwdListeners == nil {
		t.fwdListeners = make(map[string]*fwdListener)
	}
	key := localSpec
	if localSpec == "tcp:0" {
		key = fmt.Sprintf("tcp:%d", actualPort)
		fl.localSpec = key
	}
	if old, ok := t.fwdListeners[key]; ok {
		old.cancel()
		old.ln.Close()
	}
	t.fwdListeners[key] = fl
	t.fwdMu.Unlock()

	go t.fwdAcceptLoop(fwdCtx, fl, openCh)

	slog.Debug("reverse forward 已建立（轉為客戶端 forward）",
		"local", key, "remote", remoteSpec, "serial", serial, "port", actualPort)
	return actualPort, nil
}

// killReverseForward 移除指定 remoteSpec 的 reverse forward。
func (t *pairTab) killReverseForward(remoteSpec string) bool {
	t.fwdMu.Lock()
	defer t.fwdMu.Unlock()

	for key, fl := range t.fwdListeners {
		if fl.remoteSpec == remoteSpec {
			fl.cancel()
			fl.ln.Close()
			delete(t.fwdListeners, key)
			return true
		}
	}
	return false
}

// killReverseForwardAll 移除所有 reverse forward listeners。
func (t *pairTab) killReverseForwardAll() {
	t.fwdMu.Lock()
	defer t.fwdMu.Unlock()

	for key, fl := range t.fwdListeners {
		fl.cancel()
		fl.ln.Close()
		delete(t.fwdListeners, key)
	}
}

// listReverseForwards 回傳 reverse forward 清單（ADB 格式）。
func (t *pairTab) listReverseForwards() string {
	t.fwdMu.Lock()
	defer t.fwdMu.Unlock()

	var lines []string
	for _, fl := range t.fwdListeners {
		lines = append(lines, fmt.Sprintf("%s %s %s", fl.serial, fl.remoteSpec, fl.localSpec))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// closeFwdListeners 關閉所有 forward listeners 並清空 map。
// 在 pairTab.cleanup() 中呼叫，確保 disconnect 時釋放所有 port。
func (t *pairTab) closeFwdListeners() {
	t.fwdMu.Lock()
	for key, fl := range t.fwdListeners {
		fl.cancel()
		fl.ln.Close()
		delete(t.fwdListeners, key)
	}
	t.fwdListeners = nil
	t.fwdMu.Unlock()
}

// --- 被控端（伺服器端）Forward 處理 ---
// 被控端收到 adb-fwd DataChannel 後，連線到本機 ADB server 並轉發到設備服務。

// handleADBForwardConn 處理來自客戶端的 forward DataChannel 連線。
// 流程：連線本機 ADB server → host:transport:<serial>（切換到目標設備）→
// 發送 remoteSpec（如 localabstract:scrcpy）→ 雙向橋接 DataChannel ↔ ADB 連線。
func (t *pairTab) handleADBForwardConn(ctx context.Context, rwc io.ReadWriteCloser, adbAddr, serial, remoteSpec string) {
	defer rwc.Close()

	conn, err := net.Dial("tcp", adbAddr)
	if err != nil {
		slog.Debug("forward: 連線 ADB server 失敗", "error", err)
		return
	}
	defer conn.Close()

	// 切換到目標設備
	if err := sendADBCmd(conn, fmt.Sprintf("host:transport:%s", serial)); err != nil {
		slog.Debug("forward: 發送 transport 失敗", "error", err)
		return
	}
	if err := readADBStatus(conn); err != nil {
		slog.Debug("forward: transport 失敗", "serial", serial, "error", err)
		return
	}

	// 連線到 remote spec
	if err := sendADBCmd(conn, remoteSpec); err != nil {
		slog.Debug("forward: 發送 remote spec 失敗", "error", err)
		return
	}
	if err := readADBStatus(conn); err != nil {
		slog.Debug("forward: remote spec 失敗", "remoteSpec", remoteSpec, "error", err)
		return
	}

	// 雙向轉發（biCopy 結束時關閉雙方，避免死鎖）
	biCopy(ctx, rwc, conn)
}
