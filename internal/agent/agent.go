// Package agent 實作 radb 遠端代理端的核心邏輯。
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

// Config 是 Agent 的設定。
type Config struct {
	ServerURL string // Server WebSocket 位址（不含 /ws）
	Token     string
	HostID    string
	ADBAddr   string // 例如 "127.0.0.1:5037"
	ICEConfig webrtc.ICEConfig
}

// Agent 是遠端代理端的核心邏輯。
type Agent struct {
	config   Config
	hostname string

	// 執行時狀態
	connID      string
	wsConn      *ws.Conn
	deviceTable *adb.DeviceTable
	dialer      *adb.Dialer
}

// New 建立一個新的 Agent 實例。
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

// Run 執行 Agent 主迴圈：連線 Server、追蹤設備、處理信令。
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

	// 主迴圈：同時處理 ADB 設備事件和 Server 訊息
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
// 供 direct-only 模式使用（搭配 directsrv）。
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

// handleDataChannel 處理單一設備的 ADB TCP 轉發。
// label 格式：adb/<serial>/<session_id>
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

	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(adbConn, rwc)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(rwc, adbConn)
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

// --- Server WebSocket helpers ---

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

func (a *Agent) sendEnvelope(ctx context.Context, env protocol.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return a.wsConn.Write(ctx, ws.MessageText, data)
}

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
