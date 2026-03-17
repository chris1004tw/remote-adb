// connect.go 提供客戶端（Agent / Daemon）連線到 Signal Server 的共用認證流程。
//
// Agent 和 Daemon 的 connectServer 邏輯高度一致：
// WebSocket Dial → 發送 auth → 讀取 auth_ack → 驗證成功 → 取得 connID。
// ConnectAndAuth 將此流程統一為單一函式，避免重複實作。
package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/chris1004tw/remote-adb/pkg/protocol"
	ws "github.com/coder/websocket"
)

// ConnectAndAuth 連線到 Signal Server 並完成 PSK 認證。
//
// 流程：
//  1. WebSocket Dial 至 serverURL/ws（10 秒超時）
//  2. 發送 auth 訊息（含 token 與 role 角色標識）
//  3. 等待 auth_ack 回應，驗證成功後回傳連線與分配的 connID
//
// 呼叫端負責後續使用 conn（讀寫 + 關閉）。
func ConnectAndAuth(ctx context.Context, serverURL, hostname, token string, role protocol.Role) (*ws.Conn, string, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url := serverURL + "/ws"
	conn, _, err := ws.Dial(dialCtx, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to connect to server: %w", err)
	}

	// 發送認證訊息
	authEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeAuth, hostname, "temp", "",
		protocol.AuthPayload{Token: token, Role: role},
	)
	data, err := json.Marshal(authEnv)
	if err != nil {
		conn.CloseNow()
		return nil, "", fmt.Errorf("failed to marshal auth message: %w", err)
	}
	if err := conn.Write(ctx, ws.MessageText, data); err != nil {
		conn.CloseNow()
		return nil, "", fmt.Errorf("failed to send auth message: %w", err)
	}

	// 讀取 auth_ack
	_, respData, err := conn.Read(ctx)
	if err != nil {
		conn.CloseNow()
		return nil, "", fmt.Errorf("failed to read auth response: %w", err)
	}

	var ack protocol.Envelope
	if err := json.Unmarshal(respData, &ack); err != nil {
		conn.CloseNow()
		return nil, "", fmt.Errorf("failed to parse auth response: %w", err)
	}

	var ackPayload protocol.AuthAckPayload
	if err := ack.DecodePayload(&ackPayload); err != nil {
		conn.CloseNow()
		return nil, "", fmt.Errorf("failed to decode auth ack: %w", err)
	}
	if !ackPayload.Success {
		conn.CloseNow()
		return nil, "", fmt.Errorf("auth failed: %s", ackPayload.Reason)
	}

	slog.Info("server auth succeeded", "conn_id", ackPayload.AssignID)
	return conn, ackPayload.AssignID, nil
}
