// Package daemon 實作本機端的背景服務（Daemon），扮演開發者本機與遠端 Agent 之間的橋樑。
//
// 整體架構角色：
//
//	使用者 CLI ──IPC──▶ Daemon ──WebSocket──▶ Signal Server ──WebSocket──▶ Agent
//	                       │                                                  │
//	                       └──────── WebRTC DataChannel (P2P) ────────────────┘
//	                       │
//	  adb client ──TCP──▶ Proxy（本機 port）
//
// Daemon 負責：
//  1. 與 Signal Server 建立 WebSocket 連線，進行認證與信令交換
//  2. 為每個綁定的遠端設備建立 WebRTC PeerConnection + DataChannel
//  3. 在本機開啟 TCP Proxy，讓 adb client 可直接連線到 127.0.0.1:<port>
//  4. 透過 IPC（TCP/Unix Socket）接受 CLI 工具的指令（bind/unbind/list/status/hosts）
//  5. 管理 Port 分配與 Binding 狀態追蹤
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

// Config 是 Daemon 的啟動設定。
type Config struct {
	ServerURL string           // Signal Server 的 WebSocket URL（如 ws://example.com:8080）
	Token     string           // PSK 認證令牌，須與 Server 端設定一致
	PortStart int              // 本機 TCP Proxy 的 Port 分配範圍起始值（預設 15555）
	PortEnd   int              // 本機 TCP Proxy 的 Port 分配範圍結束值（預設 15655）
	ICEConfig webrtc.ICEConfig // WebRTC ICE 設定（STUN/TURN 伺服器等）
}

// Daemon 是本機端背景服務的核心結構，管理 WebRTC 連線、TCP 代理與 IPC 服務。
//
// Daemon 持有三把 mutex，鎖定順序為：waiterMu → proxyMu → hostsMu。
// 在同時需要多把鎖的場景中，必須按照此順序取鎖以避免 deadlock。
// 實際上目前各 mutex 保護的資源是獨立操作的，不會同時持有多把鎖。
type Daemon struct {
	config   Config
	ports    *PortAllocator
	bindings *BindingTable
	hostname string // 本機主機名，用於信令訊息的 source 欄位

	// Server 連線（Signal Server 的 WebSocket 連線）
	wsConn *ws.Conn
	connID string // Server 認證成功後分配的連線 ID

	// waiters 實作了非同步請求-回應的配對機制。
	// 工作原理：
	//  1. cmdBind 等方法發送請求後，呼叫 waitResponse(key) 註冊一個 channel 到 waiters map
	//  2. serverReadLoop 收到 Server 回傳的訊息後，呼叫 deliverResponse(key) 將訊息寫入對應 channel
	//  3. waitResponse 從 channel 收到回應或逾時返回
	// key 的格式為 "lock_resp:<serial>" 或 "answer:<hostID>"，用於精準匹配請求與回應。
	waiterMu sync.Mutex                       // 保護 waiters map 的讀寫
	waiters  map[string]chan protocol.Envelope // key: 回應識別鍵 → 用於接收回應的 channel

	// proxyMu 保護 proxies 和 peers 兩個 map 的讀寫，
	// 這兩個 map 以本機 port 為 key，分別儲存 TCP Proxy 和 WebRTC PeerManager。
	proxyMu sync.Mutex
	proxies map[int]*proxy.Proxy         // key: 本機 port → TCP 代理實例
	peers   map[int]*webrtc.PeerManager  // key: 本機 port → WebRTC 連線管理器

	// hostsMu 保護 hosts 快取的讀寫，使用 RWMutex 允許多個 cmdHosts 並行讀取。
	hostsMu sync.RWMutex
	hosts   []protocol.HostInfo // 從 Server 取得的遠端主機與設備清單快取
}

// NewDaemon 建立一個新的 Daemon 實例。
// 若 PortStart/PortEnd 未指定，使用預設範圍 15555~15655（共 101 個 port）。
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

