// cmd_direct.go — Direct 模式子命令（區網直連：agent / connect / discover）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chris1004tw/remote-adb/internal/agent"
	"github.com/chris1004tw/remote-adb/internal/bridge"
	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/directsrv"
)

// cmdDirectAgent 啟動 Direct 模式的區網被控端。
// 在指定 port 開啟 TCP 直連服務 + mDNS 廣播，供同一區網的主控端連線。
func cmdDirectAgent(args []string) {
	fs := flag.NewFlagSet("direct agent", flag.ExitOnError)
	port := fs.Int("port", envInt("RADB_DIRECT_PORT", 9000), "TCP 監聽埠")
	token := fs.String("token", envStr("RADB_DIRECT_TOKEN", ""), "認證 Token")
	hostID := fs.String("host-id", envStr("RADB_HOST_ID", localHostname()), "主機識別名稱")
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server 埠")
	fs.Parse(args)

	slog.Info("starting radb direct agent", "version", buildinfo.Version, "host_id", *hostID, "port", *port)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 建立 Agent（僅用於 DeviceTable 和 Dialer，不連線 Relay Server）
	a := agent.New(agent.Config{
		HostID:  *hostID,
		ADBAddr: fmt.Sprintf("127.0.0.1:%d", *adbPort),
	})

	dsrv := directsrv.New(directsrv.Config{
		DeviceTable: a.DeviceTable(),
		DialDevice: func(serial string, devPort int) (net.Conn, error) {
			return a.Dialer().DialDevice(serial, devPort)
		},
		Hostname: *hostID,
		Token:    *token,
	})

	// 啟動設備追蹤（背景更新 DeviceTable）
	go func() {
		if err := a.RunDirectOnly(ctx); err != nil && ctx.Err() == nil {
			slog.Error("device tracking failed", "error", err)
		}
	}()

	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	fmt.Printf("Direct Agent 已啟動: %s\n", addr)
	fmt.Println("按 Ctrl+C 結束")

	if err := dsrv.Serve(ctx, addr); err != nil && ctx.Err() == nil {
		slog.Error("direct server error", "error", err)
		os.Exit(1)
	}
	slog.Info("direct agent stopped")
}

// cmdConnect 建立 TCP 直連的全設備多工 ADB 轉發。
// 支援 --list 僅查詢遠端設備。
func cmdConnect(args []string) {
	fs := flag.NewFlagSet("direct connect", flag.ExitOnError)
	listOnly := fs.Bool("list", false, "只列出遠端設備")
	token := fs.String("token", envStr("RADB_DIRECT_TOKEN", ""), "認證 Token")
	portStart := fs.Int("port", envInt("RADB_PROXY_PORT", 5555), "本機 ADB proxy port 起始值")
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server port")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb direct connect <地址:port> [--list] [--token TOKEN]")
		os.Exit(1)
	}
	addr := fs.Arg(0)

	if *listOnly {
		cmdConnectList(addr, *token)
		return
	}
	cmdConnectDirect(addr, *token, *portStart, *adbPort)
}

// cmdConnectList 查詢遠端 Agent 的設備列表並印出。
// 透過 directsrv 的 JSON 協定查詢遠端設備清單。
func cmdConnectList(addr, token string) {
	resp, err := directsrv.QueryDevices(addr, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "查詢設備失敗: %v\n", err)
		os.Exit(1)
	}

	if resp.Hostname != "" {
		fmt.Printf("主機: %s\n", resp.Hostname)
	}
	if len(resp.Devices) == 0 {
		fmt.Println("目前沒有設備")
		return
	}

	fmt.Printf("%-20s %-10s %-10s %s\n", "Serial", "State", "Lock", "Locked By")
	fmt.Println(strings.Repeat("-", 55))
	for _, d := range resp.Devices {
		fmt.Printf("%-20s %-10s %-10s %s\n", d.Serial, d.State, d.Lock, d.LockedBy)
	}
}

