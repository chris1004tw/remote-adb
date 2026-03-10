// port.go 實作本機 TCP Port 的分配與回收機制。
package daemon

import (
	"fmt"
	"net"
	"sync"
)

// PortAllocator 管理本機 TCP Port 的分配與回收。
// 在指定的 port 範圍（start~end）內，採用線性掃描尋找可用 port。
// 內部同時維護一個 used map 記錄已分配的 port，避免重複分配。
type PortAllocator struct {
	mu    sync.Mutex
	start int            // port 範圍起始值（含）
	end   int            // port 範圍結束值（含）
	used  map[int]bool   // 已分配的 port 記錄
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

// isPortAvailable 透過「實際監聽測試」檢查 port 是否可用。
//
// 為什麼不用查表（如讀取 /proc/net/tcp 或呼叫 netstat）？
//  1. 查表方式依賴平台特定的 API，不具跨平台可移植性（Windows/Linux/macOS 各不相同）
//  2. 查表存在 TOCTOU（Time-of-check to time-of-use）競爭：查完到實際監聽之間 port 可能被佔走
//  3. 實際 Listen + Close 是最可靠的方式，由作業系統直接回答 port 是否可用
//  4. 監聽範圍固定在 127.0.0.1，僅測試 loopback 介面，不影響外部連線
func isPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
