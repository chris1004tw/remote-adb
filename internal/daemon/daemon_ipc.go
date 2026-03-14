package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/chris1004tw/remote-adb/internal/proxy"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
)

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
			slog.Debug("IPC accept failed", "error", err)
			return
		}
		go d.handleIPCConn(ctx, conn)
	}
}

// handleIPCConn 處理單一 IPC 連線：讀取一個 JSON 指令、執行、回傳結果後關閉。
// 每個 IPC 連線有 ipcDeadline（50s）的整體 deadline，須涵蓋 cmdBind 的
// lock（10s）+ ICE gathering（bindGatherTimeout 15s）+ answer（15s）。
func (d *Daemon) handleIPCConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(ipcDeadline))

	var cmd IPCCommand
	if err := json.NewDecoder(conn).Decode(&cmd); err != nil {
		json.NewEncoder(conn).Encode(ErrorResponse("failed to parse command"))
		return
	}

	slog.Debug("received IPC command", "action", cmd.Action)

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
		return ErrorResponse(fmt.Sprintf("unknown command: %s", cmd.Action))
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
//	步驟 7: 建立 TCP Proxy — 在本機 port 上監聯，將 TCP 流量橋接至 DataChannel
//	步驟 8: 記錄狀態 — 將 proxy/peer 存入 map，新增 binding 記錄
//	步驟 9: 監聽斷線 — 註冊 WebRTC 斷線回呼，自動更新狀態為 disconnected
//
// 任何步驟失敗時，會回收已分配的資源（port、PeerConnection 等）並回傳錯誤。
func (d *Daemon) cmdBind(ctx context.Context, payload json.RawMessage) IPCResponse {
	if d.wsConn == nil {
		return ErrorResponse("not connected to server")
	}

	var req BindRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return ErrorResponse("failed to parse bind parameters")
	}

	// 前置檢查：同一設備不允許重複綁定
	if _, found := d.bindings.FindBySerial(req.Serial); found {
		return ErrorResponse(fmt.Sprintf("device %s already bound", req.Serial))
	}

	// 步驟 1: 分配本機 port（使用 AllocateListener 避免 TOCTOU 風險）
	proxyLn, port, err := d.ports.AllocateListener()
	if err != nil {
		return ErrorResponse(err.Error())
	}

	// 步驟 2: 向 Agent 鎖定設備（透過 Server 轉發 lock_req）
	lockEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeLockReq, d.hostname, d.connID, req.HostID,
		protocol.LockReqPayload{HostID: req.HostID, Serial: req.Serial},
	)
	if err := d.sendEnvelope(ctx, lockEnv); err != nil {
		proxyLn.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("failed to send lock request: %v", err))
	}

	lockResp, err := d.waitResponse("lock_resp:"+req.Serial, 10*time.Second)
	if err != nil {
		proxyLn.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("lock response wait failed: %v", err))
	}

	var lockPayload protocol.LockRespPayload
	if err := lockResp.DecodePayload(&lockPayload); err != nil || !lockPayload.Success {
		proxyLn.Close()
		d.ports.Release(port)
		reason := "lock failed"
		if lockPayload.Reason != "" {
			reason = lockPayload.Reason
		}
		return ErrorResponse(reason)
	}

	// 步驟 3: 建立 WebRTC PeerConnection
	pm, err := webrtc.NewPeerManager(d.config.ICEConfig)
	if err != nil {
		proxyLn.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("failed to create PeerConnection: %v", err))
	}

	// 步驟 4: 開啟 DataChannel，label 格式 "adb/<serial>/<sessionID>"
	sessionID := generateSessionID()
	label := fmt.Sprintf("adb/%s/%s", req.Serial, sessionID)
	channel, err := pm.OpenChannel(label)
	if err != nil {
		pm.Close()
		proxyLn.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("failed to create DataChannel: %v", err))
	}

	// 步驟 5: 建立 SDP Offer 並透過 Server 發送給 Agent
	// 使用 bindGatherTimeout 限制 ICE gathering 時間，避免佔用整個 IPC deadline。
	offerSDP, err := pm.CreateOfferWithGatherTimeout(bindGatherTimeout)
	if err != nil {
		pm.Close()
		proxyLn.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("failed to create offer: %v", err))
	}

	offerEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeOffer, d.hostname, d.connID, req.HostID,
		protocol.SDPPayload{SDP: offerSDP, Type: "offer"},
	)
	if err := d.sendEnvelope(ctx, offerEnv); err != nil {
		pm.Close()
		proxyLn.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("failed to send offer: %v", err))
	}

	// 步驟 6: 等待 Agent 回傳的 SDP Answer（15 秒逾時）
	answerEnv, err := d.waitResponse("answer:"+req.HostID, 15*time.Second)
	if err != nil {
		pm.Close()
		proxyLn.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("answer wait failed: %v", err))
	}

	var answerPayload protocol.SDPPayload
	if err := answerEnv.DecodePayload(&answerPayload); err != nil {
		pm.Close()
		proxyLn.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("failed to parse answer: %v", err))
	}

	if err := pm.HandleAnswer(answerPayload.SDP); err != nil {
		pm.Close()
		proxyLn.Close()
		d.ports.Release(port)
		return ErrorResponse(fmt.Sprintf("failed to handle answer: %v", err))
	}

	// 步驟 7: 使用已分配的 listener 建立 TCP Proxy，橋接至 DataChannel。
	// 直接傳入步驟 1 取得的 proxyLn，避免 TOCTOU 風險。
	p := proxy.NewWithListener(proxyLn, channel)
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

	// 步驟 9: 註冊 WebRTC 斷線回呼，執行完整資源清理。
	// 整體在獨立 goroutine 中執行，避免：
	//  (1) pion OnConnectionStateChange callback 內呼叫 pm.Close() 的重入風險
	//  (2) p.Stop() 阻塞 pion 的狀態通知 goroutine
	// H24 已確保 onDisconnectFn 最多觸發一次，此處不需額外防重入。
	pm.OnDisconnect(func() {
		slog.Info("WebRTC disconnected, cleaning up", "port", port, "serial", req.Serial)
		d.bindings.UpdateStatus(port, "disconnected")

		go func() {
			// 鎖內擷取引用並從 map 移除
			d.proxyMu.Lock()
			p := d.proxies[port]
			peerRef := d.peers[port]
			delete(d.proxies, port)
			delete(d.peers, port)
			d.proxyMu.Unlock()

			// 鎖外關閉：p.Stop() 等待 accept loop 結束，peerRef.Close() 執行 DTLS teardown
			if p != nil {
				p.Stop()
			}
			if peerRef != nil {
				peerRef.Close()
			}

			d.ports.Release(port)
			slog.Info("WebRTC disconnect cleanup done", "port", port, "serial", req.Serial)
		}()
	})

	slog.Info("bind succeeded", "port", port, "serial", req.Serial, "host", req.HostID)
	return SuccessResponse(BindResult{LocalPort: port, Serial: req.Serial})
}

