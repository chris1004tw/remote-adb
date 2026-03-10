//go:build !windows

// ipc_unix.go 實作 Unix/macOS 平台的 IPC 傳輸層。
//
// 設計選擇：Unix 系統使用 Unix Domain Socket 而非 TCP，原因：
//  1. Unix Domain Socket 透過檔案系統權限控制存取，天然具備 OS 層級的安全性
//  2. 無需佔用 TCP port，避免與其他服務衝突
//  3. 效能優於 TCP loopback：不經過網路協定棧，直接在 kernel 內傳輸
//  4. 是 Unix 系統上 daemon IPC 的慣例做法（如 Docker、SSH agent）
package daemon

import (
	"net"
	"os"
	"path/filepath"
)

// DefaultSocketPath 回傳 Unix Domain Socket 的預設路徑：~/.radb/daemon.sock。
func DefaultSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".radb", "daemon.sock")
}

// IPCListen 建立 IPC 監聽器（Unix 使用 Domain Socket）。
func IPCListen() (net.Listener, error) {
	path := DefaultSocketPath()
	// 建立 socket 所在目錄，權限設為 0o700（僅目錄擁有者可讀寫執行）。
	// 這是安全性考量：Unix Domain Socket 的存取權限繼承自所在目錄，
	// 0o700 確保只有當前使用者能連線到 daemon，防止同機器上的其他使用者存取。
	os.MkdirAll(filepath.Dir(path), 0o700)
	// 移除可能殘留的 socket 檔案（例如上次 daemon 非正常關閉時留下的）。
	// 若不移除，net.Listen("unix", ...) 會因檔案已存在而失敗。
	os.Remove(path)
	return net.Listen("unix", path)
}

// IPCDial 連線到 IPC 服務（Unix 使用 Domain Socket）。
func IPCDial() (net.Conn, error) {
	return net.Dial("unix", DefaultSocketPath())
}
