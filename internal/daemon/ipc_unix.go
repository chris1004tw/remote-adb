//go:build !windows

package daemon

import (
	"net"
	"os"
	"path/filepath"
)

// DefaultSocketPath 回傳 Unix Domain Socket 的預設路徑。
func DefaultSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".radb", "daemon.sock")
}

// IPCListen 建立 IPC 監聽器（Unix 使用 Domain Socket）。
func IPCListen() (net.Listener, error) {
	path := DefaultSocketPath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.Remove(path) // 移除可能殘留的 socket 檔案
	return net.Listen("unix", path)
}

// IPCDial 連線到 IPC 服務（Unix 使用 Domain Socket）。
func IPCDial() (net.Conn, error) {
	return net.Dial("unix", DefaultSocketPath())
}
