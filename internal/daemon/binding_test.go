package daemon_test

import (
	"testing"

	"github.com/chris1004tw/remote-adb/internal/daemon"
)

func TestBindingTable_AddAndList(t *testing.T) {
	bt := daemon.NewBindingTable()
	err := bt.Add(daemon.Binding{
		LocalPort: 15555,
		HostID:    "agent-1",
		Serial:    "DEV001",
		Status:    "active",
	})
	if err != nil {
		t.Fatalf("Add 失敗: %v", err)
	}

	list := bt.List()
	if len(list) != 1 {
		t.Fatalf("List 數量 = %d, 預期 1", len(list))
	}
	if list[0].Serial != "DEV001" {
		t.Errorf("Serial = %q, 預期 %q", list[0].Serial, "DEV001")
	}
}

func TestBindingTable_AddDuplicate(t *testing.T) {
	bt := daemon.NewBindingTable()
	bt.Add(daemon.Binding{LocalPort: 15555, Serial: "DEV001", Status: "active"})

	err := bt.Add(daemon.Binding{LocalPort: 15555, Serial: "DEV002", Status: "active"})
	if err == nil {
		t.Error("重複 port 應回傳錯誤")
	}
}

func TestBindingTable_Remove(t *testing.T) {
	bt := daemon.NewBindingTable()
	bt.Add(daemon.Binding{LocalPort: 15555, Serial: "DEV001", Status: "active"})
	bt.Remove(15555)

	if bt.Count() != 0 {
		t.Errorf("移除後 Count = %d, 預期 0", bt.Count())
	}
}

func TestBindingTable_Get(t *testing.T) {
	bt := daemon.NewBindingTable()
	bt.Add(daemon.Binding{LocalPort: 15555, Serial: "DEV001", Status: "active"})

	b, ok := bt.Get(15555)
	if !ok {
		t.Fatal("Get 應找到綁定")
	}
	if b.Serial != "DEV001" {
		t.Errorf("Serial = %q, 預期 %q", b.Serial, "DEV001")
	}

	_, ok = bt.Get(99999)
	if ok {
		t.Error("不存在的 port 不應找到綁定")
	}
}

func TestBindingTable_FindBySerial(t *testing.T) {
	bt := daemon.NewBindingTable()
	bt.Add(daemon.Binding{LocalPort: 15555, Serial: "DEV001", Status: "active"})
	bt.Add(daemon.Binding{LocalPort: 15556, Serial: "DEV002", Status: "active"})

	b, ok := bt.FindBySerial("DEV002")
	if !ok {
		t.Fatal("FindBySerial 應找到綁定")
	}
	if b.LocalPort != 15556 {
		t.Errorf("LocalPort = %d, 預期 15556", b.LocalPort)
	}
}

func TestBindingTable_UpdateStatus(t *testing.T) {
	bt := daemon.NewBindingTable()
	bt.Add(daemon.Binding{LocalPort: 15555, Serial: "DEV001", Status: "connecting"})

	bt.UpdateStatus(15555, "active")

	b, _ := bt.Get(15555)
	if b.Status != "active" {
		t.Errorf("Status = %q, 預期 %q", b.Status, "active")
	}
}
