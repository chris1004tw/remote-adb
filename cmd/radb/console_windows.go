//go:build windows

package main

import (
	"os"
	"syscall"
)

// attachParentConsole 在 CLI 模式下附加父行程的主控台。
// 當用 -H windowsgui 建置時，exe 不會自帶主控台視窗，
// 此函式讓 CLI 輸出能正確顯示在呼叫者的終端機中。
func attachParentConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("AttachConsole")
	const ATTACH_PARENT_PROCESS = ^uintptr(0)
	r, _, _ := proc.Call(ATTACH_PARENT_PROCESS)
	if r == 0 {
		return // 沒有父主控台（例如雙擊啟動）
	}
	// 重新開啟 stdout/stderr 到主控台
	conout, err := syscall.Open("CONOUT$", syscall.O_RDWR, 0)
	if err != nil {
		return
	}
	os.Stdout = os.NewFile(uintptr(conout), "CONOUT$")
	os.Stderr = os.NewFile(uintptr(conout), "CONOUT$")
}
