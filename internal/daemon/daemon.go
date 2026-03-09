package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/chris1004tw/remote-adb/internal/proxy"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
	ws "github.com/coder/websocket"
)

// Config 是 Daemon 的設定。
type Config struct {
	SignalURL string
	Token     string
	PortStart int
	PortEnd   int
	ICEConfig webrtc.ICEConfig
}

// Daemon 管理 WebRTC 連線、TCP 代理與 IPC 服務。
type Daemon struct {
	config   Config
	ports    *PortAllocator
	bindings *BindingTable
	hostname string

	// Signal Server 連線
	wsConn *ws.Conn
	connID string

	// 回應等待者
	waiterMu sync.Mutex
	waiters  map[string]chan protocol.Envelope

	// 活躍的 proxy 和 peer
	proxyMu sync.Mutex
	proxies map[int]*proxy.Proxy
	peers   map[int]*webrtc.PeerManager

	// 主機列表快取
	hostsMu sync.RWMutex
	hosts   []protocol.HostInfo
}

// NewDaemon 建立一個新的 Daemon 實例。
func NewDaemon(cfg Config) *Daemon {
	if cfg.PortStart == 0 {
		cfg.PortStart = 15555
	}
	if cfg.PortEnd == 0 {
		cfg.PortEnd = 15655
	}

	hostname, _ := os.Hostname()

	return &Daemon{
		config:   cfg,
		ports:    NewPortAllocator(cfg.PortStart, cfg.PortEnd),
		bindings: NewBindingTable(),
		hostname: hostname,
		waiters:  make(map[string]chan protocol.Envelope),
		proxies:  make(map[int]*proxy.Proxy),
		peers:    make(map[int]*webrtc.PeerManager),
	}
}

// Bindings 回傳綁定表（供測試使用）。
func (d *Daemon) Bindings() *BindingTable {
	return d.bindings
}

// Ports 回傳 Port 分配器（供測試使用）。
func (d *Daemon) Ports() *PortAllocator {
	return d.ports
}

// Start 啟動 Daemon：連線 Signal Server、啟動 IPC 服務。
func (d *Daemon) Start(ctx context.Context, ipcListener net.Listener) error {
	if err := d.connectSignal(ctx); err != nil {
		return err
	}

	go d.signalReadLoop(ctx)
	d.requestHostList(ctx)

	slog.Info("Daemon 就緒", "conn_id", d.connID, "ipc", ipcListener.Addr())

	// ServeIPC 會阻塞直到 ctx 取消
	d.ServeIPC(ctx, ipcListener)

	return d.shutdown()
}

// ServeIPC 啟動 IPC 服務，接受連線並處理指令。
// 可獨立於 Signal 連線使用（方便測試）。
func (d *Daemon) ServeIPC(ctx context.Context, ln net.Listener) {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("IPC Accept 失敗", "error", err)
			return
		}
		go d.handleIPCConn(ctx, conn)
	}
}

func (d *Daemon) handleIPCConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	var cmd IPCCommand
	if err := json.NewDecoder(conn).Decode(&cmd); err != nil {
		json.NewEncoder(conn).Encode(ErrorResponse("解析指令失敗"))
		return
	}

	slog.Debug("收到 IPC 指令", "action", cmd.Action)

	resp := d.handleCommand(ctx, cmd)
	json.NewEncoder(conn).Encode(resp)
}

func (d *Daemon) handleCommand(ctx context.Context, cmd IPCCommand) IPCResponse {
	switch cmd.Action {
	case "list":
		return d.cmdList()
	case "status":
		return d.cmdStatus()
	case "hosts":
		return d.cmdHosts()
	case "bind":
		return d.cmdBind(ctx, cmd.Payload)
	case "unbind":
		return d.cmdUnbind(ctx, cmd.Payload)
	default:
		return ErrorResponse(fmt.Sprintf("未知指令: %s", cmd.Action))
	}
}

func (d *Daemon) cmdList() IPCResponse {
	return SuccessResponse(d.bindings.List())
}

func (d *Daemon) cmdStatus() IPCResponse {
	return SuccessResponse(StatusInfo{
		Connected: d.wsConn != nil,
		ConnID:    d.connID,
		SignalURL: d.config.SignalURL,
		BindCount: d.bindings.Count(),
	})
}

func (d *Daemon) cmdHosts() IPCResponse {
	d.hostsMu.RLock()
	defer d.hostsMu.RUnlock()
	return SuccessResponse(d.hosts)
}