// Start 啟動 Daemon，執行以下步驟：
//  1. 連線 Signal Server 並完成 PSK 認證
//  2. 啟動背景 goroutine 持續讀取 Server 訊息
//  3. 主動請求一次遠端主機列表
//  4. 進入 IPC 服務迴圈（阻塞至 ctx 取消）
//  5. ctx 取消後執行 shutdown 清理所有資源
func (d *Daemon) Start(ctx context.Context, ipcListener net.Listener) error {
	if err := d.connectServer(ctx); err != nil {
		return err
	}

	go d.serverReadLoop(ctx)
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

// handleIPCConn 處理單一 IPC 連線：讀取一個 JSON 指令、執行、回傳結果後關閉。
// 每個 IPC 連線有 30 秒的整體 deadline，避免慢速或斷線的 CLI 佔住資源。
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

// handleCommand 根據 IPC 指令的 Action 分派到對應的處理方法。
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

// cmdList 回傳所有綁定關係的快照（對應 IPC action: "list"）。
func (d *Daemon) cmdList() IPCResponse {
	return SuccessResponse(d.bindings.List())
}

// cmdStatus 回傳 Daemon 目前的連線狀態（對應 IPC action: "status"）。
func (d *Daemon) cmdStatus() IPCResponse {
	return SuccessResponse(StatusInfo{
		Connected: d.wsConn != nil,
		ConnID:    d.connID,
		ServerURL: d.config.ServerURL,
		BindCount: d.bindings.Count(),
	})
}

// cmdHosts 回傳快取中的遠端主機與設備列表（對應 IPC action: "hosts"）。
func (d *Daemon) cmdHosts() IPCResponse {
	d.hostsMu.RLock()
	defer d.hostsMu.RUnlock()
	return SuccessResponse(d.hosts)
}

// cmdBind 執行設備綁定流程（對應 IPC action: "bind"），共 9 個步驟：
//
//	步驟 1: 分配本機 Port — 從 PortAllocator 取得一個可用 port
//	步驟 2: 鎖定遠端設備 — 透過 Server 向 Agent 發送 lock_req，等待 lock_resp 確認
//	步驟 3: 建立 WebRTC PeerConnection — 初始化 ICE 設定
//	步驟 4: 開啟 DataChannel — 建立以 "adb/<serial>/<sessionID>" 命名的通道
//	步驟 5: 建立 Offer — 產生 SDP Offer 並透過 Server 發送給 Agent
//	步驟 6: 等待 Answer — 等待 Agent 回傳的 SDP Answer 並套用
//	步驟 7: 建立 TCP Proxy — 在本機 port 上監聽，將 TCP 流量橋接至 DataChannel
//	步驟 8: 記錄狀態 — 將 proxy/peer 存入 map，新增 binding 記錄
//	步驟 9: 監聽斷線 — 註冊 WebRTC 斷線回呼，自動更新狀態為 disconnected
//
// 任何步驟失敗時，會回收已分配的資源（port、PeerConnection 等）並回傳錯誤。
func (d *Daemon) cmdBind(ctx context.Context, payload json.RawMessage) IPCResponse {
	if d.wsConn == nil {
		return ErrorResponse("未連線到 Server")
	}

	var req BindRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return ErrorResponse("解析 bind 參數失敗")
	}

	// 前置檢查：同一設備不允許重複綁定
	if _, found := d.bindings.FindBySerial(req.Serial); found {
		return ErrorResponse(fmt.Sprintf("設備 %s 已被綁定", req.Serial))
	}

	// 步驟 1: 分配本機 port
	port, err := d.ports.Allocate()
	if err != nil {
		return ErrorResponse(err.Error())
	}

	// 步驟 2: 向 Agent 鎖定設備（透過 Server 轉發 lock_req）
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

	// 步驟 3: 建立 WebRTC PeerConnection
	pm, err := webrtc.NewPeerManager(d.config.ICEConfig)
	if err != nil {
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("建立 PeerConnection 失敗: %v", err))
	}

	// 步驟 4: 開啟 DataChannel，label 格式 "adb/<serial>/<sessionID>"
	sessionID := generateSessionID()
	label := fmt.Sprintf("adb/%s/%s", req.Serial, sessionID)
	channel, err := pm.OpenChannel(label)
	if err != nil {
		pm.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("建立 DataChannel 失敗: %v", err))
	}

	// 步驟 5: 建立 SDP Offer 並透過 Server 發送給 Agent
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

	// 步驟 6: 等待 Agent 回傳的 SDP Answer（15 秒逾時）
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

	// 步驟 7: 建立 TCP Proxy，在本機 port 監聽並橋接至 DataChannel
	p, err := proxy.New(port, channel)
	if err != nil {
		pm.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("建立 TCP 代理失敗: %v", err))
	}
	p.Start(ctx)

	// 步驟 8: 記錄 proxy/peer 與 binding 狀態
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

	// 步驟 9: 註冊 WebRTC 斷線回呼，自動將 binding 狀態更新為 "disconnected"
	pm.OnDisconnect(func() {
		slog.Info("WebRTC 連線斷開", "port", port, "serial", req.Serial)
		d.bindings.UpdateStatus(port, "disconnected")
	})

	slog.Info("綁定成功", "port", port, "serial", req.Serial, "host", req.HostID)
	return SuccessResponse(BindResult{LocalPort: port, Serial: req.Serial})
}

