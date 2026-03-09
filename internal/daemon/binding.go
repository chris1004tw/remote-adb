package daemon

import (
	"fmt"
	"sync"
)

// Binding 代表一個「本機 port ↔ 遠端設備」的綁定關係。
type Binding struct {
	LocalPort int    `json:"local_port"`
	HostID    string `json:"host_id"`
	Serial    string `json:"serial"`
	Status    string `json:"status"` // "connecting", "active", "disconnected"
}

// BindingTable 管理所有綁定關係。
type BindingTable struct {
	mu       sync.RWMutex
	bindings map[int]*Binding // key: local port
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
		return fmt.Errorf("port %d 已被綁定", b.LocalPort)
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

// List 回傳所有綁定的快照。
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
