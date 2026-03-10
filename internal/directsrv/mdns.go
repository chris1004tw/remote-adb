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
	// 使用 buffered channel 避免發現過程中因主 goroutine 處理較慢而丟失結果
	entriesCh := make(chan *mdns.ServiceEntry, 8)
	var agents []DiscoveredAgent

	// 在獨立 goroutine 中執行 mDNS 查詢，查詢完畢後關閉 channel
	go func() {
		defer close(entriesCh)
		params := mdns.DefaultParams("_radb._tcp") // 只查詢 radb 服務類型
		params.Entries = entriesCh
		params.Timeout = timeout
		mdns.Query(params)
	}()

	// 持續讀取查詢結果直到 channel 關閉（即查詢超時）
	for entry := range entriesCh {
		// 過濾非 radb 服務（某些 mDNS 實作會回傳所有服務）
		if !strings.Contains(entry.Name, "_radb._tcp") {
			continue
		}
		// 優先使用 IPv4 位址，若無則回退到 IPv6
		addr := entry.AddrV4
		if addr == nil {
			addr = entry.AddrV6
		}
		// 從 TXT records 中解析 token 欄位（格式："token=<value>"）
		var token string
		for _, field := range entry.InfoFields {
			if strings.HasPrefix(field, "token=") {
				token = strings.TrimPrefix(field, "token=")
				break
			}
		}
		agents = append(agents, DiscoveredAgent{
			Name:  entry.Name,
			Addr:  addr,
			Port:  entry.Port,
			Token: token,
			Info:  entry.InfoFields,
		})
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

	// 建立 mDNS 服務描述（對應一筆 DNS-SD 服務紀錄）
	service, err := mdns.NewMDNSService(
		hostname,     // instance 名稱：mDNS 服務的人類可讀識別名
		"_radb._tcp", // 服務類型：遵循 DNS-SD 命名慣例，_應用名._傳輸協定
		"",           // domain（空字串 = 預設 "local."，即區域網路 mDNS 網域）
		"",           // hostName（空字串 = 自動偵測本機主機名稱）
		port,         // 服務埠號：Agent Direct Server 的 TCP 監聽埠
		nil,          // IPs（nil = 自動偵測本機所有網路介面的 IP）
		txtRecords,   // TXT records：攜帶版本與 token 等元資料
	)
	if err != nil {
		return nil, fmt.Errorf("建立 mDNS 服務描述失敗: %w", err)
	}

	// 啟動 mDNS server，開始持續回應區域網路內的 mDNS 查詢
	server, err := mdns.NewServer(&mdns.Config{Zone: service})
	if err != nil {
		return nil, fmt.Errorf("啟動 mDNS server 失敗: %w", err)
	}

	slog.Info("mDNS 廣播已啟動", "hostname", hostname, "port", port, "service", "_radb._tcp")

	// 使用 sync.Once 包裝 shutdown 函式，確保冪等性（idempotent）：
	// 無論呼叫幾次 shutdown()，實際的 server.Shutdown() 只會執行一次。
	// 這是因為 Serve() 中的 defer shutdown() 與 context 取消可能同時觸發，
	// 避免重複關閉造成 panic 或錯誤。
	var once sync.Once
	return func() {
		once.Do(func() {
			server.Shutdown()
			slog.Info("mDNS 廣播已停止")
		})
	}, nil
}
