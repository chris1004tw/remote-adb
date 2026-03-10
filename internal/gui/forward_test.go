package gui

import (
	"testing"
)

func TestResolveForwardSerial(t *testing.T) {
	t.Run("命中遠端真實 serial", func(t *testing.T) {
		tab := &pairTab{
			cliDevices: []ctrlDevice{{Serial: "R58X40L07QP", State: "device"}},
		}
		got, ok := tab.resolveForwardSerial("R58X40L07QP")
		if !ok || got != "R58X40L07QP" {
			t.Fatalf("got (%q, %v), want (%q, true)", got, ok, "R58X40L07QP")
		}
	})

	t.Run("本機 adb serial 映射到唯一遠端設備", func(t *testing.T) {
		tab := &pairTab{
			cliDevices: []ctrlDevice{{Serial: "R58X40L07QP", State: "device"}},
		}
		got, ok := tab.resolveForwardSerial("127.0.0.1:15037")
		if !ok || got != "R58X40L07QP" {
			t.Fatalf("got (%q, %v), want (%q, true)", got, ok, "R58X40L07QP")
		}
	})

	t.Run("多裝置且 serial 不可解析時失敗", func(t *testing.T) {
		tab := &pairTab{
			cliDevices: []ctrlDevice{
				{Serial: "R58X40L07QP", State: "device"},
				{Serial: "ABC123", State: "device"},
			},
		}
		_, ok := tab.resolveForwardSerial("127.0.0.1:15037")
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
			fc := parseForwardCmd(tt.cmd)
			if tt.wantNil {
				if fc != nil {
					t.Errorf("期望 nil，但得到 %+v", fc)
				}
				return
			}
			if fc == nil {
				t.Fatal("期望非 nil，但得到 nil")
			}
			if fc.serial != tt.wantSerial {
				t.Errorf("serial: got %q, want %q", fc.serial, tt.wantSerial)
			}
			if fc.localSpec != tt.wantLocal {
				t.Errorf("localSpec: got %q, want %q", fc.localSpec, tt.wantLocal)
			}
			if fc.remoteSpec != tt.wantRemote {
				t.Errorf("remoteSpec: got %q, want %q", fc.remoteSpec, tt.wantRemote)
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
			spec, ok := parseKillForwardCmd(tt.cmd)
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
		if got := isKillForwardAll(tt.cmd); got != tt.want {
			t.Errorf("isKillForwardAll(%q): got %v, want %v", tt.cmd, got, tt.want)
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
		if got := isListForward(tt.cmd); got != tt.want {
			t.Errorf("isListForward(%q): got %v, want %v", tt.cmd, got, tt.want)
		}
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
		port, err := parseLocalSpec(tt.spec)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseLocalSpec(%q): err=%v, wantErr=%v", tt.spec, err, tt.wantErr)
		}
		if !tt.wantErr && port != tt.want {
			t.Errorf("parseLocalSpec(%q): got %d, want %d", tt.spec, port, tt.want)
		}
	}
}