func (d *Daemon) cmdBind(ctx context.Context, payload json.RawMessage) IPCResponse {
	if d.wsConn == nil {
		return ErrorResponse("未連線到 Signal Server")
	}

	var req BindRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return ErrorResponse("解析 bind 參數失敗")
	}

	// 檢查是否已綁定相同設備
	if _, found := d.bindings.FindBySerial(req.Serial); found {
		return ErrorResponse(fmt.Sprintf("設備 %s 已被綁定", req.Serial))
	}

	// 1. 分配 port
	port, err := d.ports.Allocate()
	if err != nil {
		return ErrorResponse(err.Error())
	}

	// 2. 鎖定設備
	lockEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeLockReq, d.hostname, d.connID, req.HostID,
		protocol.LockReqPayload{HostID: req.HostID, Serial: req.Serial},
	)
	if err := d.sendEnvelope(ctx, lockEnv); err != nil {
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("發送鎖定請求失敗: %v", err))
	}

	lockResp, err := d.waitResponse("lock_resp:"+req.Serial, 10*time.Second)
	if err != nil {
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("等待鎖定回應失敗: %v", err))
	}

	var lockPayload protocol.LockRespPayload
	if err := lockResp.DecodePayload(&lockPayload); err != nil || !lockPayload.Success {
		d.ports.Release(port)
		reason := "鎖定失敗"
		if lockPayload.Reason != "" {
			reason = lockPayload.Reason
		}
		return ErrorResponse(reason)
	}

	// 3. 建立 WebRTC 連線
	pm, err := webrtc.NewPeerManager(d.config.ICEConfig)
	if err != nil {
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("建立 PeerConnection 失敗: %v", err))
	}

	// 4. 開啟 DataChannel
	sessionID := generateSessionID()
	label := fmt.Sprintf("adb/%s/%s", req.Serial, sessionID)
	channel, err := pm.OpenChannel(label)
	if err != nil {
		pm.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("建立 DataChannel 失敗: %v", err))
	}

	// 5. 建立 Offer 並發送
	offerSDP, err := pm.CreateOffer()
	if err != nil {
		pm.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("建立 Offer 失敗: %v", err))
	}

	offerEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeOffer, d.hostname, d.connID, req.HostID,
		protocol.SDPPayload{SDP: offerSDP, Type: "offer"},
	)
	if err := d.sendEnvelope(ctx, offerEnv); err != nil {
		pm.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("發送 Offer 失敗: %v", err))
	}

	// 6. 等待 Answer
	answerEnv, err := d.waitResponse("answer:"+req.HostID, 15*time.Second)
	if err != nil {
		pm.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("等待 Answer 失敗: %v", err))
	}

	var answerPayload protocol.SDPPayload
	if err := answerEnv.DecodePayload(&answerPayload); err != nil {
		pm.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("解析 Answer 失敗: %v", err))
	}

	if err := pm.HandleAnswer(answerPayload.SDP); err != nil {
		pm.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("處理 Answer 失敗: %v", err))
	}

	// 7. 建立 TCP 代理
	p, err := proxy.New(port, channel)
	if err != nil {
		pm.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("建立 TCP 代理失敗: %v", err))
	}
	p.Start(ctx)

	// 8. 記錄
	d.proxyMu.Lock()
	d.proxies[port] = p
	d.peers[port] = pm
	d.proxyMu.Unlock()

	d.bindings.Add(Binding{
		LocalPort: port,
		HostID:    req.HostID,
		Serial:    req.Serial,
		Status:    "active",
	})

	// 9. 監聽斷線
	pm.OnDisconnect(func() {
		slog.Info("WebRTC 連線斷開", "port", port, "serial", req.Serial)
		d.bindings.UpdateStatus(port, "disconnected")
	})

	slog.Info("綁定成功", "port", port, "serial", req.Serial, "host", req.HostID)
	return SuccessResponse(BindResult{LocalPort: port, Serial: req.Serial})
}

func (d *Daemon) cmdUnbind(ctx context.Context, payload json.RawMessage) IPCResponse {
	var req UnbindRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return ErrorResponse("解析 unbind 參數失敗")
	}

	binding, ok := d.bindings.Get(req.LocalPort)
	if !ok {
		return ErrorResponse(fmt.Sprintf("Port %d 未綁定", req.LocalPort))
	}

	// 停止代理和 PeerConnection
	d.proxyMu.Lock()
	if p, ok := d.proxies[req.LocalPort]; ok {
		p.Stop()
		delete(d.proxies, req.LocalPort)
	}
	if pm, ok := d.peers[req.LocalPort]; ok {
		pm.Close()
		delete(d.peers, req.LocalPort)
	}
	d.proxyMu.Unlock()

	// 通知 Agent 解鎖（fire-and-forget）
	if d.wsConn != nil {
		env, _ := protocol.NewEnvelope(
			protocol.MsgTypeUnlockReq, d.hostname, d.connID, binding.HostID,
			protocol.UnlockReqPayload{HostID: binding.HostID, Serial: binding.Serial},
		)
		d.sendEnvelope(ctx, env)
	}

	d.bindings.Remove(req.LocalPort)
	d.ports.Release(req.LocalPort)

	slog.Info("解綁成功", "port", req.LocalPort, "serial", binding.Serial)
	return SuccessResponse(nil)
}

