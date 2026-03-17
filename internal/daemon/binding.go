// binding.go 定義綁定表（BindingTable），追蹤本機 port 與遠端 ADB 設備的對應關係。
package daemon

import (
	"fmt"
	"sync"
)

// Binding 代表一個「本機 port <-> 遠端設備」的綁定關係。
//
// Status 狀態轉換：
//   - "connecting": cmdBind 流程進行中（分配 port 後、WebRTC 連線完成前）
//   - "active":     WebRTC DataChannel 建立完成，TCP Proxy 正在運作
//   - "disconnected": WebRTC 連線斷開（由 PeerManager.OnDisconnect 回呼觸發）
//
// 狀態只會往後轉換：connecting → active → disconnected，不會反向。
type Binding struct {
	LocalPort int    `json:"local_port"` // 本機監聽的 TCP port（adb client 連線用）
	HostID    string `json:"host_id"`    // 遠端 Agent 的 host ID
	Serial    string `json:"serial"`     // Android 設備序號
	Status    string `json:"status"`     // 綁定狀態："connecting", "active", "disconnected"
}

// BindingTable 管理所有綁定關係，使用 RWMutex 實現並行安全的讀寫存取。
// 以本機 port 為主鍵，確保同一 port 不會被重複綁定。
type BindingTable struct {
	mu       sync.RWMutex
	bindings map[int]*Binding // key: 本機 port → 綁定記錄
}

// NewBindingTable 建立一個新的 BindingTable。
func NewBindingTable() *BindingTable {
	return &BindingTable{
		bindings: make(map[int]*Binding),
	}
}

// Add 新增一筆綁定。
func (bt *BindingTable) Add(b Binding) error {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if _, exists := bt.bindings[b.LocalPort]; exists {
		return fmt.Errorf("port %d already bound", b.LocalPort)
	}
	bt.bindings[b.LocalPort] = &b
	return nil
}

// Remove 移除指定 port 的綁定。
func (bt *BindingTable) Remove(localPort int) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	delete(bt.bindings, localPort)
}

// Get 取得指定 port 的綁定。
func (bt *BindingTable) Get(localPort int) (Binding, bool) {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	b, ok := bt.bindings[localPort]
	if !ok {
		return Binding{}, false
	}
	return *b, true
}

// List 回傳所有綁定的快照（值拷貝），呼叫者可安全使用而不影響內部狀態。
func (bt *BindingTable) List() []Binding {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	result := make([]Binding, 0, len(bt.bindings))
	for _, b := range bt.bindings {
		result = append(result, *b)
	}
	return result
}

// UpdateStatus 更新綁定的狀態。
func (bt *BindingTable) UpdateStatus(localPort int, status string) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if b, ok := bt.bindings[localPort]; ok {
		b.Status = status
	}
}

// FindBySerial 根據設備序號尋找綁定。
// 此方法用於防止重複綁定同一設備：cmdBind 在開始流程前會呼叫此方法，
// 若該 serial 已存在任何綁定（不論狀態），即拒絕重複建立。
// 這確保了同一時間一個設備只會對應到一個本機 port。
func (bt *BindingTable) FindBySerial(serial string) (Binding, bool) {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	for _, b := range bt.bindings {
		if b.Serial == serial {
			return *b, true
		}
	}
	return Binding{}, false
}

// Count 回傳綁定數量。
func (bt *BindingTable) Count() int {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	return len(bt.bindings)
}