// cmdUnbind 解除指定 port 的設備綁定（對應 IPC action: "unbind"）：
// 停止 TCP Proxy → 關閉 PeerConnection → 通知 Agent 解鎖 → 移除 binding → 釋放 port。
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

	// 通知 Agent 解鎖設備（fire-and-forget，不等待回應）
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

// --- Server 連線 ---

// connectServer 連線到 Signal Server 並完成 PSK 認證。
// 成功後 d.wsConn 與 d.connID 會被設定。
func (d *Daemon) connectServer(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url := d.config.ServerURL + "/ws"
	conn, _, err := ws.Dial(dialCtx, url, nil)
	if err != nil {
		return fmt.Errorf("連線 Server 失敗: %w", err)
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
	slog.Info("Server 認證成功", "conn_id", d.connID)
	return nil
}

// serverReadLoop 持續從 Server WebSocket 讀取訊息並分派處理。
// 此方法在獨立 goroutine 中執行，直到 ctx 取消或讀取錯誤時結束。
func (d *Daemon) serverReadLoop(ctx context.Context) {
	for {
		_, data, err := d.wsConn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("Server 讀取失敗", "error", err)
			return
		}

		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		d.handleSignalMessage(env)
	}
}

// handleSignalMessage 根據訊息類型分派處理：
//   - host_list_resp: 更新主機列表快取
//   - device_update:  更新特定主機的設備列表
//   - lock_resp:      透過 waiters 機制回傳給等待中的 cmdBind
//   - unlock_resp:    透過 waiters 機制回傳（目前 unbind 是 fire-and-forget，無人等待）
//   - answer:         透過 waiters 機制回傳 SDP Answer 給等待中的 cmdBind
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

// updateHostDevices 更新指定主機的設備列表。
// 若該 hostID 不在快取中（例如 Agent 是在 Daemon 啟動後才上線），會先建立一筆空記錄。
func (d *Daemon) updateHostDevices(hostID string, devices []protocol.DeviceInfo) {
	d.hostsMu.Lock()
	defer d.hostsMu.Unlock()
	for i := range d.hosts {
		if d.hosts[i].HostID == hostID {
			d.hosts[i].Devices = devices
			return
		}
	}
	// 未知 host，先建立記錄（hostname 待下次 host_list_resp 補全）
	d.hosts = append(d.hosts, protocol.HostInfo{
		HostID:  hostID,
		Devices: devices,
	})
}

// sendEnvelope 將信令訊息序列化為 JSON 並透過 WebSocket 發送。
func (d *Daemon) sendEnvelope(ctx context.Context, env protocol.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return d.wsConn.Write(ctx, ws.MessageText, data)
}

// requestHostList 向 Server 請求一次遠端主機列表（fire-and-forget）。
// 回應會在 serverReadLoop 中由 handleSignalMessage 處理。
func (d *Daemon) requestHostList(ctx context.Context) {
	env, _ := protocol.NewEnvelope(
		protocol.MsgTypeHostList, d.hostname, d.connID, "",
		struct{}{},
	)
	d.sendEnvelope(ctx, env)
}

// waitResponse 註冊一個等待者並阻塞等待回應。
// 流程：建立 buffered channel → 存入 waiters map → 等待 deliverResponse 寫入或逾時。
// 無論成功或逾時，defer 都會清理 waiters map 中的記錄。
func (d *Daemon) waitResponse(key string, timeout time.Duration) (protocol.Envelope, error) {
	// 使用 buffered channel（容量 1），避免 deliverResponse 在寫入時阻塞
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

// deliverResponse 將收到的回應寫入對應的等待者 channel。
// 使用 select + default 確保即使無人等待（已逾時清理）也不會阻塞。
func (d *Daemon) deliverResponse(key string, env protocol.Envelope) {
	d.waiterMu.Lock()
	ch, ok := d.waiters[key]
	d.waiterMu.Unlock()
	if ok {
		select {
		case ch <- env:
		default:
			// 無人等待（可能已逾時），丟棄回應
		}
	}
}

// shutdown 清理所有資源：停止所有 TCP Proxy、關閉所有 PeerConnection、關閉 WebSocket 連線。
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

// generateSessionID 產生 16 字元的隨機十六進位字串，用於 DataChannel label 的唯一識別。
func generateSessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
