package signal

import (
	"log/slog"
	"sync"

	"github.com/chris1004tw/remote-adb/pkg/protocol"
)

// Hub 管理所有 WebSocket 連線與訊息路由。
type Hub struct {
	mu    sync.RWMutex
	conns map[string]*Conn // key: conn ID

	// agents 記錄已註冊的 Agent 的最新主機資訊。
	agents map[string]protocol.HostInfo // key: conn ID
}

// NewHub 建立一個新的 Hub。
func NewHub() *Hub {
	return &Hub{
		conns:  make(map[string]*Conn),
		agents: make(map[string]protocol.HostInfo),
	}
}

// Register 將連線加入 Hub。
func (h *Hub) Register(conn *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[conn.ID()] = conn
	slog.Info("連線已註冊", "conn_id", conn.ID(), "role", conn.Role())
}

// Unregister 將連線從 Hub 移除，並清理相關的 Agent 資訊。
func (h *Hub) Unregister(connID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if conn, ok := h.conns[connID]; ok {
		conn.Close()
		delete(h.conns, connID)
	}
	delete(h.agents, connID)
	slog.Info("連線已移除", "conn_id", connID)
}

// Route 將訊息路由到目標連線。
// 如果 TargetID 為空，回傳 false。
// 如果目標不存在，回傳 false。
func (h *Hub) Route(msg protocol.Envelope) bool {
	if msg.TargetID == "" {
		return false
	}

	h.mu.RLock()
	target, ok := h.conns[msg.TargetID]
	h.mu.RUnlock()

	if !ok {
		slog.Debug("路由目標不存在", "target_id", msg.TargetID)
		return false
	}

	target.Send(msg)
	return true
}

// RegisterAgent 記錄 Agent 的主機資訊。
func (h *Hub) RegisterAgent(connID string, info protocol.HostInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[connID] = info
	slog.Info("Agent 已註冊", "conn_id", connID, "host_id", info.HostID, "hostname", info.Hostname)
}

// UpdateAgentDevices 更新指定 Agent 的設備列表。
func (h *Hub) UpdateAgentDevices(connID string, devices []protocol.DeviceInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if info, ok := h.agents[connID]; ok {
		info.Devices = devices
		h.agents[connID] = info
	}
}

// Agents 回傳所有已註冊 Agent 的主機資訊快照。
func (h *Hub) Agents() []protocol.HostInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]protocol.HostInfo, 0, len(h.agents))
	for _, info := range h.agents {
		result = append(result, info)
	}
	return result
}

// GetConn 根據 ID 取得連線。
func (h *Hub) GetConn(connID string) (*Conn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	conn, ok := h.conns[connID]
	return conn, ok
}

// BroadcastToClients 將訊息廣播給所有 Client 角色的連線。
func (h *Hub) BroadcastToClients(msg protocol.Envelope) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, conn := range h.conns {
		if conn.Role() == protocol.RoleClient {
			conn.Send(msg)
		}
	}
}

// ConnCount 回傳當前連線數量。
func (h *Hub) ConnCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}
