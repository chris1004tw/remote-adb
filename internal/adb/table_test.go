package adb_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/chris1004tw/remote-adb/internal/adb"
)

func TestDeviceTable_Update_AddsNewDevices(t *testing.T) {
	dt := adb.NewDeviceTable()
	dt.Update([]adb.DeviceEvent{
		{Serial: "DEV001", State: "device"},
		{Serial: "DEV002", State: "offline"},
	})

	list := dt.List()
	if len(list) != 2 {
		t.Fatalf("設備數量 = %d, 預期 2", len(list))
	}
}

func TestDeviceTable_Update_RemovesDisappearedDevices(t *testing.T) {
	dt := adb.NewDeviceTable()

	// 初始有兩個設備
	dt.Update([]adb.DeviceEvent{
		{Serial: "DEV001", State: "device"},
		{Serial: "DEV002", State: "device"},
	})

	// 更新後只剩一個
	dt.Update([]adb.DeviceEvent{
		{Serial: "DEV001", State: "device"},
	})

	list := dt.List()
	if len(list) != 1 {
		t.Fatalf("設備數量 = %d, 預期 1", len(list))
	}
	if list[0].Serial != "DEV001" {
		t.Errorf("Serial = %q, 預期 %q", list[0].Serial, "DEV001")
	}
}

func TestDeviceTable_Update_PreservesLockOnExistingDevice(t *testing.T) {
	dt := adb.NewDeviceTable()
	dt.Update([]adb.DeviceEvent{{Serial: "DEV001", State: "device"}})
	dt.Lock("DEV001", "client-1")

	// 更新硬體狀態，鎖定應保留
	dt.Update([]adb.DeviceEvent{{Serial: "DEV001", State: "device"}})

	locked, by := dt.IsLocked("DEV001")
	if !locked {
		t.Error("更新後鎖定應保留")
	}
	if by != "client-1" {
		t.Errorf("LockedBy = %q, 預期 %q", by, "client-1")
	}
}

func TestDeviceTable_Lock_Success(t *testing.T) {
	dt := adb.NewDeviceTable()
	dt.Update([]adb.DeviceEvent{{Serial: "DEV001", State: "device"}})

	if !dt.Lock("DEV001", "client-1") {
		t.Error("鎖定線上設備應成功")
	}

	locked, by := dt.IsLocked("DEV001")
	if !locked {
		t.Error("設備應已被鎖定")
	}
	if by != "client-1" {
		t.Errorf("LockedBy = %q, 預期 %q", by, "client-1")
	}
}

func TestDeviceTable_Lock_AlreadyLocked(t *testing.T) {
	dt := adb.NewDeviceTable()
	dt.Update([]adb.DeviceEvent{{Serial: "DEV001", State: "device"}})
	dt.Lock("DEV001", "client-1")

	if dt.Lock("DEV001", "client-2") {
		t.Error("已鎖定的設備不應被再次鎖定")
	}
}

func TestDeviceTable_Lock_OfflineDevice(t *testing.T) {
	dt := adb.NewDeviceTable()
	dt.Update([]adb.DeviceEvent{{Serial: "DEV001", State: "offline"}})

	if dt.Lock("DEV001", "client-1") {
		t.Error("離線設備不應被鎖定")
	}
}

func TestDeviceTable_Lock_NonexistentDevice(t *testing.T) {
	dt := adb.NewDeviceTable()
	if dt.Lock("DEV999", "client-1") {
		t.Error("不存在的設備不應被鎖定")
	}
}

func TestDeviceTable_Unlock_Success(t *testing.T) {
	dt := adb.NewDeviceTable()
	dt.Update([]adb.DeviceEvent{{Serial: "DEV001", State: "device"}})
	dt.Lock("DEV001", "client-1")

	if !dt.Unlock("DEV001", "client-1") {
		t.Error("持鎖者應能成功解鎖")
	}

	locked, _ := dt.IsLocked("DEV001")
	if locked {
		t.Error("解鎖後設備應為未鎖定")
	}
}

func TestDeviceTable_Unlock_WrongClient(t *testing.T) {
	dt := adb.NewDeviceTable()
	dt.Update([]adb.DeviceEvent{{Serial: "DEV001", State: "device"}})
	dt.Lock("DEV001", "client-1")

	if dt.Unlock("DEV001", "client-2") {
		t.Error("非持鎖者不應能解鎖")
	}
}

func TestDeviceTable_UnlockAll(t *testing.T) {
	dt := adb.NewDeviceTable()
	dt.Update([]adb.DeviceEvent{
		{Serial: "DEV001", State: "device"},
		{Serial: "DEV002", State: "device"},
		{Serial: "DEV003", State: "device"},
	})
	dt.Lock("DEV001", "client-1")
	dt.Lock("DEV002", "client-1")
	dt.Lock("DEV003", "client-2")

	dt.UnlockAll("client-1")

	locked1, _ := dt.IsLocked("DEV001")
	locked2, _ := dt.IsLocked("DEV002")
	locked3, _ := dt.IsLocked("DEV003")

	if locked1 {
		t.Error("DEV001 應已解鎖")
	}
	if locked2 {
		t.Error("DEV002 應已解鎖")
	}
	if !locked3 {
		t.Error("DEV003 不屬於 client-1，不應被解鎖")
	}
}

func TestDeviceTable_ConcurrentLock(t *testing.T) {
	dt := adb.NewDeviceTable()
	dt.Update([]adb.DeviceEvent{{Serial: "DEV001", State: "device"}})

	const goroutines = 100
	successCount := 0
	var mu sync.Mutex
	var wg sync.WaitGroup

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			if dt.Lock("DEV001", fmt.Sprintf("client-%d", id)) {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if successCount != 1 {
		t.Errorf("並行鎖定應只有 1 個成功，但有 %d 個成功", successCount)
	}
}

func TestDeviceTable_List_ShowsLockStatus(t *testing.T) {
	dt := adb.NewDeviceTable()
	dt.Update([]adb.DeviceEvent{
		{Serial: "DEV001", State: "device"},
		{Serial: "DEV002", State: "device"},
	})
	dt.Lock("DEV001", "client-1")

	list := dt.List()
	lockMap := make(map[string]string)
	for _, d := range list {
		lockMap[d.Serial] = d.Lock
	}

	if lockMap["DEV001"] != "locked" {
		t.Errorf("DEV001 應為 locked，但為 %q", lockMap["DEV001"])
	}
	if lockMap["DEV002"] != "available" {
		t.Errorf("DEV002 應為 available，但為 %q", lockMap["DEV002"])
	}
}
