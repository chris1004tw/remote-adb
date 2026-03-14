// transport.go 實作 ADB device transport 二進位協定的多工橋接。
//
// 本檔案為 bridge 套件的 transport 層，負責處理 ADB device transport 的
// 多工串流管理、以及 DataChannel/TCP 的分塊傳輸。
// ADB 協定常數與訊息解析見 transport_protocol.go。
// 由 GUI 和 CLI 共用，不依賴任何 GUI 框架。
//
// # 多工串流設計（deviceBridge + dStream）
//
// 一條 TCP 連線（device transport）上可同時有多個服務串流（shell、push、sync 等）。
// deviceBridge 為每個 OPEN 命令透過 OpenChannelFunc 建立一條獨立的通道
// （WebRTC DataChannel 或 TCP 連線），實現多工轉發：
//
//	ADB client <-(transport)-> deviceBridge <-(DataChannel/TCP)-> 遠端 handleADBStreamConn
//
// 每個 dStream 有三個 goroutine 協作：
//   - setupStream -> readFromRemote：遠端 -> transport（WRTE），等 OKAY 後才送下一筆
//   - writeToRemote：transport -> 遠端，寫完回 OKAY
//   - 主迴圈 handleWRTE：將 WRTE 資料放入 writeCh（非阻塞），由 writeToRemote 消費
//
// # 16KB 分塊寫入限制
//
// WebRTC DataChannel（SCTP）的訊息大小有限制。chunkedWrite/ChunkedCopy 將大筆資料
// 切成 16KB（BiCopyChunk）的片段逐一寫入，避免超過 DataChannel 的最大訊息大小。
package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// transport.go 專用常數（不屬於 ADB 協定層，而是本地 I/O 策略）。
const (
	readBufSize          = 64 * 1024        // 64KB：readFromRemote 讀取 buffer（DC 實際單次讀取遠小於 256KB）
	streamReadyTimeout   = 45 * time.Second // 遠端 stream ready 訊號等待上限（適配高延遲/TURN）
	wrteOkayTimeout      = 30 * time.Second // 單筆 WRTE 等待 OKAY 上限（避免高 RTT 下誤判逾時）
	toRemoteWriteTimeout = 20 * time.Second // host->device 寫入 DC 上限（避免 sync 上傳無限卡住）
)

// --- deviceBridge：ADB device transport 多工橋接 ---
//
// 設計重點：
//   - 主迴圈（transport 讀取）絕不阻塞：所有 DC 寫入由 per-stream goroutine 處理
//   - 每個 stream 有三個 goroutine：setupStream（->readFromRemote）、writeToRemote
//   - cleanupStream 透過 atomic.Bool 保證只執行一次，避免 double CLSE
//   - handleWRTE 將資料放入 writeCh，由 writeToRemote 寫入 DC 後才回 OKAY（維持流控）

// deviceBridge 管理一條 ADB device transport 連線上的所有多工串流。
// 主迴圈在 StartDeviceTransport 中執行，負責讀取 transport 訊息並分派給對應的 handler。
// writeMu 保護對 conn 的並行寫入（主迴圈和 per-stream goroutine 都可能寫入）。
type deviceBridge struct {
	conn   net.Conn              // 底層 TCP 連線（ADB device transport）
	openCh OpenChannelFunc       // 建立遠端通道的函式（DataChannel 或 TCP）
	serial string                // 目標設備序號
	rfm    ReverseForwardManager // 用於 reverse forward 管理（nil = 不支援 reverse forward）

	writeMu   sync.Mutex          // 保護 conn 的並行寫入
	streamsMu sync.Mutex          // 保護 streams map 的並行存取
	streams   map[uint32]*dStream // 活躍串流，key 為我方分配的 deviceID
	nextID    atomic.Uint32       // 遞增的 deviceID 分配器

	// localAbstractPrev 追蹤每個 localabstract 路徑的前一個連線完成信號。
	// 用於序列化同一 abstract socket 的 DataChannel 建立順序，
	// 確保遠端 agent 的 accept() 順序與客戶端 OPEN 順序一致。
	// 背景：scrcpy 等工具透過同一 abstract socket 開啟多條連線（video/audio/control），
	// 依靠 accept() 順序對應功能。DataChannel 非同步到達可能導致串流交叉。
	// key = service 名稱（如 "localabstract:scrcpy_XXXX"），value = 前次就緒完成的信號。
	localAbstractPrev map[string]<-chan struct{}
}

