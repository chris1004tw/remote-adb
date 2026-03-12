// dpm_callbacks.go 提供 GUI 各分頁共用的 DeviceProxyManager callback。
//
// P2P 分頁（tab_pair.go）和區網直連分頁（tab_lan.go）的 OnReady/OnRemoved
// callback 邏輯完全一致（slog 通知 + 自動 adb connect/disconnect + UI 刷新），
// 僅 slog message 前綴不同。本檔案將此邏輯集中，消除重複。
//
// 相關文件：CLAUDE.md「Per-Device Proxy Port」章節
package gui

import (
	"fmt"
	"log/slog"
	"time"

	"gioui.org/app"

	"github.com/chris1004tw/remote-adb/internal/adb"
)

// guiDeviceProxyCallbacks 回傳 DeviceProxyManager 的 OnReady/OnRemoved callback，
// 適用於 GUI 各分頁。callback 會：
//   - 記錄 slog.Info（使用指定的 logPrefix 區分來源）
//   - 呼叫 window.Invalidate() 刷新 UI
//   - 背景 goroutine 自動 adb connect/disconnect
//
// logPrefix 範例："device proxy"（P2P 分頁）、"LAN device proxy"（區網直連分頁）
func guiDeviceProxyCallbacks(win *app.Window, logPrefix string) (onReady func(string, int), onRemoved func(string, int)) {
	onReady = func(serial string, port int) {
		slog.Info(logPrefix+" ready", "serial", serial, "port", port)
		win.Invalidate()
		go func() {
			time.Sleep(300 * time.Millisecond)
			dialer := adb.NewDialer("")
			target := fmt.Sprintf("127.0.0.1:%d", port)
			if err := dialer.Connect(target); err != nil {
				slog.Debug("auto adb connect failed", "target", target, "error", err)
			}
		}()
	}
	onRemoved = func(serial string, port int) {
		slog.Info(logPrefix+" removed", "serial", serial, "port", port)
		win.Invalidate()
		go func() {
			dialer := adb.NewDialer("")
			dialer.Disconnect(fmt.Sprintf("127.0.0.1:%d", port))
		}()
	}
	return
}
