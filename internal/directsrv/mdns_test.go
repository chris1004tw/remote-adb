package directsrv

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/mdns"
)

// TestStartMDNS_NoError 測試 mDNS 服務能夠正常建立且不回傳錯誤。
func TestStartMDNS_NoError(t *testing.T) {
	shutdown, err := StartMDNS("test-host", 9000, "")
	if err != nil {
		t.Fatalf("StartMDNS 應成功，但回傳錯誤: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown 函式不應為 nil")
	}
	// 關閉不應 panic
	shutdown()
}

// TestStartMDNS_ShutdownIdempotent 測試重複呼叫 shutdown 不會 panic。
func TestStartMDNS_ShutdownIdempotent(t *testing.T) {
	shutdown, err := StartMDNS("test-host", 9001, "")
	if err != nil {
		t.Fatalf("StartMDNS 應成功，但回傳錯誤: %v", err)
	}
	// 連續呼叫兩次 shutdown，不應 panic
	shutdown()
	shutdown()
}

func TestDiscoverMDNS_FallbackInterface(t *testing.T) {
	origDefaultParams := mdnsDefaultParams
	origQuery := mdnsQuery
	origInterfaces := netInterfaces
	t.Cleanup(func() {
		mdnsDefaultParams = origDefaultParams
		mdnsQuery = origQuery
		netInterfaces = origInterfaces
	})

	mdnsDefaultParams = mdns.DefaultParams
	mdnsQuery = func(params *mdns.QueryParam) error {
		if params.Interface != nil && params.Interface.Name == "eth0" {
			params.Entries <- &mdns.ServiceEntry{
				Name:       "agent._radb._tcp.local.",
				AddrV4:     net.ParseIP("192.168.1.20"),
				Port:       15555,
				InfoFields: []string{"version=dev", "token=abc"},
			}
		}
		return nil
	}
	netInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{
			{Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagLoopback | net.FlagMulticast},
			{Index: 2, Name: "eth0", Flags: net.FlagUp | net.FlagMulticast},
		}, nil
	}

	agents, err := DiscoverMDNS(100 * time.Millisecond)
	if err != nil {
		t.Fatalf("DiscoverMDNS 不應回傳錯誤: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("應發現 1 個 Agent，實際 %d", len(agents))
	}
	if got := agents[0].Addr.String(); got != "192.168.1.20" {
		t.Fatalf("Agent IP = %s, want 192.168.1.20", got)
	}
	if agents[0].Token != "abc" {
		t.Fatalf("Agent token = %q, want %q", agents[0].Token, "abc")
	}
}

func TestDiscoverMDNS_ReturnsErrorWhenAllQueriesFail(t *testing.T) {
	origDefaultParams := mdnsDefaultParams
	origQuery := mdnsQuery
	origInterfaces := netInterfaces
	t.Cleanup(func() {
		mdnsDefaultParams = origDefaultParams
		mdnsQuery = origQuery
		netInterfaces = origInterfaces
	})

	mdnsDefaultParams = mdns.DefaultParams
	mdnsQuery = func(_ *mdns.QueryParam) error {
		return fmt.Errorf("query failed")
	}
	netInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{
			{Index: 2, Name: "eth0", Flags: net.FlagUp | net.FlagMulticast},
		}, nil
	}

	agents, err := DiscoverMDNS(100 * time.Millisecond)
	if err == nil {
		t.Fatal("預期回傳錯誤，但得到 nil")
	}
	if len(agents) != 0 {
		t.Fatalf("失敗時不應有結果，實際 %d", len(agents))
	}
}

func TestParseDiscoveredAgent_RejectsLoopback(t *testing.T) {
	if _, ok := parseDiscoveredAgent(&mdns.ServiceEntry{
		Name:       "agent._radb._tcp.local.",
		AddrV4:     net.ParseIP("127.0.0.1"),
		Port:       15555,
		InfoFields: []string{"token=abc"},
	}); ok {
		t.Fatal("loopback 位址不應被接受")
	}

	agent, ok := parseDiscoveredAgent(&mdns.ServiceEntry{
		Name:       "agent._radb._tcp.local.",
		AddrV4:     net.ParseIP("192.168.1.2"),
		Port:       15555,
		InfoFields: []string{"version=dev", "token=xyz"},
	})
	if !ok {
		t.Fatal("預期有效的 Agent entry")
	}
	if agent.Token != "xyz" {
		t.Fatalf("token = %q, want %q", agent.Token, "xyz")
	}
}