// dStream 追蹤一條多工串流的完整狀態。
//
// 生命週期：handleOPEN -> setupStream（等就緒 -> 註冊 stream -> 啟動 goroutine）->
// readFromRemote/writeToRemote 並行運作 -> cleanupStream（關閉 ch + doneCh + 送 CLSE）。
//
// 流控機制：
//   - host->device（WRTE）：handleWRTE 放入 writeCh -> writeToRemote 寫入 DC -> 回 OKAY
//   - device->host（WRTE）：readFromRemote 讀 DC -> 送 WRTE -> 等 ready（OKAY）-> 繼續
type dStream struct {
	serverID uint32             // ADB server 端分配的 stream ID（來自 OPEN 的 arg0）
	deviceID uint32             // 我方分配的 stream ID（由 nextID 遞增產生）
	ch       io.ReadWriteCloser // 遠端通道（DataChannel 或 TCP）
	ready    chan struct{}      // cap=1: 收到 OKAY 後發信號，允許 readFromRemote 繼續送 WRTE
	writeCh  chan []byte        // cap=4: host -> device 的資料佇列，由 writeToRemote 消費
	doneCh   chan struct{}      // cleanup 時 close，通知 writeToRemote / readFromRemote 退出
	closed   atomic.Bool        // CompareAndSwap 保證 cleanupStream 只執行一次
}

// writeMsg 以 mutex 保護將 adbMsg 寫入 transport 連線。
// 多個 goroutine（主迴圈 + per-stream）可能同時呼叫，因此需要串行化。
func (b *deviceBridge) writeMsg(msg *adbMsg) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return writeADBTransportMsg(b.conn, msg)
}

// cleanupStream 清理串流（透過 CompareAndSwap 保證只執行一次）。
func (b *deviceBridge) cleanupStream(stream *dStream) {
	if !stream.closed.CompareAndSwap(false, true) {
		return
	}
	b.streamsMu.Lock()
	delete(b.streams, stream.deviceID)
	b.streamsMu.Unlock()

	stream.ch.Close()
	close(stream.doneCh) // 通知 writeToRemote / readFromRemote 退出

	if err := b.writeMsg(&adbMsg{command: aCLSE, arg0: stream.deviceID, arg1: stream.serverID}); err != nil {
		slog.Debug("transport -> CLSE write failed", "deviceID", stream.deviceID, "error", err)
	} else {
		slog.Debug("transport -> CLSE", "deviceID", stream.deviceID, "serverID", stream.serverID)
	}
}

