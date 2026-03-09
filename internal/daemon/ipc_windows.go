//go:build windows

package daemon

import "net"

const defaultIPCAddr = "127.0.0.1:15554"

// IPCListen 建立 IPC 監聽器（Windows 使用 TCP）。
func IPCListen() (net.Listener, error) {
	return net.Listen("tcp", defaultIPCAddr)
}

// IPCDial 連線到 IPC 服務（Windows 使用 TCP）。
func IPCDial() (net.Conn, error) {
	return net.Dial("tcp", defaultIPCAddr)
}
