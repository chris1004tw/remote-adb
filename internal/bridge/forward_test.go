package bridge

import (
	"context"
	"testing"
	"time"
)

func TestResolveSerial(t *testing.T) {
	t.Run("命中遠端真實 serial", func(t *testing.T) {
		fm := NewForwardManager()
		fm.devices = []DeviceInfo{{Serial: "R58X40L07QP", State: "device"}}

		got, ok := fm.ResolveSerial("R58X40L07QP")
		if !ok || got != "R58X40L07QP" {
			t.Fatalf("got (%q, %v), want (%q, true)", got, ok, "R58X40L07QP")
		}
	})

	t.Run("本機 adb serial 映射到唯一遠端設備", func(t *testing.T) {
		fm := NewForwardManager()
		fm.devices = []DeviceInfo{{Serial: "R58X40L07QP", State: "device"}}

		got, ok := fm.ResolveSerial("127.0.0.1:15037")
		if !ok || got != "R58X40L07QP" {
			t.Fatalf("got (%q, %v), want (%q, true)", got, ok, "R58X40L07QP")
		}
	})

	t.Run("多裝置且 serial 不可解析時失敗", func(t *testing.T) {
		fm := NewForwardManager()
		fm.devices = []DeviceInfo{
			{Serial: "R58X40L07QP", State: "device"},
			{Serial: "ABC123", State: "device"},
		}

		_, ok := fm.ResolveSerial("127.0.0.1:15037")
		if ok {
			t.Fatal("expected resolve failure for ambiguous multi-device")
		}
	})
}

func TestParseForwardCmd(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		wantNil    bool
		wantSerial string
		wantLocal  string
		wantRemote string
	}{
		{
			name:       "host:forward 基本格式",
			cmd:        "host:forward:tcp:27183;localabstract:scrcpy",
			wantLocal:  "tcp:27183",
			wantRemote: "localabstract:scrcpy",
		},
		{
			name:       "host:forward:norebind",
			cmd:        "host:forward:norebind:tcp:27183;localabstract:scrcpy",
			wantLocal:  "tcp:27183",
			wantRemote: "localabstract:scrcpy",
		},
		{
			name:       "host-serial 指定裝置",
			cmd:        "host-serial:R58X40L07QP:forward:tcp:0;localabstract:scrcpy",
			wantSerial: "R58X40L07QP",
			wantLocal:  "tcp:0",
			wantRemote: "localabstract:scrcpy",
		},
		{
			name:       "host-serial + norebind",
			cmd:        "host-serial:ABC123:forward:norebind:tcp:5000;tcp:6000",
			wantSerial: "ABC123",
			wantLocal:  "tcp:5000",
			wantRemote: "tcp:6000",
		},
		{
			name:    "無效格式：缺少分號",
			cmd:     "host:forward:tcp:27183",
			wantNil: true,
		},
		{
			name:    "無效格式：不是 forward 命令",
			cmd:     "host:version",
			wantNil: true,
		},
		{
			name:    "host-serial 但不是 forward",
			cmd:     "host-serial:ABC:shell:ls",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := ParseForwardCmd(tt.cmd)
			if tt.wantNil {
				if fc != nil {
					t.Errorf("期望 nil，但得到 %+v", fc)
				}
				return
			}
			if fc == nil {
				t.Fatal("期望非 nil，但得到 nil")
			}
			if fc.Serial != tt.wantSerial {
				t.Errorf("Serial: got %q, want %q", fc.Serial, tt.wantSerial)
			}
			if fc.LocalSpec != tt.wantLocal {
				t.Errorf("LocalSpec: got %q, want %q", fc.LocalSpec, tt.wantLocal)
			}
			if fc.RemoteSpec != tt.wantRemote {
				t.Errorf("RemoteSpec: got %q, want %q", fc.RemoteSpec, tt.wantRemote)
			}
		})
	}
}

func TestParseKillForwardCmd(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		wantSpec string
		wantOK   bool
	}{
		{
			name:     "host:killforward",
			cmd:      "host:killforward:tcp:27183",
			wantSpec: "tcp:27183",
			wantOK:   true,
		},
		{
			name:     "host-serial:killforward",
			cmd:      "host-serial:ABC:killforward:tcp:5000",
			wantSpec: "tcp:5000",
			wantOK:   true,
		},
		{
			name:   "非 killforward 命令",
			cmd:    "host:forward:tcp:0;tcp:1",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := ParseKillForwardCmd(tt.cmd)
			if ok != tt.wantOK {
				t.Errorf("ok: got %v, want %v", ok, tt.wantOK)
			}
			if ok && spec != tt.wantSpec {
				t.Errorf("spec: got %q, want %q", spec, tt.wantSpec)
			}
		})
	}
}

