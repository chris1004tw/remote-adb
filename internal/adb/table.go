package adb

import (
	"sync"
)

// DeviceInfo 單一設備的完整資訊（含鎖定狀態）。
type DeviceInfo struct {
	Serial   string
	State    string // "device", "offline"
	Lock     string // "available", "locked"
	LockedBy string
}

// DeviceTable 執行緒安全的設備狀態表。
// 以設備序號 (Serial Number) 為鍵值，記錄硬體狀態與鎖定狀態。
type DeviceTable struct {
	mu      sync.RWMutex
	devices map[string]*deviceEntry
}

type deviceEntry struct {
	state    string // 硬體狀態
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
