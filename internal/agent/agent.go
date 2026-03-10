// Package agent 實作 radb 遠端代理端（Remote Agent）的核心邏輯。
//
// Agent 部署在掛載 Android 設備的遠端主機上，負責：
//   - 透過 ADB Tracker 持續追蹤本機連接的 Android 設備狀態變化
//   - 透過 WebSocket 連線至 Signal Server，同步設備清單與處理信令
//   - 接收 Client 端的 WebRTC Offer，建立 P2P 連線後將 ADB 流量橋接至本機設備
//   - 管理設備鎖定（Lock），確保同一時間只有一位 Client 操作單一設備
//
// Agent 支援兩種運作模式：
//   - Signal Server 模式（Run）：完整功能，透過 Server 做信令交換與設備發現
//   - Direct-only 模式（RunDirectOnly）：僅追蹤設備，搭配 directsrv 提供 LAN 直連
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
	ws "github.com/coder/websocket"
)

// Config 是 Agent 啟動時所需的完整設定。
type Config struct {
	ServerURL string // Signal Server 的 WebSocket 位址（不含 /ws 路徑，例如 "ws://example.com:8080"）
	Token     string // PSK 認證 token，需與 Server 端設定一致
	HostID    string // 人類可讀的主機識別名稱，用於 Client 端列表顯示
	ADBAddr   string // 本機 ADB server 的位址（例如 "127.0.0.1:5037"）
	ICEConfig webrtc.ICEConfig // WebRTC ICE 設定（STUN/TURN server 列表）
}

// Agent 是遠端代理端的核心結構，持有所有運行時狀態。
//
// Agent 的生命週期：New() 建立 → Run()/RunDirectOnly() 進入主迴圈 → context 取消時結束。
// 一個 Agent 實例同時只應執行一種模式（Signal Server 或 Direct-only）。
type Agent struct {
	config   Config // 啟動設定（建立後不變）
	hostname string // 本機主機名稱（os.Hostname），用於信令訊息的來源標識

	// 執行時狀態（Run() 啟動後才會填入）
	connID      string          // Server 分配的連線 ID，作為此 Agent 在信令系統中的唯一識別
	wsConn      *ws.Conn        // 與 Signal Server 的 WebSocket 持久連線
	deviceTable *adb.DeviceTable // 設備狀態表，記錄所有已連接設備及其鎖定狀態（執行緒安全）
	dialer      *adb.Dialer     // ADB 連線撥號器，負責建立到指定設備的 TCP 連線
}

// New 建立一個新的 Agent 實例。
// 此時僅初始化設備表與 ADB 撥號器，尚未連線 Server 或開始追蹤設備。
func New(cfg Config) *Agent {
	h, _ := os.Hostname()
	return &Agent{
		config:      cfg,
		hostname:    h,
		deviceTable: adb.NewDeviceTable(),
		dialer:      adb.NewDialer(cfg.ADBAddr),
	}
}

// DeviceTable 回傳設備狀態表（供 directsrv 共享使用）。
func (a *Agent) DeviceTable() *adb.DeviceTable {
	return a.deviceTable
}

// Dialer 回傳 ADB 撥號器（供 directsrv 共享使用）。
func (a *Agent) Dialer() *adb.Dialer {
	return a.dialer
}

// Run 執行 Agent 完整主迴圈（Signal Server 模式）。
//
// 流程：
//  1. 透過 WebSocket 連線至 Signal Server 並完成 PSK 認證
//  2. 啟動 ADB Tracker 持續監聽設備插拔事件
//  3. 向 Server 註冊自身（含初始設備清單）
//  4. 進入 select 主迴圈，同時處理：
//     - ADB 設備變化事件 → 更新設備表並通知 Server
//     - Server 信令訊息 → 處理 lock/unlock/offer 請求
//
// 與 RunDirectOnly 的差異：Run 需要 Signal Server 連線，支援完整的
// 設備發現、鎖定、WebRTC 信令交換功能；RunDirectOnly 僅做設備追蹤。
func (a *Agent) Run(ctx context.Context) error {
	if err := a.connectServer(ctx); err != nil {
		return err
	}
	defer a.wsConn.CloseNow()

	// 啟動 ADB 設備追蹤
	tracker := adb.NewTracker(a.config.ADBAddr)
	deviceCh := tracker.Track(ctx)

	// 註冊到 Server
	a.register(ctx, []protocol.DeviceInfo{})

	slog.Info("Agent 就緒", "server", a.config.ServerURL, "conn_id", a.connID)

	// 主迴圈：以 goroutine 持續讀取 Server 訊息推送至 channel，
	// 與 ADB 設備事件透過 select 多路復用處理。
	// buffer 設為 16 可避免 WebSocket 讀取被短暫阻塞。
	msgCh := make(chan protocol.Envelope, 16)
	go a.readServerLoop(ctx, msgCh)

	for {
		select {
		case <-ctx.Done():
			return nil

		case events, ok := <-deviceCh:
			if !ok {
				slog.Warn("ADB 追蹤 channel 已關閉")
				deviceCh = nil
				continue
			}
			a.handleDeviceEvents(ctx, events)

		case msg, ok := <-msgCh:
			if !ok {
				return fmt.Errorf("Server 連線已斷開")
			}
			a.handleServerMessage(ctx, msg)
		}
	}
}

