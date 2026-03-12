// device_proxy.go 為每台遠端設備管理獨立的 ADB proxy port。
//
// # 設計動機
//
// scrcpy、UIAutomator 等工具以 port 定位設備（`adb -s 127.0.0.1:<port>`），
// 單 proxy port 的 ADB transport 多工無法讓這些工具區分多台設備。
// DeviceProxyManager 為每台在線設備分配獨立 port，每個 port 由獨立的
// ForwardManager + accept loop 處理，完全重用現有的 HandleProxyConn 邏輯。
//
// # 生命週期
//
//  1. 上層（GUI/CLI）建立 DeviceProxyManager 並傳入 OpenChannelFunc + portStart
//  2. 設備清單更新時呼叫 UpdateDevices（來自 control channel 或 directsrv 輪詢）
//  3. 新設備 → allocPort + startProxy；消失的設備 → stopProxy + releasePort
//  4. 結束時呼叫 Close 關閉所有 proxy
//
// # Port 分配策略
//
// 從 portStart 遞增掃描，跳過已占用的 port，直接 net.Listen 取得 listener
// （避免 TOCTOU）。掃描上限為 portStart + 100。
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"sync/atomic"
)

// maxPortScanRange 是 port 掃描的最大範圍（從 portStart 起算）。
const maxPortScanRange = 100

// DeviceProxyConfig 是 DeviceProxyManager 的建構參數。
type DeviceProxyConfig struct {
	PortStart int            // port 分配起始值（如 5555）
	OpenCh    OpenChannelFunc // 開啟 channel 的函式（WebRTC 或 TCP 直連）
	ADBAddr   string         // 本機 ADB server 地址（如 "127.0.0.1:5037"，供 callback 使用）
	OnReady   func(serial string, port int) // 設備 proxy 就緒 callback（可為 nil）
	OnRemoved func(serial string, port int) // 設備 proxy 移除 callback（可為 nil）
}

// DeviceEntry 單台設備的 proxy 資訊（對外唯讀結構）。
type DeviceEntry struct {
	Serial string // 設備序號
	Port   int    // 分配的 proxy port
}

// DeviceProxyManager 為每台遠端設備管理獨立的 ADB proxy port。
//
// 每台設備一個 ForwardManager + 一個 TCP listener + 一個 accept loop，
// 透過 UpdateDevices 的 diff 邏輯自動增減。完全重用 ForwardManager.HandleProxyConn，
// 因為每個 per-device ForwardManager 只有一台設備，GetDevice 永遠回傳正確設備。
type DeviceProxyManager struct {
	mu        sync.Mutex
	portStart int
	usedPorts map[int]bool            // 已分配的 port（用於掃描時跳過）
	entries   map[string]*deviceEntry // serial → per-device proxy 狀態
	openCh    OpenChannelFunc
	onReady   func(serial string, port int)
	onRemoved func(serial string, port int)
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
}

// deviceEntry 單台設備的 proxy 狀態（內部使用）。
type deviceEntry struct {
	serial string
	port   int
	ln     net.Listener
	fm     *ForwardManager
	cancel context.CancelFunc // 關閉此設備的 accept loop
}