// StartDeviceTransport 處理完整的 ADB device transport 連線（來自 `adb connect`）。
//
// 參數：
//   - firstBytes：handleProxyConn 已讀取的前 4 bytes（"CNXN"）
//   - openCh：建立遠端通道的函式（WebRTC DataChannel 或 TCP）
//   - serial/features：目標設備的序號和 feature 清單
//   - rfm：ReverseForwardManager 指標，用於 reverse forward（可為 nil）
//
// 流程：
//  1. 讀取完整 CNXN 訊息
//  2. 回應 CNXN（banner 攜帶設備 features，跳過 AUTH）
//  3. 進入主迴圈：持續讀取 transport 訊息並分派給 handleOPEN/handleOKAY/handleWRTE/handleCLSE
//  4. 結束時清理所有殘留的 streams
func StartDeviceTransport(ctx context.Context, conn net.Conn, firstBytes []byte, openCh OpenChannelFunc, serial, features string, rfm ReverseForwardManager) {
	cnxn, err := readADBMsgFromPrefix(firstBytes, conn)
	if err != nil {
		slog.Debug("failed to read CNXN", "error", err)
		return
	}

	slog.Debug("received ADB transport CNXN",
		"version", fmt.Sprintf("0x%08x", cnxn.arg0),
		"maxdata", cnxn.arg1,
		"banner", string(cnxn.data))

	if serial == "" {
		slog.Debug("no available remote device, rejecting CNXN")
		return
	}

	bridge := &deviceBridge{
		conn:              conn,
		openCh:            openCh,
		serial:            serial,
		rfm:               rfm,
		streams:           make(map[uint32]*dStream),
		localAbstractPrev: make(map[string]<-chan struct{}),
	}
	bridge.nextID.Store(1)

	// transport 結束時清理所有殘留 streams
	defer func() {
		bridge.streamsMu.Lock()
		remaining := make([]*dStream, 0, len(bridge.streams))
		for _, s := range bridge.streams {
			remaining = append(remaining, s)
		}
		bridge.streamsMu.Unlock()
		for _, s := range remaining {
			bridge.cleanupStream(s)
		}
		slog.Debug("device transport closed", "serial", serial)
	}()

	// 回應 CNXN（跳過 AUTH，localhost 信任）
	var banner string
	if features != "" {
		banner = "device::features=" + features + "\x00"
	} else {
		banner = defaultDeviceBanner + "\x00"
	}
	if err := bridge.writeMsg(&adbMsg{
		command: aCNXN,
		arg0:    aVersion,
		arg1:    aMaxPayload,
		data:    []byte(banner),
	}); err != nil {
		slog.Debug("failed to send CNXN response", "error", err)
		return
	}

	slog.Debug("device transport established", "serial", serial)

	// 主迴圈：讀取 ADB server 的 transport 訊息
	for {
		msg, err := readADBTransportMsg(conn)
		if err != nil {
			if ctx.Err() != nil {
				slog.Debug("transport ended due to context cancellation", "serial", serial)
			} else {
				slog.Debug("transport read ended", "serial", serial, "error", err)
			}
			return
		}

		switch msg.command {
		case aOPEN:
			bridge.handleOPEN(ctx, msg)
		case aOKAY:
			bridge.handleOKAY(msg)
		case aWRTE:
			bridge.handleWRTE(msg)
		case aCLSE:
			bridge.handleCLSE(msg)
		default:
			slog.Debug("transport: unknown command", "cmd", adbCmdName(msg.command))
		}
	}
}

// handleOPEN 處理 OPEN 命令：為新的 adb 服務建立遠端通道。
// 特殊處理：reverse:* 命令攔截到 handleReverseOPEN（客戶端本地處理）。
// 所有服務（含 localabstract: 和一般服務）的 DataChannel 建立（openCh）
// 均在 goroutine 中非同步執行，避免阻塞主迴圈的 OKAY/WRTE/CLSE 處理。
// localabstract: 額外透過 done-channel 鏈序列化同路徑的連線順序。
func (b *deviceBridge) handleOPEN(ctx context.Context, msg *adbMsg) {
	serverID := msg.arg0
	service := strings.TrimRight(string(msg.data), "\x00")
	deviceID := b.nextID.Add(1)

	slog.Debug("transport <- OPEN", "serverID", serverID, "deviceID", deviceID, "service", service)

	// 攔截 reverse: 命令，在客戶端本地處理
	if strings.HasPrefix(service, "reverse:") {
		b.handleReverseOPEN(ctx, serverID, deviceID, service)
		return
	}

	// localabstract: 服務需序列化 DataChannel 建立與 setupStream。
	// scrcpy 等工具透過同一 abstract socket 開啟多條連線（video/audio/control），
	// 依靠 accept() 順序對應功能。若 DataChannel 非同步到達導致亂序，
	// 會造成串流交叉（如音訊資料被當成控制訊息）。
	// 序列化確保：前一條連線完成就緒信號後，才建立下一條的 DataChannel。
	if strings.HasPrefix(service, "localabstract:") {
		prevDone := b.localAbstractPrev[service]
		done := make(chan struct{})
		b.localAbstractPrev[service] = done

		go func() {
			var doneOnce sync.Once
			signalDone := func() { doneOnce.Do(func() { close(done) }) }
			defer signalDone() // 安全網：確保任何路徑都會解除後續等待

			// 等待前一條同路徑連線的就緒階段完成
			if prevDone != nil {
				select {
				case <-prevDone:
				case <-ctx.Done():
					return
				}
			}

			label := fmt.Sprintf("adb-stream/%d/%s/%s", deviceID, b.serial, service)
			ch, err := b.openCh(label)
			if err != nil {
				slog.Debug("OPEN: DataChannel creation failed", "deviceID", deviceID, "error", err)
				b.writeMsg(&adbMsg{command: aCLSE, arg0: 0, arg1: serverID})
				return
			}

			b.setupStream(ctx, ch, serverID, deviceID, signalDone)
		}()
		return
	}

	// 非同步建立 DataChannel 並等待遠端就緒，避免阻塞主迴圈。
	// openCh（DataChannel 建立）可能耗時數百毫秒，若同步執行會卡住
	// 主迴圈對其他 stream 的 OKAY/WRTE/CLSE 處理，
	// 導致高頻連線的工具（如 UIAutomator 的 tcp:9008 HTTP 請求）timeout。
	go func() {
		label := fmt.Sprintf("adb-stream/%d/%s/%s", deviceID, b.serial, service)
		ch, err := b.openCh(label)
		if err != nil {
			slog.Debug("OPEN: DataChannel creation failed", "deviceID", deviceID, "error", err)
			b.writeMsg(&adbMsg{command: aCLSE, arg0: 0, arg1: serverID})
			return
		}
		b.setupStream(ctx, ch, serverID, deviceID, nil)
	}()
}