// RunDirectOnly 僅啟動 ADB 設備追蹤，不連線 Signal Server。
//
// 此模式搭配 directsrv 使用：directsrv 透過 Agent.DeviceTable() 取得共享的設備表，
// 並自行提供 TCP 直連與 mDNS 廣播服務。因為不經過 Signal Server，所以不支援
// 遠端信令交換（lock/unlock/offer 等），僅適用於 LAN 環境。
func (a *Agent) RunDirectOnly(ctx context.Context) error {
	tracker := adb.NewTracker(a.config.ADBAddr)
	deviceCh := tracker.Track(ctx)

	slog.Info("Agent 就緒（direct-only 模式）")

	for {
		select {
		case <-ctx.Done():
			return nil
		case events, ok := <-deviceCh:
			if !ok {
				slog.Warn("ADB 追蹤 channel 已關閉")
				deviceCh = nil
				continue
			}
			a.deviceTable.Update(events)
			slog.Info("設備列表更新", "count", len(a.deviceTable.List()))
		}
	}
}

// connectServer 連線 Signal Server 並完成 PSK 認證交握。
//
// 認證流程：
//  1. WebSocket Dial 至 ServerURL/ws（10 秒超時）
//  2. 發送 auth 訊息（含 token 與 RoleAgent 角色標識）
//  3. 等待 auth_ack 回應，取得 Server 分配的 connID
//
// connID 後續作為此 Agent 在信令系統中的唯一識別，用於訊息路由。
func (a *Agent) connectServer(ctx context.Context) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()

	url := a.config.ServerURL + "/ws"
	conn, _, err := ws.Dial(dialCtx, url, nil)
	if err != nil {
		return fmt.Errorf("連線 Server 失敗: %w", err)
	}
	a.wsConn = conn

	// 認證
	authEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeAuth, a.hostname, "temp", "",
		protocol.AuthPayload{Token: a.config.Token, Role: protocol.RoleAgent},
	)
	if err := a.sendEnvelope(ctx, authEnv); err != nil {
		return fmt.Errorf("發送認證失敗: %w", err)
	}

	// 接收 auth_ack
	ack, err := a.readEnvelope(ctx)
	if err != nil {
		return fmt.Errorf("讀取認證回應失敗: %w", err)
	}

	var ackPayload protocol.AuthAckPayload
	if err := ack.DecodePayload(&ackPayload); err != nil {
		return fmt.Errorf("解析認證回應失敗: %w", err)
	}
	if !ackPayload.Success {
		return fmt.Errorf("認證失敗: %s", ackPayload.Reason)
	}

	a.connID = ackPayload.AssignID
	slog.Info("Server 認證成功", "conn_id", a.connID)
	return nil
}

// register 向 Signal Server 發送註冊訊息，讓 Server 知道此 Agent 的存在與初始設備清單。
func (a *Agent) register(ctx context.Context, devices []protocol.DeviceInfo) {
	env, _ := protocol.NewEnvelope(
		protocol.MsgTypeRegister, a.hostname, a.connID, "",
		protocol.RegisterPayload{
			HostID:   a.connID,
			Hostname: a.config.HostID,
			Devices:  devices,
		},
	)
	a.sendEnvelope(ctx, env)
}

// handleDeviceEvents 處理 ADB Tracker 回報的設備變更事件。
// 更新本地設備表後，將最新完整設備清單同步至 Signal Server。
func (a *Agent) handleDeviceEvents(ctx context.Context, events []adb.DeviceEvent) {
	a.deviceTable.Update(events)
	devices := a.deviceTable.List()

	slog.Info("設備列表更新", "count", len(devices))

	// 轉換為 protocol 格式並通知 Server
	protoDevices := make([]protocol.DeviceInfo, len(devices))
	for i, d := range devices {
		protoDevices[i] = protocol.DeviceInfo{
			Serial:   d.Serial,
			State:    protocol.DeviceState(d.State),
			Lock:     protocol.LockState(d.Lock),
			LockedBy: d.LockedBy,
		}
	}

	env, _ := protocol.NewEnvelope(
		protocol.MsgTypeDeviceUpdate, a.hostname, a.connID, "",
		protocol.DeviceUpdatePayload{HostID: a.connID, Devices: protoDevices},
	)
	a.sendEnvelope(ctx, env)
}

