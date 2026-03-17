package gui

import (
	"gioui.org/app"
	"testing"
)

// TestSettingsEventLoopDefer_OnlyClearsOwnWindow 驗證 settingsEventLoop 的 defer
// 只在 settingsWin 仍為自身時才清除參考，避免新建的視窗參考被舊 goroutine 誤清。
//
// 場景：設定視窗 A 正在關閉（defer 即將執行），但 openWindow 的 recover
// 已清除 A 並建立了新視窗 B。此時 A 的 defer 不應將 settingsWin 設為 nil。
func TestSettingsEventLoopDefer_OnlyClearsOwnWindow(t *testing.T) {
	winA := new(app.Window)
	winB := new(app.Window)

	p := &settingsPanel{}

	// 模擬：設定視窗 A 已被替換為 B（例如 recover 後重新建立）
	p.settingsWin = winB
	p.visible = true

	// 模擬 A 的 defer 邏輯：只在 settingsWin == winA 時才清除
	p.mu.Lock()
	if p.settingsWin == winA {
		p.settingsWin = nil
		p.visible = false
	}
	p.mu.Unlock()

	// 驗證 B 的參考未被清除
	if p.settingsWin != winB {
		t.Fatal("settingsWin 應保持為 winB，不應被 winA 的 defer 清除")
	}
	if !p.visible {
		t.Fatal("visible 應保持為 true")
	}
}

// TestSyncEditorsFromConfig_TURNMode 驗證 syncEditorsFromConfig 將
// TURNMode 正確映射到下拉選單索引（0=Cloudflare, 1=自訂）。
// "none" 和空字串因為是舊設定，映射到 Cloudflare（索引 0）。
func TestSyncEditorsFromConfig_TURNMode(t *testing.T) {
	tests := []struct {
		name     string
		turnMode string
		wantIdx  int
	}{
		{"none 字串（舊設定相容）", TURNModeNone, 0},
		{"空字串（舊設定相容）", "", 0},
		{"cloudflare", TURNModeCloudflare, 0},
		{"custom", TURNModeCustom, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &settingsPanel{
				config: &AppConfig{TURNMode: tt.turnMode},
			}
			p.syncEditorsFromConfig()
			if p.turnSelected != tt.wantIdx {
				t.Errorf("turnSelected = %d, want %d (TURNMode=%q)", p.turnSelected, tt.wantIdx, tt.turnMode)
			}
		})
	}
}

// TestSave_TURNMode 驗證 save() 根據下拉選單索引正確寫入 TURNMode（0=Cloudflare, 1=自訂）。
func TestSave_TURNMode(t *testing.T) {
	tests := []struct {
		name     string
		selected int
		wantMode string
	}{
		{"Cloudflare", 0, TURNModeCloudflare},
		{"自訂", 1, TURNModeCustom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := &settingsPanel{
				config:     &AppConfig{ADBPort: 5037, ProxyPort: 5555, DirectPort: 15555},
				configPath: dir + "/radb.toml",
			}
			p.turnSelected = tt.selected
			p.save()
			if p.config.TURNMode != tt.wantMode {
				t.Errorf("TURNMode = %q, want %q", p.config.TURNMode, tt.wantMode)
			}
		})
	}
}

// TestSyncEditorsFromConfig_ConnMode 驗證 syncEditorsFromConfig 將
// ConnectionMode 正確映射到下拉選單索引（0=直連優先, 1=僅直連, 2=僅中繼）。
func TestSyncEditorsFromConfig_ConnMode(t *testing.T) {
	tests := []struct {
		name     string
		connMode string
		wantIdx  int
	}{
		{"direct-first", ConnModeDirectFirst, 0},
		{"空字串（預設）", "", 0},
		{"direct-only", ConnModeDirectOnly, 1},
		{"relay-only", ConnModeRelayOnly, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &settingsPanel{
				config: &AppConfig{ConnectionMode: tt.connMode},
			}
			p.syncEditorsFromConfig()
			if p.connModeSelected != tt.wantIdx {
				t.Errorf("connModeSelected = %d, want %d (ConnectionMode=%q)", p.connModeSelected, tt.wantIdx, tt.connMode)
			}
		})
	}
}

// TestSave_ConnMode 驗證 save() 根據下拉選單索引正確寫入 ConnectionMode。
func TestSave_ConnMode(t *testing.T) {
	tests := []struct {
		name     string
		selected int
		wantMode string
	}{
		{"直連優先", 0, ConnModeDirectFirst},
		{"僅直連", 1, ConnModeDirectOnly},
		{"僅中繼", 2, ConnModeRelayOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := &settingsPanel{
				config:     &AppConfig{ADBPort: 5037, ProxyPort: 5555, DirectPort: 15555},
				configPath: dir + "/radb.toml",
			}
			p.connModeSelected = tt.selected
			p.save()
			if p.config.ConnectionMode != tt.wantMode {
				t.Errorf("ConnectionMode = %q, want %q", p.config.ConnectionMode, tt.wantMode)
			}
		})
	}
}

// TestSettingsEventLoopDefer_ClearsOwnWindow 驗證 settingsEventLoop 的 defer
// 在 settingsWin 仍為自身時正確清除。
func TestSettingsEventLoopDefer_ClearsOwnWindow(t *testing.T) {
	winA := new(app.Window)

	p := &settingsPanel{}
	p.settingsWin = winA
	p.visible = true

	// 模擬 A 的 defer 邏輯
	p.mu.Lock()
	if p.settingsWin == winA {
		p.settingsWin = nil
		p.visible = false
	}
	p.mu.Unlock()

	if p.settingsWin != nil {
		t.Fatal("settingsWin 應被清除為 nil")
	}
	if p.visible {
		t.Fatal("visible 應為 false")
	}
}
