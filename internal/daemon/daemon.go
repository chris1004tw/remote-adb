// Package daemon 實作本機端的背景服務（Daemon），扮演開發者本機與遠端 Agent 之間的橋樑。
//
// 整體架構角色：
//
//	使用者 CLI ──IPC──▶ Daemon ──WebSocket──▶ Signal Server ──WebSocket──▶ Agent
//	                       │                                                  │
//	                       └──────── WebRTC DataChannel (P2P) ────────────────┘
//	                       │
//	  adb client ──TCP──▶ Proxy（本機 port）
//
// Daemon 負責：
//  1. 與 Signal Server 建立 WebSocket 連線，進行認證與信令交換
//  2. 為每個綁定的遠端設備建立 WebRTC PeerConnection + DataChannel
//  3. 在本機開啟 TCP Proxy，讓 adb client 可直接連線到 127.0.0.1:<port>
//  4. 透過 IPC（TCP/Unix Socket）接受 CLI 工具的指令（bind/unbind/list/status/hosts）
//  5. 管理 Port 分配與 Binding 狀態追蹤
//
// 檔案結構：
//   - daemon.go          — 核心骨架（Config、Daemon struct、Start、shutdown）
//   - daemon_ipc.go      — IPC 命令處理（ServeIPC、handleCommand、cmdBind/cmdUnbind 等）
//   - daemon_signal.go   — Signal Server 通訊（connectServer、serverReadLoop、waitResponse 等）
package daemon

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/proxy"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
	ws "github.com/coder/websocket"
)

// IPC 與 Bind 相關逾時常數。
const (
	ipcDeadline       = 50 * time.Second // IPC 連線整體 deadline（須涵蓋 lock + gathering + answer）
	bindGatherTimeout = 15 * time.Second // cmdBind ICE gathering 上限，避免佔用過多 IPC 時間預算
)

// Config 是 Daemon 的啟動設定。
type Config struct {
	ServerURL string           // Signal Server 的 WebSocket URL（如 ws://example.com:8080）
	Token     string           // PSK 認證令牌，須與 Server 端設定一致
	PortStart int              // 本機 TCP Proxy 的 Port 分配範圍起始值（預設 15555）
	PortEnd   int              // 本機 TCP Proxy 的 Port 分配範圍結束值（預設 15655）
	ICEConfig webrtc.ICEConfig // WebRTC ICE 設定（STUN/TURN 伺服器等）
}

// Daemon 是本機端背景服務的核心結構，管理 WebRTC 連線、TCP 代理與 IPC 服務。
//
// Daemon 持有三把 mutex，鎖定順序為：waiterMu → proxyMu → hostsMu。
// 在同時需要多把鎖的場景中，必須按照此順序取鎖以避免 deadlock。
// 實際上目前各 mutex 保護的資源是獨立操作的，不會同時持有多把鎖。
type Daemon struct {
	config   Config
	ports    *PortAllocator
	bindings *BindingTable
	hostname string // 本機主機名，用於信令訊息的 source 欄位

	// Server 連線（Signal Server 的 WebSocket 連線）
	wsConn *ws.Conn
	connID string // Server 認證成功後分配的連線 ID

	// waiters 實作了非同步請求-回應的配對機制。
	// 工作原理：
	//  1. cmdBind 等方法發送請求後，呼叫 waitResponse(key) 註冊一個 channel 到 waiters map
	//  2. serverReadLoop 收到 Server 回傳的訊息後，呼叫 deliverResponse(key) 將訊息寫入對應 channel
	//  3. waitResponse 從 channel 收到回應或逾時返回
	// key 的格式為 "lock_resp:<serial>" 或 "answer:<hostID>"，用於精準匹配請求與回應。
	waiterMu sync.Mutex                       // 保護 waiters map 的讀寫
	waiters  map[string]chan protocol.Envelope // key: 回應識別鍵 → 用於接收回應的 channel

	// proxyMu 保護 proxies 和 peers 兩個 map 的讀寫，
	// 這兩個 map 以本機 port 為 key，分別儲存 TCP Proxy 和 WebRTC PeerManager。
	proxyMu sync.Mutex
	proxies map[int]*proxy.Proxy         // key: 本機 port → TCP 代理實例
	peers   map[int]*webrtc.PeerManager  // key: 本機 port → WebRTC 連線管理器

	// hostsMu 保護 hosts 快取的讀寫，使用 RWMutex 允許多個 cmdHosts 並行讀取。
	hostsMu sync.RWMutex
	hosts   []protocol.HostInfo // 從 Server 取得的遠端主機與設備清單快取
}