// handleServerMessage 根據信令訊息類型分派至對應的處理函式。
//
// 支援的訊息類型：
//   - lock_req：Client 請求鎖定某設備（獨佔使用）
//   - unlock_req：Client 請求解鎖某設備
//   - offer：Client 發送 WebRTC SDP Offer，請求建立 P2P 連線
func (a *Agent) handleServerMessage(ctx context.Context, msg protocol.Envelope) {
	switch msg.Type {
	case protocol.MsgTypeLockReq:
		a.handleLockReq(ctx, msg)
	case protocol.MsgTypeUnlockReq:
		a.handleUnlockReq(ctx, msg)
	case protocol.MsgTypeOffer:
		a.handleOffer(ctx, msg)
	default:
		slog.Debug("收到未處理的訊息類型", "type", msg.Type)
	}
}

// handleLockReq 處理設備鎖定請求。
//
// 設備鎖定語義：
//   - 每台設備同時只允許一位 Client 鎖定（獨佔操作）
//   - 鎖定者以 Client 的 connID（msg.SourceID）識別
//   - 鎖定成功後，其他 Client 對同一設備的 lock_req 會被拒絕
//   - 鎖定狀態儲存在 deviceTable 中（執行緒安全）
func (a *Agent) handleLockReq(ctx context.Context, msg protocol.Envelope) {
	var payload protocol.LockReqPayload
	if err := msg.DecodePayload(&payload); err != nil {
		return
	}

	success := a.deviceTable.Lock(payload.Serial, msg.SourceID)
	reason := ""
	if !success {
		reason = "設備不可用或已被鎖定"
	}

	resp, _ := protocol.NewEnvelope(
		protocol.MsgTypeLockResp, a.hostname, a.connID, msg.SourceID,
		protocol.LockRespPayload{Success: success, Serial: payload.Serial, Reason: reason},
	)
	a.sendEnvelope(ctx, resp)

	if success {
		slog.Info("設備已鎖定", "serial", payload.Serial, "client", msg.SourceID)
	}
}

// handleUnlockReq 處理設備解鎖請求。
// 只有持鎖者（LockedBy == msg.SourceID）才能成功解鎖。
func (a *Agent) handleUnlockReq(ctx context.Context, msg protocol.Envelope) {
	var payload protocol.UnlockReqPayload
	if err := msg.DecodePayload(&payload); err != nil {
		return
	}

	success := a.deviceTable.Unlock(payload.Serial, msg.SourceID)
	reason := ""
	if !success {
		reason = "設備未被鎖定或非持鎖者"
	}

	resp, _ := protocol.NewEnvelope(
		protocol.MsgTypeUnlockResp, a.hostname, a.connID, msg.SourceID,
		protocol.UnlockRespPayload{Success: success, Serial: payload.Serial, Reason: reason},
	)
	a.sendEnvelope(ctx, resp)

	if success {
		slog.Info("設備已解鎖", "serial", payload.Serial, "client", msg.SourceID)
	}
}