// NewDeviceProxyManager 建立新的 per-device proxy 管理器。
func NewDeviceProxyManager(cfg DeviceProxyConfig) *DeviceProxyManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &DeviceProxyManager{
		portStart: cfg.PortStart,
		usedPorts: make(map[int]bool),
		entries:   make(map[string]*deviceEntry),
		openCh:    cfg.OpenCh,
		onReady:   cfg.OnReady,
		onRemoved: cfg.OnRemoved,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// newDeviceProxyManagerWithCtx 使用外部 context 建立（供測試使用）。
func newDeviceProxyManagerWithCtx(parentCtx context.Context, cfg DeviceProxyConfig) *DeviceProxyManager {
	ctx, cancel := context.WithCancel(parentCtx)
	return &DeviceProxyManager{
		portStart: cfg.PortStart,
		usedPorts: make(map[int]bool),
		entries:   make(map[string]*deviceEntry),
		openCh:    cfg.OpenCh,
		onReady:   cfg.OnReady,
		onRemoved: cfg.OnRemoved,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// UpdateDevices 以新的設備清單更新 proxy 映射。
// 比對現有 entries：新增缺少的、移除消失的、跳過已存在的。
// 僅處理 State=="device" 的在線設備。
//
// 此方法為冪等操作，重複呼叫相同清單不會產生副作用。
func (m *DeviceProxyManager) UpdateDevices(devices []DeviceInfo) {
	// 收集在線設備
	newSet := make(map[string]DeviceInfo)
	for _, d := range devices {
		if d.State == "device" {
			newSet[d.Serial] = d
		}
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}

	// 找出要移除的設備（存在於 entries 但不在 newSet）
	var toRemove []*deviceEntry
	for serial, entry := range m.entries {
		if _, ok := newSet[serial]; !ok {
			toRemove = append(toRemove, entry)
			delete(m.entries, serial)
			delete(m.usedPorts, entry.port)
		}
	}

	// 找出要新增的設備（存在於 newSet 但不在 entries）
	var toAdd []DeviceInfo
	for serial, d := range newSet {
		if _, ok := m.entries[serial]; !ok {
			toAdd = append(toAdd, d)
		}
	}

	// 新增設備：分配 port + 建立 listener + 建立 ForwardManager
	var added []*deviceEntry
	for _, d := range toAdd {
		ln, port, err := m.allocPortLocked()
		if err != nil {
			slog.Warn("DeviceProxyManager: failed to allocate port",
				"serial", d.Serial, "error", err)
			continue
		}

		fm := NewForwardManager()
		fm.UpdateDevices([]DeviceInfo{d})

		entryCtx, entryCancel := context.WithCancel(m.ctx)
		entry := &deviceEntry{
			serial: d.Serial,
			port:   port,
			ln:     ln,
			fm:     fm,
			cancel: entryCancel,
		}
		m.entries[d.Serial] = entry
		m.usedPorts[port] = true
		added = append(added, entry)

		// 啟動 accept loop
		go m.acceptLoop(entryCtx, entry)
	}
	m.mu.Unlock()

	// 鎖外執行清理和 callback（避免持鎖時呼叫可能阻塞的操作）

	// 移除設備
	for _, entry := range toRemove {
		entry.cancel()
		entry.ln.Close()
		entry.fm.CloseFwdListeners()
		if m.onRemoved != nil {
			m.onRemoved(entry.serial, entry.port)
		}
	}

	// 新增設備的 callback
	for _, entry := range added {
		if m.onReady != nil {
			m.onReady(entry.serial, entry.port)
		}
	}
}

// Entries 回傳目前所有 per-device proxy 的快照（供 GUI/CLI 顯示）。
// 回傳值依 port 排序。
func (m *DeviceProxyManager) Entries() []DeviceEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]DeviceEntry, 0, len(m.entries))
	for _, entry := range m.entries {
		result = append(result, DeviceEntry{
			Serial: entry.serial,
			Port:   entry.port,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Port < result[j].Port
	})
	return result
}

// Close 關閉所有 per-device proxy 並釋放資源。
func (m *DeviceProxyManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true

	// 擷取所有 entries 的引用
	entries := make([]*deviceEntry, 0, len(m.entries))
	for serial, entry := range m.entries {
		entries = append(entries, entry)
		delete(m.entries, serial)
		delete(m.usedPorts, entry.port)
	}
	m.mu.Unlock()

	// 取消根 context（停止所有 accept loop）
	m.cancel()

	// 鎖外關閉所有 listener 和 forward listeners
	for _, entry := range entries {
		entry.cancel()
		entry.ln.Close()
		entry.fm.CloseFwdListeners()
		if m.onRemoved != nil {
			m.onRemoved(entry.serial, entry.port)
		}
	}
}

// allocPortLocked 從 portStart 遞增掃描，找第一個未占用且可 Listen 的 port。
// 呼叫者必須持有 m.mu 鎖。回傳 listener 和分配的 port。
func (m *DeviceProxyManager) allocPortLocked() (net.Listener, int, error) {
	for p := m.portStart; p < m.portStart+maxPortScanRange; p++ {
		if m.usedPorts[p] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue // port 被其他程式占用，嘗試下一個
		}
		return ln, p, nil
	}
	return nil, 0, fmt.Errorf("no available port in range %d-%d",
		m.portStart, m.portStart+maxPortScanRange-1)
}

// acceptLoop 為單台設備的 proxy TCP accept 迴圈。
// 每個連線由 ForwardManager.HandleProxyConn 處理。
func (m *DeviceProxyManager) acceptLoop(ctx context.Context, entry *deviceEntry) {
	var connID atomic.Int64

	// context 取消時關閉 listener，解除 Accept 阻塞
	go func() {
		<-ctx.Done()
		entry.ln.Close()
	}()

	for {
		conn, err := entry.ln.Accept()
		if err != nil {
			return // listener 關閉或 context 取消
		}
		id := connID.Add(1)
		go entry.fm.HandleProxyConn(ctx, conn, m.openCh, id)
	}
}
