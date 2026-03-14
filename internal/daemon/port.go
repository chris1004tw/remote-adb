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

// AllocateListener 分配一個空閒的 Port，並回傳已建立的 net.Listener。
// 與 Allocate 不同，此方法不會在分配後關閉 listener，呼叫者可直接使用
// 回傳的 listener 接受連線，避免 TOCTOU（Time-of-check to time-of-use）競爭：
// Allocate 在「確認可用」與「實際使用」之間有時間差，其他程式可能搶佔該 port。
// AllocateListener 則在確認可用的同時持有 listener，消除此風險。
func (pa *PortAllocator) AllocateListener() (net.Listener, int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	for port := pa.start; port <= pa.end; port++ {
		if pa.used[port] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // port 被其他程式占用，嘗試下一個
		}
		pa.used[port] = true
		return ln, port, nil
	}
	return nil, 0, fmt.Errorf("無可用的 port（範圍 %d-%d 已滿）", pa.start, pa.end)
}

// Allocate 分配一個空閒的 Port。
// 從 start 開始遞增搜尋，確認 port 未被佔用且可監聽。
// 內部委派給 AllocateListener，取得 listener 後立即關閉再回傳 port。
// 注意：此方法存在 TOCTOU 風險（關閉 listener 後到實際使用之間 port 可能被搶佔），
// 建議優先使用 AllocateListener 直接取得 listener。
func (pa *PortAllocator) Allocate() (int, error) {
	ln, port, err := pa.AllocateListener()
	if err != nil {
		return 0, err
	}
	ln.Close()
	return port, nil
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

