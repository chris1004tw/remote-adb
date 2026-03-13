// mdns.go 提供 mDNS（Multicast DNS）服務廣播與發現功能。
//
// # mDNS 服務發現原理
//
// mDNS 是一種零配置網路協定（RFC 6762），允許裝置在區域網路內透過
// multicast UDP（224.0.0.251:5353）廣播和查詢服務，無需傳統 DNS 伺服器。
// 當 Client 想找到 LAN 上的 Agent 時，只需發送 mDNS 查詢封包，
// 所有正在廣播的 Agent 都會回應自身的 IP、Port 及附加資訊。
//
// # 服務類型命名
//
// 服務類型 "_radb._tcp" 遵循 DNS-SD（RFC 6763）命名規則：
//   - 底線前綴 "_" 表示這是服務標識而非主機名稱
//   - "radb" 為本專案自訂的服務名稱
//   - "_tcp" 表示底層傳輸協定為 TCP
//
// # TXT Records
//
// mDNS TXT records 用於攜帶服務的附加元資料（key=value 格式），
// 本專案使用以下欄位：
//   - version=<版本號>：Agent 的軟體版本
//   - token=<共享密鑰>：Agent 設定的認證 token（若有）
package directsrv

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/mdns"

	"github.com/chris1004tw/remote-adb/internal/buildinfo"
)

const mdnsServiceType = "_radb._tcp"

var (
	mdnsDefaultParams = mdns.DefaultParams
	mdnsQuery         = mdns.Query
	netInterfaces     = net.Interfaces
)

// DiscoveredAgent 代表透過 mDNS 發現的 Agent。
// Client 端收到此結構後，可使用 Addr:Port 建立 TCP 連線，
// 若 Token 非空則需在 Request 中附帶相同 token 以通過認證。
type DiscoveredAgent struct {
	Name  string   // mDNS 服務實例名稱（通常為 Agent 主機名稱）
	Addr  net.IP   // Agent 的 IP 位址（優先 IPv4，備援 IPv6）
	Port  int      // Direct Server 的 TCP 埠號
	Token string   // 從 TXT records 解析的認證 token（空字串表示不需認證）
	Info  []string // 完整的 TXT records（如 ["version=1.0", "token=abc"]）
}

// DiscoverMDNS 在 LAN 上搜尋 radb Agent 的 mDNS 服務。
// 發送 multicast DNS 查詢並在 timeout 時間內收集所有回應的 Agent。
// 回傳的 []DiscoveredAgent 可能為空（表示區域網路內沒有 Agent 在廣播）。
func DiscoverMDNS(timeout time.Duration) ([]DiscoveredAgent, error) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	// 先用系統預設介面查詢；若找不到再 fallback 到各可用網卡，
	// 避免在多網卡（VPN/Tunnel）環境中只打到錯誤介面。
	agents, firstErr := discoverMDNSOnInterface(timeout, nil)
	if len(agents) > 0 {
		return dedupeDiscoveredAgents(agents), nil
	}

	ifaces, err := netInterfaces()
	if err != nil {
		if firstErr != nil {
			return nil, firstErr
		}
		slog.Debug("mDNS network interface enumeration failed, skipping fallback", "error", err)
		return nil, nil
	}

	fallbackTimeout := timeout
	if fallbackTimeout > 1200*time.Millisecond {
		fallbackTimeout = 1200 * time.Millisecond
	}
	if fallbackTimeout < 500*time.Millisecond {
		fallbackTimeout = 500 * time.Millisecond
	}

	for _, iface := range ifaces {
		if !isUsableMDNSInterface(iface) {
			continue
		}
		ifaceCopy := iface
		found, err := discoverMDNSOnInterface(fallbackTimeout, &ifaceCopy)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			slog.Debug("mDNS interface query failed", "iface", iface.Name, "error", err)
			continue
		}
		agents = append(agents, found...)
		if len(agents) > 0 {
			break
		}
	}

	agents = dedupeDiscoveredAgents(agents)
	if len(agents) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return agents, nil
}