// handleReverseOPEN 攔截 reverse:* 命令，在客戶端本地處理（不轉發到遠端）。
//
// reverse:forward: 的特殊處理：
// 在 P2P 架構下，設備端的 reverse forward 會連線到遠端機器而非客戶端，
// 因此回傳 FAIL 讓工具（如 scrcpy v2）自動回退到 adb forward 模式。
// scrcpy v2 預設嘗試 reverse -> 失敗後自動改用 forward，所以這不影響功能。
//
// 其他 reverse 命令（killforward/killforward-all/list-forward）仍正常處理。
func (b *deviceBridge) handleReverseOPEN(ctx context.Context, serverID, deviceID uint32, service string) {
	// 計算回應資料
	var respData []byte
	switch {
	case strings.HasPrefix(service, "reverse:forward:"):
		// P2P 架構下 reverse forward 無法正確運作（設備端反向連線會到遠端機器而非客戶端），
		// 回傳 FAIL 讓工具（如 scrcpy）自動回退到 adb forward 模式。
		msg := "reverse forward not supported via P2P bridge"
		respData = []byte(fmt.Sprintf("FAIL%04x%s", len(msg), msg))
		slog.Debug("reverse:forward: returning FAIL, client will fallback to forward mode", "service", service)

	case strings.HasPrefix(service, "reverse:killforward:"):
		spec := service[len("reverse:killforward:"):]
		if b.rfm != nil && b.rfm.KillReverseForward(spec) {
			respData = []byte("OKAY")
		} else {
			msg := fmt.Sprintf("listener '%s' not found", spec)
			respData = []byte(fmt.Sprintf("FAIL%04x%s", len(msg), msg))
		}

	case service == "reverse:killforward-all":
		if b.rfm != nil {
			b.rfm.KillReverseForwardAll()
		}
		respData = []byte("OKAY")

	case service == "reverse:list-forward":
		var list string
		if b.rfm != nil {
			list = b.rfm.ListReverseForwards()
		}
		respData = []byte(fmt.Sprintf("OKAY%04x%s", len(list), list))

	default:
		slog.Debug("unknown reverse command", "service", service)
		b.writeMsg(&adbMsg{command: aCLSE, arg0: 0, arg1: serverID})
		return
	}

	// 建立臨時 stream 以走完 OKAY -> WRTE -> (wait OKAY) -> CLSE 流程
	go b.sendOneShot(serverID, deviceID, respData)
}

