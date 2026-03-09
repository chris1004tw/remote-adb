package directsrv

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/mdns"

	"github.com/chris1004tw/remote-adb/internal/buildinfo"
)

// DiscoveredAgent 代表透過 mDNS 發現的 Agent。
type DiscoveredAgent struct {
	Name string
	Addr net.IP
	Port int
	Info []string // TXT records
}

// DiscoverMDNS 在 LAN 上搜尋 radb Agent 的 mDNS 服務。
func DiscoverMDNS(timeout time.Duration) ([]DiscoveredAgent, error) {
	entriesCh := make(chan *mdns.ServiceEntry, 8)
	var agents []DiscoveredAgent

	go func() {
		defer close(entriesCh)
		params := mdns.DefaultParams("_radb._tcp")
		params.Entries = entriesCh
		params.Timeout = timeout
		mdns.Query(params)
	}()

	for entry := range entriesCh {
		addr := entry.AddrV4
		if addr == nil {
			addr = entry.AddrV6
		}
		agents = append(agents, DiscoveredAgent{
			Name: entry.Name,
			Addr: addr,
			Port: entry.Port,
			Info: entry.InfoFields,
		})
	}
	return agents, nil
}

// StartMDNS 啟動 mDNS 服務廣播，讓區域網路內的 Client 可以自動發現此 Agent。
// 服務類型為 _radb._tcp，TXT records 包含版本資訊。
// 回傳 shutdown 函式用於停止廣播；可安全重複呼叫。
func StartMDNS(hostname string, port int) (shutdown func(), err error) {
	// 建立 mDNS 服務描述
	service, err := mdns.NewMDNSService(
		hostname,     // instance 名稱
		"_radb._tcp", // 服務類型
		"",           // domain（空字串 = 預設 "local"）
		"",           // hostName（空字串 = 自動偵測）
		port,         // 服務埠號
		nil,          // IPs（nil = 自動偵測）
		[]string{fmt.Sprintf("version=%s", buildinfo.Version)}, // TXT records
	)
	if err != nil {
		return nil, fmt.Errorf("建立 mDNS 服務描述失敗: %w", err)
	}

	// 啟動 mDNS server
	server, err := mdns.NewServer(&mdns.Config{Zone: service})
	if err != nil {
		return nil, fmt.Errorf("啟動 mDNS server 失敗: %w", err)
	}

	slog.Info("mDNS 廣播已啟動", "hostname", hostname, "port", port, "service", "_radb._tcp")

	// 回傳可安全重複呼叫的 shutdown 函式
	var once sync.Once
	return func() {
		once.Do(func() {
			server.Shutdown()
			slog.Info("mDNS 廣播已停止")
		})
	}, nil
}
