package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
	ws "github.com/coder/websocket"
)

func main() {
	signalURL := flag.String("signal", envStr("RADB_SIGNAL_URL", "ws://localhost:8080"), "Signal Server WebSocket 位址")
	token := flag.String("token", envStr("RADB_TOKEN", ""), "PSK Token")
	hostID := flag.String("host-id", envStr("RADB_HOST_ID", hostname()), "主機識別名稱")
	adbPort := flag.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server 埠")
	stunURLs := flag.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN Server URL（逗號分隔）")
	turnURL := flag.String("turn", envStr("RADB_TURN_URL", ""), "TURN Server URL")
	turnUser := flag.String("turn-user", envStr("RADB_TURN_USER", ""), "TURN 使用者名稱")
	turnPass := flag.String("turn-pass", envStr("RADB_TURN_PASS", ""), "TURN 密碼")
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "錯誤：必須設定 RADB_TOKEN 環境變數或使用 --token flag")
		os.Exit(1)
	}

	slog.Info("啟動 radb-agent", "version", buildinfo.Version, "host_id", *hostID)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ICE 設定
	iceConfig := webrtc.ICEConfig{}
	if *stunURLs != "" {
		iceConfig.STUNServers = strings.Split(*stunURLs, ",")
	}
	if *turnURL != "" {
		iceConfig.TURNServers = []webrtc.TURNServer{
			{URL: *turnURL, Username: *turnUser, Credential: *turnPass},
		}
	}

	agent := &Agent{
		signalURL: *signalURL + "/ws",
		token:     *token,
		hostID:    *hostID,
		hostname:  hostname(),
		adbAddr:   fmt.Sprintf("127.0.0.1:%d", *adbPort),
		iceConfig: iceConfig,
	}

	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("Agent 執行失敗", "error", err)
		os.Exit(1)
	}
	slog.Info("Agent 已關閉")
}

// Agent 是遠端代理端的核心邏輯。
type Agent struct {
	signalURL string
	token     string
	hostID    string
	hostname  string
	adbAddr   string
	iceConfig webrtc.ICEConfig

	// 執行時狀態
	connID      string
	wsConn      *ws.Conn
	deviceTable *adb.DeviceTable
	dialer      *adb.Dialer
}

// Run 執行 Agent 主迴圈：連線 Signal、追蹤設備、處理信令。
func (a *Agent) Run(ctx context.Context) error {
	a.deviceTable = adb.NewDeviceTable()
	a.dialer = adb.NewDialer(a.adbAddr)

	// 連線到 Signal Server
	if err := a.connectSignal(ctx); err != nil {
		return err
	}
	defer a.wsConn.CloseNow()

	// 啟動 ADB 設備追蹤
	tracker := adb.NewTracker(a.adbAddr)
	deviceCh := tracker.Track(ctx)

	// 註冊到 Signal Server
	a.register(ctx, []protocol.DeviceInfo{})

	slog.Info("Agent 就緒", "signal", a.signalURL, "conn_id", a.connID)

	// 主迴圈：同時處理 ADB 設備事件和 Signal 訊息
	msgCh := make(chan protocol.Envelope, 16)
	go a.readSignalLoop(ctx, msgCh)

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
				return fmt.Errorf("Signal Server 連線已斷開")
			}
			a.handleSignalMessage(ctx, msg)
		}
	}
}

func (a *Agent) connectSignal(ctx context.Context) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()

	conn, _, err := ws.Dial(dialCtx, a.signalURL, nil)
	if err != nil {
		return fmt.Errorf("連線 Signal Server 失敗: %w", err)
	}
	a.wsConn = conn

	// 認證
	authEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeAuth, a.hostname, "temp", "",
		protocol.AuthPayload{Token: a.token, Role: protocol.RoleAgent},
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
	slog.Info("Signal Server 認證成功", "conn_id", a.connID)
	return nil
}

func (a *Agent) register(ctx context.Context, devices []protocol.DeviceInfo) {
	env, _ := protocol.NewEnvelope(
		protocol.MsgTypeRegister, a.hostname, a.connID, "",
		protocol.RegisterPayload{
			HostID:   a.connID,
			Hostname: a.hostID,
			Devices:  devices,
		},
	)
	a.sendEnvelope(ctx, env)
}

func (a *Agent) handleDeviceEvents(ctx context.Context, events []adb.DeviceEvent) {
	a.deviceTable.Update(events)
	devices := a.deviceTable.List()

	slog.Info("設備列表更新", "count", len(devices))

	// 轉換為 protocol 格式並通知 Signal
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

func (a *Agent) handleSignalMessage(ctx context.Context, msg protocol.Envelope) {
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

	// 建立新的 PeerConnection
	pm, err := webrtc.NewPeerManager(a.iceConfig)
	if err != nil {
		slog.Error("建立 PeerConnection 失敗", "error", err)
		return
	}

	// 監聽 DataChannel（Client 開啟的設備通道）
	pm.OnChannel(func(label string, rwc io.ReadWriteCloser) {
		go a.handleDataChannel(ctx, clientID, label, rwc)
	})

	// 監聽斷線
	pm.OnDisconnect(func() {
		slog.Info("Client 斷線，釋放所有設備", "client", clientID)
		a.deviceTable.UnlockAll(clientID)
		pm.Close()
	})

	// 處理 Offer 並回傳 Answer
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

	// 解析 label 取得設備序號
	parts := strings.SplitN(label, "/", 3)
	if len(parts) < 2 || parts[0] != "adb" {
		slog.Warn("無效的 DataChannel label", "label", label)
		return
	}
	serial := parts[1]

	slog.Info("開始 ADB 轉發", "serial", serial, "client", clientID, "label", label)

	// 連線到 ADB server 上的目標設備
	adbConn, err := a.dialer.DialDevice(serial, 5555)
	if err != nil {
		slog.Error("連線 ADB 設備失敗", "serial", serial, "error", err)
		return
	}
	defer adbConn.Close()

	// 雙向資料泵浦
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(adbConn, rwc)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(rwc, adbConn)
		errc <- err
	}()

	// 等待任一方向結束
	select {
	case err := <-errc:
		if err != nil {
			slog.Debug("ADB 轉發結束", "serial", serial, "error", err)
		}
	case <-ctx.Done():
	}

	slog.Info("ADB 轉發已停止", "serial", serial, "client", clientID)
}

// --- Signal WebSocket helpers ---

func (a *Agent) readSignalLoop(ctx context.Context, ch chan<- protocol.Envelope) {
	defer close(ch)
	for {
		env, err := a.readEnvelope(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("讀取 Signal 訊息失敗", "error", err)
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

// --- helpers ---

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