// sendOneShot 為一次性回應（如 reverse forward 結果）建立臨時 stream。
// 流程：註冊 stream -> 送 OKAY（接受 stream）-> 送 WRTE（回應資料）->
// 等 ADB server 的 OKAY（5 秒逾時）-> cleanupStream（送 CLSE 關閉）。
// 使用 nopRWC 作為 ch，因為不需要真正的遠端通道。
func (b *deviceBridge) sendOneShot(serverID, deviceID uint32, data []byte) {
	stream := &dStream{
		serverID: serverID,
		deviceID: deviceID,
		ch:       nopRWC{},
		ready:    make(chan struct{}, 1),
		writeCh:  make(chan []byte, 4),
		doneCh:   make(chan struct{}),
	}

	b.streamsMu.Lock()
	b.streams[deviceID] = stream
	b.streamsMu.Unlock()

	// transport OKAY（接受 stream）
	if err := b.writeMsg(&adbMsg{command: aOKAY, arg0: deviceID, arg1: serverID}); err != nil {
		slog.Debug("sendOneShot: OKAY failed", "deviceID", deviceID, "error", err)
		b.cleanupStream(stream)
		return
	}

	// WRTE（smart socket 回應）
	if err := b.writeMsg(&adbMsg{command: aWRTE, arg0: deviceID, arg1: serverID, data: data}); err != nil {
		slog.Debug("sendOneShot: WRTE failed", "deviceID", deviceID, "error", err)
		b.cleanupStream(stream)
		return
	}

	// 等 ADB server 的 OKAY（確認收到 WRTE），然後 CLSE
	oneshotTimer := time.NewTimer(5 * time.Second)
	defer oneshotTimer.Stop()
	select {
	case <-stream.ready:
	case <-stream.doneCh:
		return // ADB server 先 CLSE 了
	case <-oneshotTimer.C:
		slog.Debug("sendOneShot: OKAY timeout", "deviceID", deviceID)
	}

	b.cleanupStream(stream)
}

// setupStream 等待遠端就緒信號（1 byte），成功後啟動 readFromRemote/writeToRemote。
//
// 就緒信號協定：遠端 handleADBStreamConn 完成 transport + service 命令後，
// 寫入 1 byte（1=成功, 0=失敗）。成功後資料雙向流動。
//
// onSetup 為可選回調（可為 nil）：在就緒信號處理完成後、readFromRemote 啟動前呼叫。
// 用於 localabstract 連線序列化——通知下一條等待中的連線可以開始建立 DataChannel。
//
// 特別注意（pion/datachannel detach 模式的陷阱）：
// 首次 Read 的 buffer 必須 >= 4 bytes，因為 pion 的 ReadDataChannel 會在
// 內部消費 DCEP ACK（4 bytes），buffer 太小會得到 io.ErrShortBuffer。
// 若就緒信號和第一筆資料黏在同一個 SCTP 訊息中，多餘的 bytes 會用
// PrefixedRWC 包裝保留，避免掉包。
func (b *deviceBridge) setupStream(ctx context.Context, ch io.ReadWriteCloser, serverID, deviceID uint32, onSetup func()) {
	// 等待遠端就緒信號（1 byte: 1=成功, 0=失敗）
	// 注意：buffer 必須 >= 4 bytes，因為 pion/datachannel detach 模式下
	// 首次 Read 需要容納 DCEP ACK（4 bytes），ReadDataChannel 內部會消費它後
	// 才返回使用者資料。buffer 太小會導致 io.ErrShortBuffer。
	type readyResult struct {
		ready  bool
		prefix []byte
	}
	readyCh := make(chan readyResult, 1)
	go func() {
		var buf [4]byte
		n, err := ch.Read(buf[:])
		if err != nil {
			slog.Debug("setupStream: ready read failed", "deviceID", deviceID, "error", err, "n", n)
		}
		res := readyResult{ready: err == nil && n >= 1 && buf[0] == 1}
		if res.ready && n > 1 {
			res.prefix = append([]byte(nil), buf[1:n]...)
		}
		readyCh <- res
	}()

	ready := false
	readyTimer := time.NewTimer(streamReadyTimeout)
	defer readyTimer.Stop()
	select {
	case res := <-readyCh:
		ready = res.ready
		if ready && len(res.prefix) > 0 {
			// 保留就緒訊號後緊接的資料，避免掉首包。
			ch = &PrefixedRWC{Ch: ch, Prefix: res.prefix}
			slog.Debug("setupStream: retained prefix data", "deviceID", deviceID, "bytes", len(res.prefix))
		}
		slog.Debug("setupStream: received ready signal", "deviceID", deviceID, "ready", ready)
	case <-readyTimer.C:
		slog.Debug("setupStream: ready wait timeout", "deviceID", deviceID)
	case <-ctx.Done():
		slog.Debug("setupStream: context cancelled", "deviceID", deviceID)
		ch.Close()
		if onSetup != nil {
			onSetup()
		}
		return
	}

	// 通知序列化鏈：本連線的就緒階段已完成（無論成功或失敗）。
	// 這讓下一條等待中的 localabstract 連線可以開始建立 DataChannel。
	if onSetup != nil {
		onSetup()
	}

	if !ready {
		slog.Debug("setupStream: remote ready failed", "deviceID", deviceID, "serverID", serverID)
		ch.Close()
		if err := b.writeMsg(&adbMsg{command: aCLSE, arg0: 0, arg1: serverID}); err != nil {
			slog.Debug("setupStream: CLSE write failed", "deviceID", deviceID, "error", err)
		}
		return
	}

	stream := &dStream{
		serverID: serverID,
		deviceID: deviceID,
		ch:       ch,
		ready:    make(chan struct{}, 1),
		writeCh:  make(chan []byte, 4),
		doneCh:   make(chan struct{}),
	}

	b.streamsMu.Lock()
	b.streams[deviceID] = stream
	b.streamsMu.Unlock()

	// 先啟動 writeToRemote，確保 OKAY 送出前就能接收 WRTE
	go b.writeToRemote(stream)

	// 回應 OKAY 給 ADB server
	if err := b.writeMsg(&adbMsg{command: aOKAY, arg0: deviceID, arg1: serverID}); err != nil {
		slog.Debug("setupStream: OKAY write failed", "deviceID", deviceID, "error", err)
		b.cleanupStream(stream)
		return
	}

	slog.Debug("transport -> OKAY (OPEN)", "deviceID", deviceID, "serverID", serverID)

	// DC -> transport（WRTE），在此 goroutine 直接執行
	b.readFromRemote(ctx, stream)
}