// NewDaemon 建立一個新的 Daemon 實例。
// 若 PortStart/PortEnd 未指定，使用預設範圍 15555~15655（共 101 個 port）。
func NewDaemon(cfg Config) *Daemon {
	if cfg.PortStart == 0 {
		cfg.PortStart = 15555
	}
	if cfg.PortEnd == 0 {
		cfg.PortEnd = 15655
	}

	return &Daemon{
		config:   cfg,
		ports:    NewPortAllocator(cfg.PortStart, cfg.PortEnd),
		bindings: NewBindingTable(),
		hostname: buildinfo.Hostname(),
		waiters:  make(map[string]chan protocol.Envelope),
		proxies:  make(map[int]*proxy.Proxy),
		peers:    make(map[int]*webrtc.PeerManager),
	}
}

// Bindings 回傳綁定表（供測試使用）。
func (d *Daemon) Bindings() *BindingTable {
	return d.bindings
}

// Ports 回傳 Port 分配器（供測試使用）。
func (d *Daemon) Ports() *PortAllocator {
	return d.ports
}

// Start 啟動 Daemon，執行以下步驟：
//  1. 連線 Signal Server 並完成 PSK 認證
//  2. 啟動背景 goroutine 持續讀取 Server 訊息
//  3. 主動請求一次遠端主機列表
//  4. 進入 IPC 服務迴圈（阻塞至 ctx 取消）
//  5. ctx 取消後執行 shutdown 清理所有資源
func (d *Daemon) Start(ctx context.Context, ipcListener net.Listener) error {
	if err := d.connectServer(ctx); err != nil {
		return err
	}

	go d.serverReadLoop(ctx)
	d.requestHostList(ctx)

	slog.Info("daemon ready", "conn_id", d.connID, "ipc", ipcListener.Addr())

	// ServeIPC 會阻塞直到 ctx 取消
	d.ServeIPC(ctx, ipcListener)

	return d.shutdown()
}

// shutdown 清理所有資源：停止所有 TCP Proxy、關閉所有 PeerConnection、關閉 WebSocket 連線。
// 採用「鎖內擷取引用、鎖外關閉」模式，避免持有 proxyMu 期間呼叫
// p.Stop()（等待 accept loop）和 pm.Close()（DTLS teardown），
// 多台設備時耗時累加會阻塞所有 IPC 請求。
func (d *Daemon) shutdown() error {
	slog.Info("Daemon shutting down...")

	d.proxyMu.Lock()
	proxyList := make([]*proxy.Proxy, 0, len(d.proxies))
	peerList := make([]*webrtc.PeerManager, 0, len(d.peers))
	for _, p := range d.proxies {
		proxyList = append(proxyList, p)
	}
	for _, pm := range d.peers {
		peerList = append(peerList, pm)
	}
	d.proxies = make(map[int]*proxy.Proxy)
	d.peers = make(map[int]*webrtc.PeerManager)
	d.proxyMu.Unlock()

	for _, p := range proxyList {
		p.Stop()
	}
	for _, pm := range peerList {
		pm.Close()
	}

	if d.wsConn != nil {
		d.wsConn.CloseNow()
	}

	slog.Info("Daemon stopped")
	return nil
}

// generateSessionID 產生 16 字元的隨機十六進位字串，用於 DataChannel label 的唯一識別。
func generateSessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
