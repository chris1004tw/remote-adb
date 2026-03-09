package daemon

import (
	"testing"

	"github.com/chris1004tw/remote-adb/pkg/protocol"
)

func TestUpdateHostDevices_UnknownHost(t *testing.T) {
	d := NewDaemon(Config{})

	// 先設定已知 host
	d.hostsMu.Lock()
	d.hosts = []protocol.HostInfo{
		{HostID: "host-1", Hostname: "known", Devices: nil},
	}
	d.hostsMu.Unlock()

	// 更新已知 host — 應正常更新
	d.updateHostDevices("host-1", []protocol.DeviceInfo{
		{Serial: "DEV001", State: protocol.DeviceStateOnline},
	})
	d.hostsMu.RLock()
	if len(d.hosts) != 1 || len(d.hosts[0].Devices) != 1 {
		t.Errorf("已知 host 更新失敗")
	}
	d.hostsMu.RUnlock()

	// 更新未知 host — 應新增
	d.updateHostDevices("host-2", []protocol.DeviceInfo{
		{Serial: "DEV002", State: protocol.DeviceStateOnline},
	})
	d.hostsMu.RLock()
	defer d.hostsMu.RUnlock()
	if len(d.hosts) != 2 {
		t.Fatalf("hosts 數量 = %d, 預期 2", len(d.hosts))
	}
	found := false
	for _, h := range d.hosts {
		if h.HostID == "host-2" && len(h.Devices) == 1 {
			found = true
		}
	}
	if !found {
		t.Error("未知 host 應被新增到 hosts 列表")
	}
}
