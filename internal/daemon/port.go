// Package daemon 實作本機背景服務，管理 WebRTC 連線、TCP 代理與 Port 分配。
package daemon

import (
	"fmt"
	"net"
	"sync"
)

// PortAllocator 管理本機 Port 的分配與回收。
type PortAllocator struct {
	mu    sync.Mutex
	start int
	end   int
	used  map[int]bool
}

// NewPortAllocator 建立一個新的 Port 分配器。
func NewPortAllocator(start, end int) *PortAllocator {
	return &PortAllocator{
		start: start,
		end:   end,
		used:  make(map[int]bool),
	}
}

// Allocate 分配一個空閒的 Port。
// 從 start 開始遞增搜尋，確認 port 未被佔用且可監聽。
func (pa *PortAllocator) Allocate() (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	for port := pa.start; port <= pa.end; port++ {
		if pa.used[port] {
			continue
		}
		// 嘗試監聽確認 port 可用
		if isPortAvailable(port) {
			pa.used[port] = true
			return port, nil
		}
	}
	return 0, fmt.Errorf("無可用的 port（範圍 %d-%d 已滿）", pa.start, pa.end)
}

// Release 釋放已分配的 Port。
func (pa *PortAllocator) Release(port int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	delete(pa.used, port)
}

// UsedCount 回傳已分配的 Port 數量。
func (pa *PortAllocator) UsedCount() int {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return len(pa.used)
}

func isPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