func TestIsKillForwardAll(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"host:killforward-all", true},
		{"host-serial:ABC:killforward-all", true},
		{"host:killforward:tcp:0", false},
		{"host:version", false},
	}
	for _, tt := range tests {
		if got := IsKillForwardAll(tt.cmd); got != tt.want {
			t.Errorf("IsKillForwardAll(%q): got %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestIsListForward(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"host:list-forward", true},
		{"host-serial:ABC:list-forward", true},
		{"host:killforward-all", false},
	}
	for _, tt := range tests {
		if got := IsListForward(tt.cmd); got != tt.want {
			t.Errorf("IsListForward(%q): got %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

// TestGetDevice_ImmediateReturn 測試 GetDevice 在已有設備時立即回傳。
func TestGetDevice_ImmediateReturn(t *testing.T) {
	fm := NewForwardManager()
	fm.devices = []DeviceInfo{
		{Serial: "R58X40L07QP", State: "device", Features: "shell_v2,cmd"},
	}

	serial, features := fm.GetDevice(context.Background(), time.Second)
	if serial != "R58X40L07QP" {
		t.Errorf("serial: got %q, want %q", serial, "R58X40L07QP")
	}
	if features != "shell_v2,cmd" {
		t.Errorf("features: got %q, want %q", features, "shell_v2,cmd")
	}
}

// TestGetDevice_WaitsForDevice 測試 GetDevice 在無設備時等待 deviceReadyCh，
// 模擬 PeerConnection 尚在 connecting 時 CNXN 到達的場景。
func TestGetDevice_WaitsForDevice(t *testing.T) {
	fm := NewForwardManager()

	done := make(chan struct{})
	var serial, features string
	go func() {
		serial, features = fm.GetDevice(context.Background(), 5*time.Second)
		close(done)
	}()

	// 短暫延遲後模擬 control channel 推送設備清單
	time.Sleep(100 * time.Millisecond)
	fm.UpdateDevices([]DeviceInfo{
		{Serial: "R58X40L07QP", State: "device", Features: "shell_v2"},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GetDevice 應在 deviceReadyCh 信號後回傳，但逾時")
	}

	if serial != "R58X40L07QP" {
		t.Errorf("serial: got %q, want %q", serial, "R58X40L07QP")
	}
	if features != "shell_v2" {
		t.Errorf("features: got %q, want %q", features, "shell_v2")
	}
}

// TestGetDevice_Timeout 測試 GetDevice 在逾時後回傳空值。
func TestGetDevice_Timeout(t *testing.T) {
	fm := NewForwardManager()

	start := time.Now()
	serial, _ := fm.GetDevice(context.Background(), 200*time.Millisecond)
	elapsed := time.Since(start)

	if serial != "" {
		t.Errorf("逾時後 serial 應為空，got %q", serial)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("回傳過快，僅 %v", elapsed)
	}
}

// TestGetDevice_ContextCancel 測試 context 取消時 GetDevice 立即回傳。
func TestGetDevice_ContextCancel(t *testing.T) {
	fm := NewForwardManager()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		fm.GetDevice(ctx, 30*time.Second)
		close(done)
	}()

	// 取消 context
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("context 取消後 GetDevice 應立即回傳")
	}
}

// TestGetDevice_NilReadyCh 測試 deviceReadyCh 為 nil 時直接回傳。
func TestGetDevice_NilReadyCh(t *testing.T) {
	fm := &ForwardManager{} // deviceReadyCh = nil, devices = nil

	serial, _ := fm.GetDevice(context.Background(), time.Second)
	if serial != "" {
		t.Errorf("deviceReadyCh 為 nil 時應直接回傳空值，got %q", serial)
	}
}

func TestParseLocalSpec(t *testing.T) {
	tests := []struct {
		spec    string
		want    int
		wantErr bool
	}{
		{"tcp:27183", 27183, false},
		{"tcp:0", 0, false},
		{"tcp:65535", 65535, false},
		{"localabstract:scrcpy", 0, true},
		{"tcp:abc", 0, true},
	}
	for _, tt := range tests {
		port, err := ParseLocalSpec(tt.spec)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseLocalSpec(%q): err=%v, wantErr=%v", tt.spec, err, tt.wantErr)
		}
		if !tt.wantErr && port != tt.want {
			t.Errorf("ParseLocalSpec(%q): got %d, want %d", tt.spec, port, tt.want)
		}
	}
}

// TestUpdateDevices_Signal 測試 UpdateDevices 正確管理 deviceReadyCh 信號。
func TestUpdateDevices_Signal(t *testing.T) {
	fm := NewForwardManager()

	// 初始狀態：channel 未關閉
	select {
	case <-fm.deviceReadyCh:
		t.Fatal("deviceReadyCh 不應在無設備時被關閉")
	default:
	}

	// 加入在線設備：channel 應被關閉
	fm.UpdateDevices([]DeviceInfo{{Serial: "ABC", State: "device"}})
	select {
	case <-fm.deviceReadyCh:
	default:
		t.Fatal("deviceReadyCh 應在設備出現後被關閉")
	}

	// 再次呼叫不應 panic（已關閉的 channel）
	fm.UpdateDevices([]DeviceInfo{{Serial: "ABC", State: "device"}})

	// 設備消失：channel 應被重建
	fm.UpdateDevices([]DeviceInfo{})
	select {
	case <-fm.deviceReadyCh:
		t.Fatal("deviceReadyCh 不應在設備消失後仍被關閉")
	default:
	}
}

// TestOnlineDevices 測試 OnlineDevices 只回傳在線設備。
func TestOnlineDevices(t *testing.T) {
	fm := NewForwardManager()
	fm.devices = []DeviceInfo{
		{Serial: "A", State: "device"},
		{Serial: "B", State: "offline"},
		{Serial: "C", State: "device"},
	}

	online := fm.OnlineDevices()
	if len(online) != 2 {
		t.Fatalf("expected 2 online devices, got %d", len(online))
	}
	if online[0].Serial != "A" || online[1].Serial != "C" {
		t.Errorf("unexpected devices: %+v", online)
	}
}
