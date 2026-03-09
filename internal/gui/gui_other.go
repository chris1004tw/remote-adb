//go:build !(windows || darwin)

package gui

import "fmt"

// Run 在不支援 GUI 的平台上印出提示訊息並返回。
func Run() {
	fmt.Println("GUI 僅支援 Windows 與 macOS，請使用 CLI 模式。")
}
