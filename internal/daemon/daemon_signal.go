package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/chris1004tw/remote-adb/pkg/protocol"
	ws "github.com/coder/websocket"
)

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
	slog.Info("server auth succeeded", "conn_id", d.connID)
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
			slog.Error("server read failed", "error", err)
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
		slog.Debug("unhandled message", "type", env.Type)
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
