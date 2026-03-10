// Package signal — 連線封裝模組。
//
// 本檔案將原始的 WebSocket 連線封裝為 Conn，提供以下關鍵特性：
//
//  1. 非阻塞發送：透過有界佇列（sendCh）實現，避免慢速 Client 拖慢整體伺服器。
//     當佇列滿時直接丟棄訊息而非阻塞，確保伺服器的訊息路由不會因單一慢速連線而卡住。
//
//  2. 讀寫分離：讀取在呼叫端的 goroutine（readLoop）中進行，
//     寫入由獨立的 WritePump goroutine 負責。
//     這是 WebSocket 的常見模式，因為 websocket.Conn 不支援並行寫入，
//     透過 WritePump 序列化所有寫入操作，避免競態條件。
//
//  3. 安全關閉：透過 sync.Once 保證 Close 只執行一次，搭配 done channel
//     通知所有相關的 goroutine 優雅退出。
package signal

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/chris1004tw/remote-adb/pkg/protocol"
	"github.com/coder/websocket"
)

// Conn 封裝一條 WebSocket 連線，提供非阻塞的訊息發送。
// 每條 Conn 對應一個 Agent 或 Client 的 WebSocket 連線。
type Conn struct {
	id       string          // 伺服器分配的唯一 ID（格式："{role}-{hex}"）
	role     protocol.Role   // 連線端角色：agent 或 client
	hostname string          // 連線端的主機名稱（用於顯示）
	ws       *websocket.Conn // 底層 WebSocket 連線

	sendCh chan protocol.Envelope // 發送佇列（容量 64），WritePump 從此 channel 取出訊息寫入 WebSocket
	done   chan struct{}          // 連線關閉信號，所有相關 goroutine 透過此 channel 感知關閉事件
	once   sync.Once             // 確保 Close 只執行一次，避免重複關閉 channel 導致 panic
}

// NewConn 建立一個新的連線封裝。
// sendCh 的容量設為 64，為訊息突發（例如大量設備更新同時到達）提供緩衝。
func NewConn(id string, role protocol.Role, hostname string, ws *websocket.Conn) *Conn {
	return &Conn{
		id:       id,
		role:     role,
		hostname: hostname,
		ws:       ws,
		sendCh:   make(chan protocol.Envelope, 64),
		done:     make(chan struct{}),
	}
}

// ID 回傳連線的唯一識別碼。
func (c *Conn) ID() string { return c.id }

// Role 回傳連線端的角色。
func (c *Conn) Role() protocol.Role { return c.role }

// Hostname 回傳連線端的主機名稱。
func (c *Conn) Hostname() string { return c.hostname }

// Send 將訊息放入發送佇列（非阻塞）。
// 如果佇列已滿，訊息會被丟棄並記錄警告。
//
// 設計決策：使用 select + default 實現非阻塞發送。
// 三種情況：
//   - sendCh 有空位：訊息成功排入佇列
//   - done 已關閉：連線已斷開，靜默丟棄
//   - default：佇列已滿，丟棄訊息並記錄警告
//
// 之所以選擇「佇列滿時丟棄」而非「阻塞等待」，是因為信令伺服器需要同時服務
// 大量連線，若某個慢速 Client 的佇列阻塞，會連帶拖慢其他所有連線的訊息派發。
func (c *Conn) Send(msg protocol.Envelope) {
	select {
	case c.sendCh <- msg:
	case <-c.done:
	default:
		slog.Warn("發送佇列已滿，丟棄訊息",
			"conn_id", c.id,
			"msg_type", msg.Type,
		)
	}
}

// WritePump 持續從發送佇列取出訊息並寫入 WebSocket。
// 必須在獨立的 goroutine 中執行。
//
// 為什麼需要獨立的 WritePump goroutine？
//   - WebSocket 連線不支援並行寫入（concurrent writes），所有寫入必須序列化
//   - readLoop 在主 goroutine 中阻塞讀取，若在 readLoop 中直接寫入會造成死鎖風險
//   - WritePump 作為唯一的寫入者，從 sendCh 依序取出訊息寫入，保證執行緒安全
//
// 退出條件（任一觸發即結束）：
//   - WebSocket 寫入失敗（連線斷開）
//   - context 取消（HTTP 請求結束）
//   - done channel 關閉（主動 Close）
//
// 退出時會呼叫 c.Close() 確保資源釋放。
func (c *Conn) WritePump(ctx context.Context) {
	defer c.Close()
	for {
		select {
		case msg := <-c.sendCh:
			data, err := json.Marshal(msg)
			if err != nil {
				slog.Error("序列化訊息失敗", "error", err)
				continue // 序列化失敗不應中斷整個寫入迴圈
			}
			if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
				slog.Debug("寫入 WebSocket 失敗", "conn_id", c.id, "error", err)
				return // 寫入失敗表示連線已斷，退出迴圈
			}
		case <-ctx.Done():
			return
		case <-c.done:
			return
		}
	}
}

// ReadMessage 從 WebSocket 讀取一筆訊息並解析為 Envelope。
// 此方法會阻塞直到收到訊息或發生錯誤。
// 僅由 readLoop 呼叫，無並行讀取問題。
func (c *Conn) ReadMessage(ctx context.Context) (protocol.Envelope, error) {
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}

	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return protocol.Envelope{}, err
	}
	return env, nil
}

// Close 關閉連線，釋放所有相關資源。
// 透過 sync.Once 保證冪等性，可安全地多次呼叫而不會 panic。
// 關閉順序：先關閉 done channel（通知 WritePump 和 Send 退出），再關閉底層 WebSocket。
func (c *Conn) Close() {
	c.once.Do(func() {
		close(c.done)    // 通知所有監聽 done 的 goroutine
		c.ws.CloseNow()  // 立即關閉底層 WebSocket 連線
	})
}

// Done 回傳一個在連線關閉時會被關閉的 channel。
// 外部可透過 <-conn.Done() 等待連線關閉事件。
func (c *Conn) Done() <-chan struct{} {
	return c.done
}