// StartMDNS 啟動 mDNS 服務廣播，讓區域網路內的 Client 可以自動發現此 Agent。
// 服務類型為 _radb._tcp，TXT records 包含版本與 token 資訊。
// 回傳 shutdown 函式用於停止廣播；可安全重複呼叫。
//
// TXT records 中的 token 欄位用途：
//   - Agent 設定了 token 時，會將 token 寫入 TXT records
//   - Client 透過 mDNS 發現 Agent 時即可得知是否需要認證
//   - 若 token 為空字串則不寫入 TXT records，表示此 Agent 不需認證
func StartMDNS(hostname string, port int, token string) (shutdown func(), err error) {
	// 組裝 TXT records：版本號必填，token 僅在有值時加入
	txtRecords := []string{fmt.Sprintf("version=%s", buildinfo.Version)}
	if token != "" {
		txtRecords = append(txtRecords, fmt.Sprintf("token=%s", token))
	}

	advertiseIPs, ipErr := collectAdvertiseIPs()
	if ipErr != nil {
		// 非致命：若無法列舉網卡，回退到 hashicorp/mdns 的自動偵測。
		slog.Debug("mDNS local IP collection failed, using auto-detection", "error", ipErr)
	}

	// 建立 mDNS 服務描述（對應一筆 DNS-SD 服務紀錄）
	service, err := mdns.NewMDNSService(
		hostname,        // instance 名稱：mDNS 服務的人類可讀識別名
		mdnsServiceType, // 服務類型：遵循 DNS-SD 命名慣例，_應用名._傳輸協定
		"",              // domain（空字串 = 預設 "local."，即區域網路 mDNS 網域）
		"",              // hostName（空字串 = 自動偵測本機主機名稱）
		port,            // 服務埠號：Agent Direct Server 的 TCP 監聽埠
		advertiseIPs,    // 明確使用可用網卡 IP，降低多網卡環境誤廣播風險
		txtRecords,      // TXT records：攜帶版本與 token 等元資料
	)
	if err != nil {
		return nil, fmt.Errorf("建立 mDNS 服務描述失敗: %w", err)
	}

	// 啟動 mDNS server，開始持續回應區域網路內的 mDNS 查詢
	server, err := mdns.NewServer(&mdns.Config{Zone: service})
	if err != nil {
		return nil, fmt.Errorf("啟動 mDNS server 失敗: %w", err)
	}

	slog.Info("mDNS broadcast started", "hostname", hostname, "port", port, "service", mdnsServiceType, "ip_count", len(advertiseIPs))

	// 使用 sync.Once 包裝 shutdown 函式，確保冪等性（idempotent）：
	// 無論呼叫幾次 shutdown()，實際的 server.Shutdown() 只會執行一次。
	// 這是因為 Serve() 中的 defer shutdown() 與 context 取消可能同時觸發，
	// 避免重複關閉造成 panic 或錯誤。
	var once sync.Once
	return func() {
		once.Do(func() {
			server.Shutdown()
			slog.Info("mDNS broadcast stopped")
		})
	}, nil
}

func discoverMDNSOnInterface(timeout time.Duration, iface *net.Interface) ([]DiscoveredAgent, error) {
	entriesCh := make(chan *mdns.ServiceEntry, 16)
	errCh := make(chan error, 1)
	var agents []DiscoveredAgent

	go func() {
		defer close(entriesCh)
		params := mdnsDefaultParams(mdnsServiceType)
		params.Entries = entriesCh
		params.Timeout = timeout
		if iface != nil {
			params.Interface = iface
		}
		errCh <- mdnsQuery(params)
	}()

	for entry := range entriesCh {
		agent, ok := parseDiscoveredAgent(entry)
		if !ok {
			continue
		}
		agents = append(agents, agent)
	}

	return dedupeDiscoveredAgents(agents), <-errCh
}

func parseDiscoveredAgent(entry *mdns.ServiceEntry) (DiscoveredAgent, bool) {
	if entry == nil {
		return DiscoveredAgent{}, false
	}
	if !strings.Contains(strings.ToLower(entry.Name), "."+mdnsServiceType+".") {
		return DiscoveredAgent{}, false
	}
	if entry.Port <= 0 {
		return DiscoveredAgent{}, false
	}

	// 優先使用 IPv4；若僅有 IPv6，優先使用帶 zone 的 AddrV6IPAddr。
	addr := entry.AddrV4
	if addr == nil && entry.AddrV6IPAddr != nil {
		addr = entry.AddrV6IPAddr.IP
	}
	if addr == nil {
		addr = entry.AddrV6
	}
	if addr == nil || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLoopback() {
		return DiscoveredAgent{}, false
	}

	var token string
	for _, field := range entry.InfoFields {
		if strings.HasPrefix(field, "token=") {
			token = strings.TrimPrefix(field, "token=")
			break
		}
	}

	return DiscoveredAgent{
		Name:  entry.Name,
		Addr:  append(net.IP(nil), addr...),
		Port:  entry.Port,
		Token: token,
		Info:  append([]string(nil), entry.InfoFields...),
	}, true
}

func dedupeDiscoveredAgents(in []DiscoveredAgent) []DiscoveredAgent {
	if len(in) <= 1 {
		return in
	}
	out := make([]DiscoveredAgent, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, a := range in {
		if a.Addr == nil || a.Port <= 0 {
			continue
		}
		key := fmt.Sprintf("%s:%d", a.Addr.String(), a.Port)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, a)
	}
	return out
}

func isUsableMDNSInterface(iface net.Interface) bool {
	if iface.Flags&net.FlagUp == 0 {
		return false
	}
	if iface.Flags&net.FlagLoopback != 0 {
		return false
	}
	if iface.Flags&net.FlagMulticast == 0 {
		return false
	}
	return true
}

func collectAdvertiseIPs() ([]net.IP, error) {
	ifaces, err := netInterfaces()
	if err != nil {
		return nil, err
	}

	ips := make([]net.IP, 0, 4)
	seen := map[string]struct{}{}

	for _, iface := range ifaces {
		if !isUsableMDNSInterface(iface) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip := extractIP(addr)
			if ip == nil || ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}

			if v4 := ip.To4(); v4 != nil {
				key := v4.String()
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				ips = append(ips, append(net.IP(nil), v4...))
				continue
			}

			// link-local IPv6 缺少 zone 時通常不可直接連線，這裡只保留可路由位址。
			if ip.To16() == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
				continue
			}
			ip16 := append(net.IP(nil), ip.To16()...)
			key := ip16.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			ips = append(ips, ip16)
		}
	}

	return ips, nil
}

func extractIP(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}
