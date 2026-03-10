package adb

import (
	"sync"
)

// DeviceInfo 單一設備的完整資訊（含鎖定狀態），用於對外暴露的快照。
type DeviceInfo struct {
	Serial   string
	State    string // "device"（可用）, "offline"（離線）
	Lock     string // "available"（可綁定）, "locked"（已被某 client 獨佔）
	LockedBy string // 鎖定者的 ID（Client 連線 ID 或 IP 位址）
}

// DeviceTable 執行緒安全的設備狀態表。
// 以設備序號 (Serial Number) 為鍵值，記錄硬體狀態與鎖定狀態。
//
// 設計目的：防止多個 Client 同時控制同一支手機，造成 ADB 指令交錯。
// 一個設備同一時間只能被一個 Client 鎖定（排他鎖）。
// 當 Client 斷線時，Agent 會呼叫 UnlockAll 自動釋放該 Client 持有的所有鎖。
//
// 此表由 Agent 端維護，被 Signal Server 模式和 Direct 模式共享。
type DeviceTable struct {
	mu      sync.RWMutex
	devices map[string]*deviceEntry
}

// deviceEntry 內部使用的設備記錄。
// 使用私有結構避免外部直接修改鎖定狀態，必須透過 Lock/Unlock 方法操作。
type deviceEntry struct {
	state    string // 硬體狀態（來自 ADB track-devices）
	locked   bool
	lockedBy string
}

// NewDeviceTable 建立一個新的空設備狀態表。
func NewDeviceTable() *DeviceTable {
	return &DeviceTable{
		devices: make(map[string]*deviceEntry),
	}
}

// Update 根據 track-devices 回傳的事件，更新設備列表。
// 新出現的設備預設為未鎖定；已消失的設備會被移除（同時釋放鎖定）。
// 已存在的設備只更新硬體狀態，保留鎖定狀態。
func (dt *DeviceTable) Update(events []DeviceEvent) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	// 收集本次有出現的序號
	seen := make(map[string]bool, len(events))
	for _, ev := range events {
		seen[ev.Serial] = true
		if entry, ok := dt.devices[ev.Serial]; ok {
			// 設備已存在，僅更新硬體狀態
			entry.state = ev.State
		} else {
			// 新設備
			dt.devices[ev.Serial] = &deviceEntry{
				state: ev.State,
			}
		}
	}

	// 移除已拔除的設備
	for serial := range dt.devices {
		if !seen[serial] {
			delete(dt.devices, serial)
		}
	}
}

// List 回傳當前所有設備及其鎖定狀態的快照。
func (dt *DeviceTable) List() []DeviceInfo {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	result := make([]DeviceInfo, 0, len(dt.devices))
	for serial, entry := range dt.devices {
		lock := "available"
		if entry.locked {
			lock = "locked"
		}
		result = append(result, DeviceInfo{
			Serial:   serial,
			State:    entry.state,
			Lock:     lock,
			LockedBy: entry.lockedBy,
		})
	}
	return result
}

// Lock 鎖定指定設備。回傳是否成功。
// 若設備不存在、已離線、或已被鎖定，回傳 false。
func (dt *DeviceTable) Lock(serial, clientID string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	entry, ok := dt.devices[serial]
	if !ok {
		return false
	}
	if entry.state != "device" {
		return false
	}
	if entry.locked {
		return false
	}

	entry.locked = true
	entry.lockedBy = clientID
	return true
}

// Unlock 解鎖指定設備。只有持鎖者可以解鎖。
// 回傳是否成功。
func (dt *DeviceTable) Unlock(serial, clientID string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	entry, ok := dt.devices[serial]
	if !ok {
		return false
	}
	if !entry.locked || entry.lockedBy != clientID {
		return false
	}

	entry.locked = false
	entry.lockedBy = ""
	return true
}

// UnlockAll 解鎖指定 client 持有的所有設備。
// 用於 client 異常斷線時的清理。
func (dt *DeviceTable) UnlockAll(clientID string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for _, entry := range dt.devices {
		if entry.locked && entry.lockedBy == clientID {
			entry.locked = false
			entry.lockedBy = ""
		}
	}
}

// IsLocked 查詢設備是否被鎖定，以及鎖定者是誰。
func (dt *DeviceTable) IsLocked(serial string) (bool, string) {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	entry, ok := dt.devices[serial]
	if !ok {
		return false, ""
	}
	return entry.locked, entry.lockedBy
}
