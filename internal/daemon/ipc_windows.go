//go:build windows

// ipc_windows.go 實作 Windows 平台的 IPC 傳輸層。
//
// 設計選擇：Windows 使用 TCP loopback（127.0.0.1:15554）而非 Unix Domain Socket，原因：
//  1. Windows 對 Unix Domain Socket 的支援較晚（Windows 10 1803+），且行為與 POSIX 不完全一致
//  2. Named Pipe 雖然是 Windows 原生 IPC 機制，但 Go 標準庫不直接支援，需要第三方套件
//  3. TCP loopback 在 Go 標準庫中跨平台通用，且綁定 127.0.0.1 確保只接受本機連線
//  4. 監聽 127.0.0.1 而非 0.0.0.0，外部網路無法存取，安全性足夠
package daemon

import "net"

// defaultIPCAddr 是 Windows 上 IPC 的 TCP 監聽位址。
// Port 15554 選在 ADB 預設 port（5555）附近但不衝突的範圍。
const defaultIPCAddr = "127.0.0.1:15554"

// IPCListen 建立 IPC 監聽器（Windows 使用 TCP loopback）。
func IPCListen() (net.Listener, error) {
	return net.Listen("tcp", defaultIPCAddr)
}

// IPCDial 連線到 IPC 服務（Windows 使用 TCP loopback）。
func IPCDial() (net.Conn, error) {
	return net.Dial("tcp", defaultIPCAddr)
}