// cmdUnbind 解除指定 port 的設備綁定（對應 IPC action: "unbind"）：
// 停止 TCP Proxy → 關閉 PeerConnection → 通知 Agent 解鎖 → 移除 binding → 釋放 port。
func (d *Daemon) cmdUnbind(ctx context.Context, payload json.RawMessage) IPCResponse {
	var req UnbindRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return ErrorResponse("failed to parse unbind parameters")
	}

	binding, ok := d.bindings.Get(req.LocalPort)
	if !ok {
		return ErrorResponse(fmt.Sprintf("port %d not bound", req.LocalPort))
	}

	// 鎖內擷取引用並從 map 移除，鎖外關閉——
	// 避免持有 proxyMu 期間呼叫 p.Stop()（等待 accept loop）
	// 和 pm.Close()（DTLS teardown），多台設備時耗時累加會阻塞所有 IPC 請求。
	d.proxyMu.Lock()
	p := d.proxies[req.LocalPort]
	pm := d.peers[req.LocalPort]
	delete(d.proxies, req.LocalPort)
	delete(d.peers, req.LocalPort)
	d.proxyMu.Unlock()

	if p != nil {
		p.Stop()
	}
	if pm != nil {
		pm.Close()
	}

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

	slog.Info("unbind succeeded", "port", req.LocalPort, "serial", binding.Serial)
	return SuccessResponse(nil)
}