// cmdConnectDirect 建立 TCP 直連的 per-device ADB 轉發。
// 流程：
//  1. 查詢遠端設備清單（驗證連線可用）
//  2. 建立 DeviceProxyManager（每台設備獨立 proxy port）
//  3. 初始設備 + 背景輪詢驅動 DeviceProxyManager 增減
//  4. 等待 Ctrl+C
func cmdConnectDirect(addr, token string, portStart, adbPort int) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 1. 查詢設備（驗證連線可用）
	devices := queryDirectDevices(addr, token)
	if len(devices) == 0 {
		fmt.Fprintln(os.Stderr, "遠端無可用設備")
		os.Exit(1)
	}

	// 2. 建立 OpenChannelFunc（透過 directsrv）
	openCh := makeDirectOpenChannel(addr, token)

	// 3. 建立 per-device proxy 管理器
	adbAddr := fmt.Sprintf("127.0.0.1:%d", adbPort)
	onReady, onRemoved := cliDeviceProxyCallbacks(adbAddr)
	dpm := bridge.NewDeviceProxyManager(bridge.DeviceProxyConfig{
		PortStart: portStart,
		OpenCh:    openCh,
		ADBAddr:   adbAddr,
		OnReady:   onReady,
		OnRemoved: onRemoved,
	})
	defer dpm.Close()

	// 初始設備清單
	dpm.UpdateDevices(directsrv.ToBridgeDevices(devices))

	fmt.Println("按 Ctrl+C 結束")

	// 背景輪詢設備
	go directsrv.PollDeviceLoop(ctx, 3*time.Second,
		func() []directsrv.DeviceInfo { return queryDirectDevicesQuiet(addr, token) },
		func(devs []bridge.DeviceInfo) { dpm.UpdateDevices(devs) },
	)

	<-ctx.Done()
	fmt.Println("\n轉發已停止")
}

// makeDirectOpenChannel 建立 LAN 直連用的 bridge.OpenChannelFunc。
// 根據 label 前綴路由到不同的 directsrv action，與 GUI tab_lan.go 的 makeOpenChannel 邏輯一致。
//
// label 格式與路由：
//   - "adb-server/{id}" → connect-server（ADB server 協定命令轉發）
//   - "adb-stream/{id}/{serial}/{service}" → connect-service + PrefixedRWC（設備服務串流）
//   - "adb-fwd/{id}/{serial}/{remoteSpec}" → connect-service（forward 連線到設備服務）
//
// adb-stream 的特殊處理：setupStream 期待讀取 1 byte ready signal，
// 但 directsrv 的 connect-service 回傳連線時已完成 ADB transport + service，
// 因此使用 PrefixedRWC 注入虛擬的 ready byte（0x01），讓 setupStream 正確通過。
func makeDirectOpenChannel(addr, token string) bridge.OpenChannelFunc {
	return func(label string) (io.ReadWriteCloser, error) {
		switch {
		case strings.HasPrefix(label, "adb-server/"):
			return directsrv.DialService(addr, token, "connect-server", "", "")

		case strings.HasPrefix(label, "adb-stream/"):
			parts := strings.SplitN(label, "/", 4)
			if len(parts) < 4 {
				return nil, fmt.Errorf("invalid stream label: %s", label)
			}
			conn, err := directsrv.DialService(addr, token, "connect-service", parts[2], parts[3])
			if err != nil {
				return nil, err
			}
			// setupStream 期待 ready signal（1 byte），connect-service 成功後連線已就緒
			return &bridge.PrefixedRWC{Ch: conn, Prefix: []byte{1}}, nil

		case strings.HasPrefix(label, "adb-fwd/"):
			parts := strings.SplitN(label, "/", 4)
			if len(parts) < 4 {
				return nil, fmt.Errorf("invalid fwd label: %s", label)
			}
			return directsrv.DialService(addr, token, "connect-service", parts[2], parts[3])

		default:
			return nil, fmt.Errorf("unknown channel: %s", label)
		}
	}
}

// queryDirectDevices 查詢遠端 Agent 的設備清單。
// 回傳全部設備（含 offline），供 cmdConnectDirect 初始化使用。
// 失敗時直接 os.Exit。
func queryDirectDevices(addr, token string) []directsrv.DeviceInfo {
	resp, err := directsrv.QueryDevices(addr, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "查詢設備失敗: %v\n", err)
		os.Exit(1)
	}

	if resp.Hostname != "" {
		fmt.Printf("主機: %s\n", resp.Hostname)
	}
	return resp.Devices
}

// queryDirectDevicesQuiet 靜默查詢遠端設備清單（不印輸出、不 exit）。
// 供 pollDirectDevicesDPM 輪詢使用，失敗時回傳 nil。
func queryDirectDevicesQuiet(addr, token string) []directsrv.DeviceInfo {
	resp, err := directsrv.QueryDevices(addr, token)
	if err != nil {
		return nil
	}
	return resp.Devices
}

// cmdDiscover 掃描區網上的 radb Agent（mDNS）。
// 預設掃描 3 秒，列出所有發現的被控端名稱、地址和附加資訊。
func cmdDiscover() {
	fmt.Println("正在掃描區網內的 radb Agent...")

	agents, err := directsrv.DiscoverMDNS(3 * time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mDNS 掃描失敗: %v\n", err)
		os.Exit(1)
	}

	if len(agents) == 0 {
		fmt.Println("未發現任何 Agent")
		return
	}

	for _, a := range agents {
		info := ""
		if len(a.Info) > 0 {
			info = " [" + strings.Join(a.Info, ", ") + "]"
		}
		fmt.Printf("  %s (%s:%d)%s\n", a.Name, a.Addr, a.Port, info)
	}
}