// handleOffer 處理 Client 發來的 WebRTC SDP Offer，建立 P2P 連線。
//
// WebRTC 連線建立的非同步流程：
//  1. 建立新的 PeerConnection（PeerManager），配置 ICE server
//  2. 註冊 OnChannel 回調 — 當 Client 開啟 DataChannel 時觸發，
//     每個 DataChannel 對應一台設備的 ADB 轉發，在獨立 goroutine 中處理
//  3. 註冊 OnDisconnect 回調 — 當 WebRTC 連線斷開時觸發，
//     自動呼叫 UnlockAll 釋放該 Client 持有的所有設備鎖定（自動清理機制），
//     避免 Client 異常離線後設備被永久鎖定
//  4. 呼叫 HandleOffer 設定遠端 SDP 並產生 Answer
//  5. 將 Answer 透過 Signal Server 回傳給 Client
//
// 注意：步驟 2、3 的回調是非同步的，它們會在 WebRTC 內部 goroutine 中被觸發，
// 而非在 handleOffer 返回前執行。handleOffer 返回後 ICE 協商才真正開始。
func (a *Agent) handleOffer(ctx context.Context, msg protocol.Envelope) {
	var sdpPayload protocol.SDPPayload
	if err := msg.DecodePayload(&sdpPayload); err != nil {
		return
	}

	clientID := msg.SourceID
	slog.Info("收到 WebRTC Offer", "from", clientID)

	pm, err := webrtc.NewPeerManager(a.config.ICEConfig)
	if err != nil {
		slog.Error("建立 PeerConnection 失敗", "error", err)
		return
	}

	pm.OnChannel(func(label string, rwc io.ReadWriteCloser) {
		go a.handleDataChannel(ctx, clientID, label, rwc)
	})

	// OnDisconnect 回調：Client WebRTC 連線斷開時的自動清理機制。
	// UnlockAll 會釋放該 clientID 持有的所有設備鎖定，
	// 確保即使 Client 異常斷線（網路中斷、程式崩潰），設備也不會被永久鎖定。
	// 最後關閉 PeerConnection 釋放底層資源。
	pm.OnDisconnect(func() {
		slog.Info("Client 斷線，釋放所有設備", "client", clientID)
		a.deviceTable.UnlockAll(clientID)
		pm.Close()
	})

	answerSDP, err := pm.HandleOffer(sdpPayload.SDP)
	if err != nil {
		slog.Error("處理 Offer 失敗", "error", err)
		pm.Close()
		return
	}

	answer, _ := protocol.NewEnvelope(
		protocol.MsgTypeAnswer, a.hostname, a.connID, clientID,
		protocol.SDPPayload{SDP: answerSDP, Type: "answer"},
	)
	a.sendEnvelope(ctx, answer)
}

// handleDataChannel 處理單一 DataChannel 上的 ADB 流量轉發。
//
// DataChannel label 格式："adb/<serial>/<session_id>"
//   - "adb"：固定前綴，用於識別 channel 用途（未來可擴展其他類型）
//   - <serial>：Android 設備序號（例如 "emulator-5554"），指定要連線的目標設備
//   - <session_id>：Client 產生的唯一 session 識別碼，用於區分同一設備的多次連線
//
// 轉發機制：
//   - 透過 ADB Dialer 建立到目標設備 5555 port 的 TCP 連線
//   - 以雙向 io.Copy 橋接 DataChannel（rwc）與 ADB TCP 連線
//   - 任一方向的串流結束或 context 取消時，整個轉發停止
func (a *Agent) handleDataChannel(ctx context.Context, clientID, label string, rwc io.ReadWriteCloser) {
	defer rwc.Close()

	parts := strings.SplitN(label, "/", 3)
	if len(parts) < 2 || parts[0] != "adb" {
		slog.Warn("無效的 DataChannel label", "label", label)
		return
	}
	serial := parts[1]

	slog.Info("開始 ADB 轉發", "serial", serial, "client", clientID, "label", label)

	adbConn, err := a.dialer.DialDevice(serial, 5555)
	if err != nil {
		slog.Error("連線 ADB 設備失敗", "serial", serial, "error", err)
		return
	}
	defer adbConn.Close()

	// 雙向橋接：DataChannel ↔ ADB TCP
	// 使用 buffer 為 2 的 error channel，確保兩個 goroutine 都能寫入而不阻塞。
	// 只要任一方向結束（EOF 或錯誤），即視為轉發結束。
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(adbConn, rwc) // Client → ADB 設備
		errc <- err
	}()
	go func() {
		_, err := io.Copy(rwc, adbConn) // ADB 設備 → Client
		errc <- err
	}()

	select {
	case err := <-errc:
		if err != nil {
			slog.Debug("ADB 轉發結束", "serial", serial, "error", err)
		}
	case <-ctx.Done():
	}

	slog.Info("ADB 轉發已停止", "serial", serial, "client", clientID)
}

// --- Server WebSocket 輔助方法 ---

// readServerLoop 持續從 WebSocket 讀取信令訊息並推送至 channel。
// 當連線斷開或 context 取消時，關閉 channel 通知主迴圈。
func (a *Agent) readServerLoop(ctx context.Context, ch chan<- protocol.Envelope) {
	defer close(ch)
	for {
		env, err := a.readEnvelope(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("讀取 Server 訊息失敗", "error", err)
			return
		}
		select {
		case ch <- env:
		case <-ctx.Done():
			return
		}
	}
}

// sendEnvelope 將信令訊息序列化為 JSON 並透過 WebSocket 發送至 Server。
func (a *Agent) sendEnvelope(ctx context.Context, env protocol.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return a.wsConn.Write(ctx, ws.MessageText, data)
}

// readEnvelope 從 WebSocket 讀取一則訊息並反序列化為信令 Envelope。
func (a *Agent) readEnvelope(ctx context.Context) (protocol.Envelope, error) {
	_, data, err := a.wsConn.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return protocol.Envelope{}, err
	}
	return env, nil
}
