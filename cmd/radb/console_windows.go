//go:build windows

package main

import "syscall"

// freeConsole 在 GUI 模式下脫離父行程的主控台。
//
// 背景：Release 版不再使用 -H windowsgui（GUI subsystem），改為 console subsystem，
// 讓 CLI 模式下 PowerShell/cmd.exe 能正常等待程式結束、stdin/stdout/stderr 正常運作。
//
// 副作用：以 console subsystem 建置時，雙擊 exe 會短暫顯示主控台視窗。
// 在 GUI 模式啟動時，盡早呼叫 FreeConsole() 脫離主控台，使視窗立即消失。
// CLI 模式不需呼叫此函式——直接使用繼承的主控台即可。
func freeConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("FreeConsole")
	proc.Call()
}
