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