// --- Signal Server 連線 ---

func (d *Daemon) connectSignal(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url := d.config.SignalURL + "/ws"
	conn, _, err := ws.Dial(dialCtx, url, nil)
	if err != nil {
		return fmt.Errorf("連線 Signal Server 失敗: %w", err)
	}
	d.wsConn = conn

	// 認證
	authEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeAuth, d.hostname, "temp", "",
		protocol.AuthPayload{Token: d.config.Token, Role: protocol.RoleClient},
	)
	if err := d.sendEnvelope(ctx, authEnv); err != nil {
		return fmt.Errorf("發送認證失敗: %w", err)
	}

	// 讀取 auth_ack
	_, data, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("讀取認證回應失敗: %w", err)
	}

	var ack protocol.Envelope
	if err := json.Unmarshal(data, &ack); err != nil {
		return fmt.Errorf("解析認證回應失敗: %w", err)
	}

	var ackPayload protocol.AuthAckPayload
	if err := ack.DecodePayload(&ackPayload); err != nil || !ackPayload.Success {
		return fmt.Errorf("認證失敗")
	}

	d.connID = ackPayload.AssignID
	slog.Info("Signal Server 認證成功", "conn_id", d.connID)
	return nil
}

func (d *Daemon) signalReadLoop(ctx context.Context) {
	for {
		_, data, err := d.wsConn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("Signal 讀取失敗", "error", err)
			return
		}

		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		d.handleSignalMessage(env)
	}
}

func (d *Daemon) handleSignalMessage(env protocol.Envelope) {
	switch env.Type {
	case protocol.MsgTypeHostListResp:
		var payload protocol.HostListRespPayload
		if err := env.DecodePayload(&payload); err == nil {
			d.hostsMu.Lock()
			d.hosts = payload.Hosts
			d.hostsMu.Unlock()
		}

	case protocol.MsgTypeDeviceUpdate:
		var payload protocol.DeviceUpdatePayload
		if err := env.DecodePayload(&payload); err == nil {
			d.updateHostDevices(payload.HostID, payload.Devices)
		}

	case protocol.MsgTypeLockResp:
		var payload protocol.LockRespPayload
		if err := env.DecodePayload(&payload); err == nil {
			d.deliverResponse("lock_resp:"+payload.Serial, env)
		}

	case protocol.MsgTypeUnlockResp:
		var payload protocol.UnlockRespPayload
		if err := env.DecodePayload(&payload); err == nil {
			d.deliverResponse("unlock_resp:"+payload.Serial, env)
		}

	case protocol.MsgTypeAnswer:
		d.deliverResponse("answer:"+env.SourceID, env)

	default:
		slog.Debug("收到未處理的訊息", "type", env.Type)
	}
}

func (d *Daemon) updateHostDevices(hostID string, devices []protocol.DeviceInfo) {
	d.hostsMu.Lock()
	defer d.hostsMu.Unlock()
	for i := range d.hosts {
		if d.hosts[i].HostID == hostID {
			d.hosts[i].Devices = devices
			return
		}
	}
}

func (d *Daemon) sendEnvelope(ctx context.Context, env protocol.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return d.wsConn.Write(ctx, ws.MessageText, data)
}

func (d *Daemon) requestHostList(ctx context.Context) {
	env, _ := protocol.NewEnvelope(
		protocol.MsgTypeHostList, d.hostname, d.connID, "",
		struct{}{},
	)
	d.sendEnvelope(ctx, env)
}

func (d *Daemon) waitResponse(key string, timeout time.Duration) (protocol.Envelope, error) {
	ch := make(chan protocol.Envelope, 1)
	d.waiterMu.Lock()
	d.waiters[key] = ch
	d.waiterMu.Unlock()

	defer func() {
		d.waiterMu.Lock()
		delete(d.waiters, key)
		d.waiterMu.Unlock()
	}()

	select {
	case env := <-ch:
		return env, nil
	case <-time.After(timeout):
		return protocol.Envelope{}, fmt.Errorf("等待 %s 逾時", key)
	}
}

func (d *Daemon) deliverResponse(key string, env protocol.Envelope) {
	d.waiterMu.Lock()
	ch, ok := d.waiters[key]
	d.waiterMu.Unlock()
	if ok {
		select {
		case ch <- env:
		default:
		}
	}
}

func (d *Daemon) shutdown() error {
	slog.Info("Daemon 正在關閉...")

	d.proxyMu.Lock()
	for port, p := range d.proxies {
		p.Stop()
		delete(d.proxies, port)
	}
	for port, pm := range d.peers {
		pm.Close()
		delete(d.peers, port)
	}
	d.proxyMu.Unlock()

	if d.wsConn != nil {
		d.wsConn.CloseNow()
	}

	slog.Info("Daemon 已關閉")
	return nil
}

func generateSessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
