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
type Conn struct {
	id       string
	role     protocol.Role
	hostname string
	ws       *websocket.Conn

	sendCh chan protocol.Envelope
	done   chan struct{}
	once   sync.Once
}

// NewConn 建立一個新的連線封裝。
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
// 應在獨立的 goroutine 中執行。
func (c *Conn) WritePump(ctx context.Context) {
	defer c.Close()
	for {
		select {
		case msg := <-c.sendCh:
			data, err := json.Marshal(msg)
			if err != nil {
				slog.Error("序列化訊息失敗", "error", err)
				continue
			}
			if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
				slog.Debug("寫入 WebSocket 失敗", "conn_id", c.id, "error", err)
				return
			}
		case <-ctx.Done():
			return
		case <-c.done:
			return
		}
	}
}

// ReadMessage 從 WebSocket 讀取一筆訊息並解析為 Envelope。
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

// Close 關閉連線。可安全地多次呼叫。
func (c *Conn) Close() {
	c.once.Do(func() {
		close(c.done)
		c.ws.CloseNow()
	})
}

// Done 回傳一個在連線關閉時會被關閉的 channel。
func (c *Conn) Done() <-chan struct{} {
	return c.done
}