// writeToRemote 是 per-stream 的寫入 goroutine：
// 從 writeCh 讀取 WRTE 資料 -> chunkedWrite 到 DataChannel -> 回 OKAY 給 ADB server。
// 設計要點：主迴圈的 handleWRTE 只做非阻塞的 channel send，實際的 DC 寫入和
// OKAY 回應在這裡完成，確保主迴圈永遠不阻塞。
func (b *deviceBridge) writeToRemote(stream *dStream) {
	for {
		select {
		case data := <-stream.writeCh:
			setWriteDeadline(stream.ch, time.Now().Add(toRemoteWriteTimeout))
			if _, err := chunkedWrite(stream.ch, data, BiCopyChunk); err != nil {
				slog.Debug("writeToRemote: DC write failed", "deviceID", stream.deviceID, "error", err)
				setWriteDeadline(stream.ch, time.Time{})
				b.cleanupStream(stream)
				return
			}
			setWriteDeadline(stream.ch, time.Time{})
			// DC 寫入成功，回 OKAY 允許 host 繼續送 WRTE
			if err := b.writeMsg(&adbMsg{
				command: aOKAY,
				arg0:    stream.deviceID,
				arg1:    stream.serverID,
			}); err != nil {
				slog.Debug("writeToRemote: OKAY write failed", "deviceID", stream.deviceID, "error", err)
				b.cleanupStream(stream)
				return
			}
		case <-stream.doneCh:
			return
		}
	}
}

// setWriteDeadline 只在底層連線支援 deadline 時設定。
// detach 模式的 DataChannel 在支援時可避免 write 永久卡住。
func setWriteDeadline(w io.Writer, t time.Time) {
	if d, ok := w.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := d.SetWriteDeadline(t); err != nil {
			slog.Debug("set write deadline failed", "error", err)
		}
	}
}

// chunkedWrite 將單筆資料以 chunkSize（16KB）為單位切段寫入 dst。
// 避免單次寫入超過 WebRTC DataChannel（SCTP）的最大訊息大小限制。
func chunkedWrite(dst io.Writer, data []byte, chunkSize int) (int, error) {
	total := 0
	for len(data) > 0 {
		n := len(data)
		if n > chunkSize {
			n = chunkSize
		}
		written := 0
		for written < n {
			wn, err := dst.Write(data[written:n])
			total += wn
			if err != nil {
				return total, err
			}
			if wn == 0 {
				return total, io.ErrShortWrite
			}
			written += wn
		}
		data = data[n:]
	}
	return total, nil
}

