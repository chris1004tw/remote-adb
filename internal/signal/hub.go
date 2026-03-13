// Package signal — Hub 中央路由模組。
//
// Hub 是信令伺服器的核心元件，扮演「中央路由器」的角色：
//   - 維護所有活躍的 WebSocket 連線清冊（conns map）
//   - 維護已註冊 Agent 的設備清冊（agents map），記錄每個 Agent 掛載了哪些 Android 設備
//   - 提供點對點訊息路由（Route）與廣播功能（BroadcastToClients）
//
// Hub 的所有操作都透過 sync.RWMutex 保護，確保在多個 goroutine
// （每條 WebSocket 連線的 readLoop 與 WritePump）並行存取時的執行緒安全。
//
// 設備清冊的維護流程：
//  1. Agent 連線後發送 register → Hub.RegisterAgent() 建立初始清冊
//  2. Agent 偵測到設備插拔 → Hub.UpdateAgentDevices() 更新清冊
//  3. Agent 斷線 → Hub.Unregister() 自動清理對應的設備清冊
//
// 每次設備清冊變更後，Server 會呼叫 BroadcastToClients 將最新狀態推送給所有 Client。
package signal

import (
	"log/slog"
	"sync"

	"github.com/chris1004tw/remote-adb/pkg/protocol"
)

// Hub 管理所有 WebSocket 連線與訊息路由，是信令伺服器的中央路由器。
type Hub struct {
	mu    sync.RWMutex                 // 保護 conns 與 agents 的讀寫鎖
	conns map[string]*Conn             // 所有活躍連線，key 為伺服器分配的 conn ID

	// agents 記錄已註冊 Agent 的主機資訊與設備清單。
	// 僅包含已發送 register 訊息的 Agent（剛認證但尚未 register 的不在此 map 中）。
	agents map[string]protocol.HostInfo // key: conn ID
}

// NewHub 建立一個新的 Hub，初始化空的連線清冊與 Agent 設備清冊。
func NewHub() *Hub {
	return &Hub{
		conns:  make(map[string]*Conn),
		agents: make(map[string]protocol.HostInfo),
	}
}

// Register 將連線加入 Hub 的連線清冊。
// 在認證成功後由 Server.handleWebSocket 呼叫。
func (h *Hub) Register(conn *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[conn.ID()] = conn
	slog.Info("connection registered", "conn_id", conn.ID(), "role", conn.Role())
}

// Unregister 將連線從 Hub 移除，同時清理該連線對應的 Agent 設備清冊。
// 透過 defer 機制在連線斷開時自動呼叫，確保不會殘留無效的連線或過期的設備資訊。
// 對於 Client 角色，delete(h.agents, connID) 是無害的（key 不存在時 delete 為 no-op）。
func (h *Hub) Unregister(connID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if conn, ok := h.conns[connID]; ok {
		conn.Close()
		delete(h.conns, connID)
	}
	// 不論角色，統一清理 agents map（Client 角色本身就不在 agents 中，delete 為 no-op）
	delete(h.agents, connID)
	slog.Info("connection removed", "conn_id", connID)
}

// Route 將訊息路由到指定的目標連線（點對點轉發）。
// 根據訊息中的 TargetID 查找目標 Conn 並投遞訊息。
// 回傳 false 表示路由失敗（TargetID 為空或目標不在線），呼叫端可據此回傳錯誤給發送者。
func (h *Hub) Route(msg protocol.Envelope) bool {
	if msg.TargetID == "" {
		return false
	}

	h.mu.RLock()
	target, ok := h.conns[msg.TargetID]
	h.mu.RUnlock()

	if !ok {
		slog.Debug("route target not found", "target_id", msg.TargetID)
		return false
	}

	target.Send(msg)
	return true
}

// RegisterAgent 將 Agent 的主機資訊記錄到設備清冊。
// 在 Agent 發送 register 訊息後由 Server.handleRegister 呼叫。
// 此操作之後 Server 會觸發 BroadcastToClients，通知所有 Client 新 Agent 上線。
func (h *Hub) RegisterAgent(connID string, info protocol.HostInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[connID] = info
	slog.Info("agent registered", "conn_id", connID, "host_id", info.HostID, "hostname", info.Hostname)
}

// UpdateAgentDevices 更新指定 Agent 的設備列表。
// 在 Agent 發送 device_update 訊息後呼叫。
// 僅更新已在 agents map 中的 Agent；若 Agent 尚未 register，更新會被靜默忽略。
func (h *Hub) UpdateAgentDevices(connID string, devices []protocol.DeviceInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if info, ok := h.agents[connID]; ok {
		info.Devices = devices
		h.agents[connID] = info // HostInfo 是值類型，修改後需寫回 map
	}
}

// Agents 回傳所有已註冊 Agent 的主機資訊快照（snapshot）。
// 回傳的是一份副本，呼叫端可安全使用而不需擔心後續的並行修改。
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
// 回傳值的第二個布林值表示是否找到。
func (h *Hub) GetConn(connID string) (*Conn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	conn, ok := h.conns[connID]
	return conn, ok
}

// BroadcastToClients 將訊息廣播給所有 Client 角色的連線。
// 僅向 Client 廣播（不向 Agent 廣播），因為 Agent 不需要知道其他 Agent 的狀態。
//
// 觸發時機：
//   - Agent 發送 register → 新 Agent 上線，Client 需更新主機列表
//   - Agent 發送 device_update → 設備插拔，Client 需更新設備清單
//   - Agent 斷線 → Unregister 後，Server 可選擇性廣播通知 Client
//
// 注意：每個 conn.Send 都是非阻塞的，不會因某個慢速 Client 而影響其他 Client 的廣播。
func (h *Hub) BroadcastToClients(msg protocol.Envelope) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, conn := range h.conns {
		if conn.Role() == protocol.RoleClient {
			conn.Send(msg)
		}
	}
}

// ConnCount 回傳當前活躍的連線數量（包含 Agent 與 Client）。
// 主要用於監控和測試。
func (h *Hub) ConnCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}
