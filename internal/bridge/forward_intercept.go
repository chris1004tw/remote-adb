// forward_intercept.go 實作客戶端 forward 攔截、reverse forward 管理，
// 以及被控端的 forward DataChannel 處理。
//
// 客戶端攔截：HandleForwardInterception 分派 forward/killforward/list-forward 命令，
// HandleForward 在本機建立 TCP listener 並透過 DataChannel 轉發。
//
// Reverse forward：SetupReverseForward 將 reverse forward 轉換為客戶端的等效 forward。
//
// 被控端：HandleADBForwardConn 接收 DataChannel 並連線到本機 ADB → 設備服務。
package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"

	"github.com/chris1004tw/remote-adb/internal/adb"
)

// --- 客戶端 Forward 攔截 ---
// 以下函式在客戶端（主控端）的 ADB proxy listener 上運作，
// 攔截 forward 相關命令並在本機建立 listener，非 forward 命令則透過 DataChannel 轉發。

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

// setupFwdListener 封裝 forward listener 的共用建立流程：
// ParseLocalSpec → Listen → 建構 FwdListener → 註冊到 fwdListeners map → 啟動 accept loop。
// HandleForward 和 SetupReverseForward 共用此邏輯，各自處理 serial 解析和回應格式。
// 回傳實際分配的 port。
func (fm *ForwardManager) setupFwdListener(ctx context.Context, serial, localSpec, remoteSpec string, openCh OpenChannelFunc) (int, error) {
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
	return actualPort, nil
}

// HandleForward 攔截 adb forward 命令：在本機建立 TCP listener，
// 每個進入的連線透過 DataChannel 轉發到遠端設備的指定服務。
// 支援 tcp:0（自動分配 port，回傳實際 port 給 adb client）。
// 相同 LocalSpec 的舊 listener 會被自動替換（除非指定 norebind）。
func (fm *ForwardManager) HandleForward(ctx context.Context, conn net.Conn, fc *FwdCmd, openCh OpenChannelFunc) {
	serial, ok := fm.ResolveSerial(fc.Serial)
	if !ok {
		WriteADBOkay(conn)
		WriteADBFail(conn, "cannot resolve target device serial")
		return
	}

	actualPort, err := fm.setupFwdListener(ctx, serial, fc.LocalSpec, fc.RemoteSpec, openCh)
	if err != nil {
		WriteADBOkay(conn)
		WriteADBFail(conn, err.Error())
		return
	}

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

	slog.Debug("forward established", "local", fc.LocalSpec, "remote", fc.RemoteSpec, "serial", serial, "port", actualPort)
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
	actualPort, err := fm.setupFwdListener(ctx, serial, localSpec, remoteSpec, openCh)
	if err != nil {
		return 0, err
	}
	slog.Debug("reverse forward established (converted to client forward)",
		"local", localSpec, "remote", remoteSpec, "serial", serial, "port", actualPort)
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

// --- 被控端（伺服器端）Forward 處理 ---
// 被控端收到 adb-fwd DataChannel 後，連線到本機 ADB server 並轉發到設備服務。

// HandleADBForwardConn 處理來自客戶端的 forward DataChannel 連線。
// 流程：連線本機 ADB server → host:transport:<serial>（切換到目標設備）→
// 發送 remoteSpec（如 localabstract:scrcpy）→ 雙向橋接 DataChannel ↔ ADB 連線。
// 此函式為無狀態操作，不依賴任何 struct 欄位，由被控端的 ServerHandler 呼叫。
func HandleADBForwardConn(ctx context.Context, rwc io.ReadWriteCloser, adbAddr, serial, remoteSpec string) {
	defer rwc.Close()

	conn, err := adb.NewDialer(adbAddr).DialServiceWithRetry(ctx, serial, remoteSpec)
	if err != nil {
		slog.Debug("forward: DialService failed", "serial", serial, "remoteSpec", remoteSpec, "error", err)
		return
	}
	defer conn.Close()

	// 雙向轉發（BiCopy 結束時關閉雙方，避免死鎖）
	BiCopy(ctx, rwc, conn)
}