// readFromRemote 是 per-stream 的讀取 goroutine：
// 從 DataChannel 讀取資料 -> 送 WRTE 到 ADB transport -> 等 OKAY -> 繼續讀取。
// 每次 WRTE 後必須等待 ADB server 回應 OKAY 才能送下一筆（流控協定）。
// 結束時呼叫 cleanupStream（idempotent，可安全重複呼叫）。
func (b *deviceBridge) readFromRemote(ctx context.Context, stream *dStream) {
	defer b.cleanupStream(stream) // idempotent

	slog.Debug("readFromRemote: started", "deviceID", stream.deviceID, "serverID", stream.serverID)

	buf := make([]byte, readBufSize)
	firstRead := true
	// 在迴圈外建立一次 Timer，迴圈內重用，避免每次迭代建立新 Timer 造成記憶體洩漏。
	// 高吞吐時每秒可能有數百次迭代，若使用 time.After 會累積大量未釋放的 30s Timer。
	okayTimer := time.NewTimer(wrteOkayTimeout)
	defer okayTimer.Stop()
	for {
		n, err := stream.ch.Read(buf)
		if n > 0 {
			if firstRead {
				slog.Debug("readFromRemote: first data", "deviceID", stream.deviceID, "bytes", n)
				firstRead = false
			}
			if writeErr := b.writeMsg(&adbMsg{
				command: aWRTE,
				arg0:    stream.deviceID,
				arg1:    stream.serverID,
				data:    buf[:n],
			}); writeErr != nil {
				slog.Debug("readFromRemote: WRTE write failed", "deviceID", stream.deviceID, "error", writeErr)
				return
			}
			// 重置 Timer：先 Stop + drain 殘留觸發事件，再 Reset
			if !okayTimer.Stop() {
				select {
				case <-okayTimer.C:
				default:
				}
			}
			okayTimer.Reset(wrteOkayTimeout)
			// 等待 ADB server 回應 OKAY 後才能繼續
			select {
			case <-stream.ready:
			case <-ctx.Done():
				return
			case <-stream.doneCh:
				return
			case <-okayTimer.C:
				slog.Debug("readFromRemote: WRTE OKAY timeout", "deviceID", stream.deviceID)
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.Debug("readFromRemote: DC read error", "deviceID", stream.deviceID, "error", err)
			} else {
				slog.Debug("readFromRemote: DC EOF", "deviceID", stream.deviceID)
			}
			return
		}
	}
}

// handleOKAY 處理 transport 收到的 OKAY 命令。
// OKAY 的 arg1（deviceID）指向我方的串流，通知 readFromRemote 可以繼續送 WRTE，
// 或通知 sendOneShot 可以結束。透過 ready channel 發信號。
func (b *deviceBridge) handleOKAY(msg *adbMsg) {
	deviceID := msg.arg1
	b.streamsMu.Lock()
	stream, ok := b.streams[deviceID]
	b.streamsMu.Unlock()
	if !ok {
		return
	}
	select {
	case stream.ready <- struct{}{}:
	default:
	}
}

// handleWRTE 處理 transport 收到的 WRTE 命令（host -> device 方向）。
// 將資料放入 per-stream 的 writeCh 佇列（非阻塞 select），
// 由 writeToRemote goroutine 消費寫入 DataChannel 後才回 OKAY。
// 這樣主迴圈永遠不會阻塞在 DC 寫入上，即使某條串流的 DC 暫時不可寫入。
func (b *deviceBridge) handleWRTE(msg *adbMsg) {
	serverID := msg.arg0
	deviceID := msg.arg1

	b.streamsMu.Lock()
	stream, ok := b.streams[deviceID]
	b.streamsMu.Unlock()

	if !ok {
		slog.Debug("transport <- WRTE: stream not found, sending CLSE", "deviceID", deviceID, "serverID", serverID)
		b.writeMsg(&adbMsg{command: aCLSE, arg0: deviceID, arg1: serverID})
		return
	}

	// 非阻塞放入佇列；若 stream 正在關閉，doneCh 會被觸發
	select {
	case stream.writeCh <- msg.data:
	case <-stream.doneCh:
	}
}

// handleCLSE 處理 CLSE 命令：關閉對應串流。
func (b *deviceBridge) handleCLSE(msg *adbMsg) {
	deviceID := msg.arg1
	slog.Debug("transport <- CLSE", "deviceID", deviceID, "serverID", msg.arg0)

	b.streamsMu.Lock()
	stream, ok := b.streams[deviceID]
	b.streamsMu.Unlock()

	if ok {
		b.cleanupStream(stream)
	}
}
