//go:build windows

package main

import (
	"os"
	"syscall"
)

// attachParentConsole 在 CLI 模式下附加父行程的主控台。
//
// 背景：當使用 -ldflags="-H windowsgui" 建置時（Release 版），exe 屬於
// GUI subsystem，不會自動分配主控台視窗。若使用者從 cmd.exe 或 PowerShell
// 執行 CLI 子命令，stdout/stderr 會無法輸出。
//
// 解法：呼叫 Windows API AttachConsole(ATTACH_PARENT_PROCESS) 附加到父行程
// 的主控台，然後將 Go 的 os.Stdout/os.Stderr 重新指向 CONOUT$（主控台輸出裝置）。
//
// 若沒有父主控台（例如雙擊 exe 啟動），AttachConsole 會失敗並直接返回，
// 此時由 main() 進入 GUI 模式，不需要主控台輸出。
func attachParentConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("AttachConsole")
	// ATTACH_PARENT_PROCESS = 0xFFFFFFFF，表示附加到父行程的主控台
	const ATTACH_PARENT_PROCESS = ^uintptr(0)
	r, _, _ := proc.Call(ATTACH_PARENT_PROCESS)
	if r == 0 {
		return // 沒有父主控台（例如雙擊啟動），直接返回
	}
	// 重新開啟 CONOUT$（主控台輸出裝置），取得新的 file descriptor
	conout, err := syscall.Open("CONOUT$", syscall.O_RDWR, 0)
	if err != nil {
		return // GUI 應用中若無法開啟也無妨，不影響 GUI 運作
	}
	os.Stdout = os.NewFile(uintptr(conout), "CONOUT$")
	os.Stderr = os.NewFile(uintptr(conout), "CONOUT$")
}
