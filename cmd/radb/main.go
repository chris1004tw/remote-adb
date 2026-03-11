// Package main 是 radb 的統一入口。
//
// radb 支援三種主要運作模式，透過子命令切換：
//
//   Signal Server 模式（需要中央伺服器）：
//     server       — 啟動 WebSocket 信令伺服器
//     agent        — 啟動常駐遠端被控端
//     agent pair   — 一次性 P2P 被控端（手動 SDP 交換）
//     daemon       — 啟動本機背景服務（管理 WebRTC 連線與 TCP 代理）
//     bind         — 綁定遠端設備到本機 port（互動式 TUI 或 CLI flag）
//     unbind       — 解除設備綁定
//     list         — 列出已綁定的設備
//     status       — 查詢 daemon 連線狀態
//     hosts        — 列出可用的遠端主機與設備
//
//   直連模式（無需 Server）：
//     connect <addr>        — TCP 直連 ADB 全設備多工轉發
//     connect <addr> --list — 查詢遠端設備列表
//     connect pair          — P2P SDP 配對全設備多工（跨 NAT）
//     discover              — mDNS 掃描區網被控端
//
//   其他：
//     update  — 從 GitHub Releases 下載最新版本並替換自身
//     version — 顯示版本資訊
//
// 舊指令 direct / pair 保留為隱藏別名（向後相容），但不顯示在 help 中。
// 無引數執行時進入 GUI 模式（Gio 圖形介面）。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea" // TUI 互動式選單框架

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/agent"
	"github.com/chris1004tw/remote-adb/internal/bridge"
	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/cli"
	"github.com/chris1004tw/remote-adb/internal/daemon"
	"github.com/chris1004tw/remote-adb/internal/directsrv"
	"github.com/chris1004tw/remote-adb/internal/gui"
	"github.com/chris1004tw/remote-adb/internal/proxy"
	signalpkg "github.com/chris1004tw/remote-adb/internal/signal" // 別名避免與 os/signal 衝突
	"github.com/chris1004tw/remote-adb/internal/updater"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

func main() {
	// 清理上次更新留下的 .old 備份檔案
	if selfPath, err := os.Executable(); err == nil {
		updater.CleanupOldBinaries(filepath.Dir(selfPath))
	}

	if len(os.Args) < 2 {
		if f := setupGUILog(); f != nil {
			defer f.Close()
		}
		gui.Run() // 無引數 → 啟動 GUI
		return
	}

	// 有引數 → CLI 模式（Windows 附加父行程主控台）
	attachParentConsole()

	switch os.Args[1] {
	case "server":
		cmdServer(os.Args[2:])
	case "agent":
		// 檢查是否有 "pair" 子命令：radb agent pair <offer-token>
		if len(os.Args) > 2 && os.Args[2] == "pair" {
			cmdAgentPair(os.Args[3:])
		} else {
			cmdAgent(os.Args[2:])
		}
	case "daemon":
		cmdDaemon(os.Args[2:])
	case "bind":
		cmdBind(os.Args[2:])
	case "unbind":
		cmdUnbind(os.Args[2:])
	case "list":
		cmdList()
	case "status":
		cmdStatus()
	case "hosts":
		cmdHosts()
	case "connect":
		cmdConnect(os.Args[2:])
	case "discover":
		cmdDiscover(os.Args[2:])
	// 舊指令隱藏別名（向後相容，不顯示在 printUsage）
	case "direct":
		cmdDirect(os.Args[2:])
	case "pair":
		cmdPair(os.Args[2:])
	case "update":
		cmdUpdate(os.Args[2:])
	case "version":
		fmt.Printf("radb %s (commit: %s, built: %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "用法: radb <子命令> [選項]\n\n")
	fmt.Fprintf(os.Stderr, "Signal Server 模式:\n")
	fmt.Fprintf(os.Stderr, "  server          啟動信令伺服器\n")
	fmt.Fprintf(os.Stderr, "  agent           啟動遠端被控端\n")
	fmt.Fprintf(os.Stderr, "  agent pair      一次性 P2P 被控端\n")
	fmt.Fprintf(os.Stderr, "  daemon          啟動背景服務\n")
	fmt.Fprintf(os.Stderr, "  bind            綁定遠端設備\n")
	fmt.Fprintf(os.Stderr, "  unbind          解除綁定\n")
	fmt.Fprintf(os.Stderr, "  list            列出已綁定設備\n")
	fmt.Fprintf(os.Stderr, "  status          查詢 daemon 狀態\n")
	fmt.Fprintf(os.Stderr, "  hosts           列出可用主機\n")
	fmt.Fprintf(os.Stderr, "\n直連模式（無需 Server）:\n")
	fmt.Fprintf(os.Stderr, "  connect <addr>  TCP 直連 ADB 轉發\n")
	fmt.Fprintf(os.Stderr, "  connect pair    P2P SDP 配對（跨 NAT）\n")
	fmt.Fprintf(os.Stderr, "  discover        掃描區網被控端（mDNS）\n")
	fmt.Fprintf(os.Stderr, "\n其他:\n")
	fmt.Fprintf(os.Stderr, "  update          檢查並更新到最新版本\n")
	fmt.Fprintf(os.Stderr, "  version         顯示版本\n")
}

// cmdServer 啟動 WebSocket 信令伺服器。
// 提供 /ws 端點供 Agent 和 Client 連線，透過 PSK Token 認證。
// 監聽 SIGINT/SIGTERM 收到信號後優雅關閉（10 秒超時）。
func cmdServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	port := fs.Int("port", envInt("RADB_SERVER_PORT", 8080), "HTTP/WebSocket 監聽埠")
	host := fs.String("host", envStr("RADB_SERVER_HOST", "0.0.0.0"), "監聽地址")
	token := fs.String("token", envStr("RADB_TOKEN", ""), "PSK 驗證 Token")
	fs.Parse(args)

	if *token == "" {
		fmt.Fprintln(os.Stderr, "錯誤：必須設定 RADB_TOKEN 環境變數或使用 --token flag")
		os.Exit(1)
	}

	slog.Info("啟動 radb server", "version", buildinfo.Version, "host", *host, "port", *port)

	hub := signalpkg.NewHub()
	auth := signalpkg.NewPSKAuth(*token)
	srv := signalpkg.NewServer(hub, auth)

	httpServer := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", *host, *port),
		Handler: srv.Handler(),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		slog.Info("Server 開始監聽", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server 錯誤", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("收到關閉信號，準備優雅關閉...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server 關閉失敗", "error", err)
	}
	slog.Info("Server 已關閉")
}

// cmdAgent 啟動遠端代理端。
// 支援同時啟用 Signal Server 模式和 Direct 模式：
//   - 有 --token → 連線到 Signal Server，接收 Client 的設備綁定請求
//   - 有 --direct-port → 開啟 TCP 直連服務 + mDNS 廣播
//   - 兩者皆有 → 混合模式，共享同一個 DeviceTable
func cmdAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	serverURL := fs.String("server", envStrFallback("RADB_SERVER_URL", "RADB_SIGNAL_URL", "ws://localhost:8080"), "Server WebSocket 位址")
	token := fs.String("token", envStr("RADB_TOKEN", ""), "PSK Token")
	hostID := fs.String("host-id", envStr("RADB_HOST_ID", localHostname()), "主機識別名稱")
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server 埠")
	stunURLs := fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN Server URL")
	turnURL := fs.String("turn", envStr("RADB_TURN_URL", ""), "TURN Server URL")
	turnUser := fs.String("turn-user", envStr("RADB_TURN_USER", ""), "TURN 使用者名稱")
	turnPass := fs.String("turn-pass", envStr("RADB_TURN_PASS", ""), "TURN 密碼")
	directPort := fs.Int("direct-port", envInt("RADB_DIRECT_PORT", 0), "Direct TCP 監聽埠（0=停用）")
	directToken := fs.String("direct-token", envStr("RADB_DIRECT_TOKEN", ""), "Direct 連線 Token")
	fs.Parse(args)

	// 必須至少啟用 Signal Server 模式或 Direct 模式
	if *token == "" && *directPort == 0 {
		fmt.Fprintln(os.Stderr, "錯誤：必須設定 --token（Signal Server 模式）或 --direct-port（Direct 模式）")
		os.Exit(1)
	}

	iceConfig := webrtc.ICEConfig{}
	if *stunURLs != "" {
		iceConfig.STUNServers = strings.Split(*stunURLs, ",")
	}
	if *turnURL != "" {
		iceConfig.TURNServers = []webrtc.TURNServer{
			{URL: *turnURL, Username: *turnUser, Credential: *turnPass},
		}
	}

	slog.Info("啟動 radb agent", "version", buildinfo.Version, "host_id", *hostID)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a := agent.New(agent.Config{
		ServerURL: *serverURL,
		Token:     *token,
		HostID:    *hostID,
		ADBAddr:   fmt.Sprintf("127.0.0.1:%d", *adbPort),
		ICEConfig: iceConfig,
	})

	// 啟動 Direct Server（如有設定）
	if *directPort > 0 {
		dsrv := directsrv.New(directsrv.Config{
			DeviceTable: a.DeviceTable(),
			DialDevice: func(serial string, port int) (net.Conn, error) {
				return a.Dialer().DialDevice(serial, port)
			},
			Hostname: *hostID,
			Token:    *directToken,
		})
		go func() {
			addr := fmt.Sprintf("0.0.0.0:%d", *directPort)
			if err := dsrv.Serve(ctx, addr); err != nil && ctx.Err() == nil {
				slog.Error("Direct Server 錯誤", "error", err)
			}
		}()
		slog.Info("Direct Server 啟動", "port", *directPort)
	}

	// 如果有 signal token，連線 Signal Server；否則 direct-only 模式
	if *token != "" {
		if err := a.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("Agent 執行失敗", "error", err)
			os.Exit(1)
		}
	} else {
		if err := a.RunDirectOnly(ctx); err != nil && ctx.Err() == nil {
			slog.Error("Agent 執行失敗", "error", err)
			os.Exit(1)
		}
	}
	slog.Info("Agent 已關閉")
}

func localHostname() string {
	h, _ := os.Hostname()
	return h
}

// cmdUpdate 檢查 GitHub Releases 是否有新版本，並自動下載替換。
// --check 僅檢查不更新。更新流程：下載 → SHA256 校驗 → 解壓 → 替換 binary。
func cmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	checkOnly := fs.Bool("check", false, "只檢查是否有新版本，不執行更新")
	fs.Parse(args)

	u := updater.NewUpdater()
	ctx := context.Background()

	if *checkOnly {
		result, err := u.Check(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "檢查更新失敗: %v\n", err)
			os.Exit(1)
		}
		if !result.HasUpdate {
			fmt.Printf("目前已是最新版本 (%s)\n", result.CurrentVersion)
			return
		}
		fmt.Printf("有新版本可用！%s → %s\n", result.CurrentVersion, result.LatestVersion)
		fmt.Println("執行 radb update 進行更新")
		return
	}

	fmt.Println("檢查更新中...")
	result, err := u.Update(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "更新失敗: %v\n", err)
		os.Exit(1)
	}

	if !result.HasUpdate {
		fmt.Printf("目前已是最新版本 (%s)\n", result.CurrentVersion)
		return
	}

	fmt.Printf("更新成功！%s → %s\n", result.CurrentVersion, result.LatestVersion)
	if runtime.GOOS == "windows" {
		fmt.Println("請重新啟動相關程式以使用新版本")
	}
}

// cmdDaemon 啟動本機背景服務。
// Daemon 負責：連線到 Signal Server → 維護可用主機列表 → 透過 IPC 接受 CLI 指令（bind/unbind/list/status/hosts）
// → 建立 WebRTC P2P 連線 → 啟動 TCP 代理供本機 ADB 使用。
// IPC 在 Windows 上使用 TCP 127.0.0.1:15554，在 Unix 上使用 ~/.radb/daemon.sock。
func cmdDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	serverURL := fs.String("server", envStrFallback("RADB_SERVER_URL", "RADB_SIGNAL_URL", "ws://localhost:8080"), "Server 位址")
	token := fs.String("token", envStr("RADB_TOKEN", ""), "PSK Token")
	portStart := fs.Int("port-start", envInt("RADB_PORT_START", 15555), "Port 起始值")
	portEnd := fs.Int("port-end", envInt("RADB_PORT_END", 15655), "Port 結束值")
	stunURLs := fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN URLs")
	turnURL := fs.String("turn", envStr("RADB_TURN_URL", ""), "TURN URL")
	turnUser := fs.String("turn-user", envStr("RADB_TURN_USER", ""), "TURN 使用者")
	turnPass := fs.String("turn-pass", envStr("RADB_TURN_PASS", ""), "TURN 密碼")
	fs.Parse(args)

	if *token == "" {
		fmt.Fprintln(os.Stderr, "錯誤：必須設定 RADB_TOKEN 環境變數或使用 --token flag")
		os.Exit(1)
	}

	iceConfig := webrtc.ICEConfig{}
	if *stunURLs != "" {
		iceConfig.STUNServers = strings.Split(*stunURLs, ",")
	}
	if *turnURL != "" {
		iceConfig.TURNServers = []webrtc.TURNServer{
			{URL: *turnURL, Username: *turnUser, Credential: *turnPass},
		}
	}

	cfg := daemon.Config{
		ServerURL: *serverURL,
		Token:     *token,
		PortStart: *portStart,
		PortEnd:   *portEnd,
		ICEConfig: iceConfig,
	}

	d := daemon.NewDaemon(cfg)

	ipcLn, err := daemon.IPCListen()
	if err != nil {
		fmt.Fprintf(os.Stderr, "IPC 監聽失敗: %v\n", err)
		os.Exit(1)
	}
	defer ipcLn.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("radb daemon %s 啟動中...\n", buildinfo.Version)
	if err := d.Start(ctx, ipcLn); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "Daemon 錯誤: %v\n", err)
		os.Exit(1)
	}
}

func cmdBind(args []string) {
	fs := flag.NewFlagSet("bind", flag.ExitOnError)
	hostID := fs.String("host", "", "主機 ID")
	serial := fs.String("serial", "", "設備序號")
	fs.Parse(args)

	// 無指定 host/serial 時啟動互動式 TUI（bubbletea），引導使用者逐步選擇主機→設備
	if *hostID == "" || *serial == "" {
		m := cli.NewModel(ipcSender)
		p := tea.NewProgram(m)
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TUI 錯誤: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 直接綁定模式
	payload, _ := json.Marshal(daemon.BindRequest{HostID: *hostID, Serial: *serial})
	resp := sendIPCCommand(daemon.IPCCommand{Action: "bind", Payload: payload})

	if !resp.Success {
		fmt.Fprintf(os.Stderr, "綁定失敗: %s\n", resp.Error)
		os.Exit(1)
	}

	var result daemon.BindResult
	json.Unmarshal(resp.Data, &result)
	fmt.Printf("綁定成功！本機 port: %d, 設備: %s\n", result.LocalPort, result.Serial)
	fmt.Printf("使用方式: adb -s 127.0.0.1:%d shell\n", result.LocalPort)
}

// ipcSender 是供互動式 CLI 使用的 IPC 發送函式。
func ipcSender(cmd daemon.IPCCommand) daemon.IPCResponse {
	return sendIPCCommand(cmd)
}

func cmdUnbind(args []string) {
	fs := flag.NewFlagSet("unbind", flag.ExitOnError)
	port := fs.Int("port", 0, "本機 port")
	fs.Parse(args)

	if *port == 0 {
		fmt.Fprintln(os.Stderr, "用法: radb unbind --port PORT")
		os.Exit(1)
	}

	payload, _ := json.Marshal(daemon.UnbindRequest{LocalPort: *port})
	resp := sendIPCCommand(daemon.IPCCommand{Action: "unbind", Payload: payload})

	if !resp.Success {
		fmt.Fprintf(os.Stderr, "解綁失敗: %s\n", resp.Error)
		os.Exit(1)
	}
	fmt.Println("解綁成功")
}

func cmdList() {
	resp := sendIPCCommand(daemon.IPCCommand{Action: "list"})
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "查詢失敗: %s\n", resp.Error)
		os.Exit(1)
	}

	var bindings []daemon.Binding
	json.Unmarshal(resp.Data, &bindings)

	if len(bindings) == 0 {
		fmt.Println("目前沒有綁定的設備")
		return
	}

	fmt.Printf("%-8s %-20s %-15s %s\n", "Port", "Serial", "Host", "Status")
	fmt.Println(strings.Repeat("-", 60))
	for _, b := range bindings {
		fmt.Printf("%-8d %-20s %-15s %s\n", b.LocalPort, b.Serial, b.HostID, b.Status)
	}
}

func cmdStatus() {
	resp := sendIPCCommand(daemon.IPCCommand{Action: "status"})
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "查詢失敗: %s\n", resp.Error)
		os.Exit(1)
	}

	var status daemon.StatusInfo
	json.Unmarshal(resp.Data, &status)

	fmt.Printf("Server: %s\n", status.ServerURL)
	fmt.Printf("連線狀態:      %v\n", status.Connected)
	if status.ConnID != "" {
		fmt.Printf("連線 ID:       %s\n", status.ConnID)
	}
	fmt.Printf("綁定數量:      %d\n", status.BindCount)
}

func cmdHosts() {
	resp := sendIPCCommand(daemon.IPCCommand{Action: "hosts"})
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "查詢失敗: %s\n", resp.Error)
		os.Exit(1)
	}

	var hosts []struct {
		HostID   string `json:"host_id"`
		Hostname string `json:"hostname"`
		Devices  []struct {
			Serial string `json:"serial"`
			State  string `json:"state"`
			Lock   string `json:"lock"`
		} `json:"devices"`
	}
	json.Unmarshal(resp.Data, &hosts)

	if len(hosts) == 0 {
		fmt.Println("目前沒有可用的主機")
		return
	}

	for _, h := range hosts {
		fmt.Printf("主機: %s (%s)\n", h.Hostname, h.HostID)
		if len(h.Devices) == 0 {
			fmt.Println("  (無設備)")
		}
		for _, d := range h.Devices {
			fmt.Printf("  %s [%s] %s\n", d.Serial, d.State, d.Lock)
		}
	}
}

// sendIPCCommand 連線到 Daemon IPC 服務並發送指令。
func sendIPCCommand(cmd daemon.IPCCommand) daemon.IPCResponse {
	conn, err := daemon.IPCDial()
	if err != nil {
		fmt.Fprintf(os.Stderr, "無法連線到 daemon: %v\n", err)
		fmt.Fprintln(os.Stderr, "請確認 daemon 是否已啟動 (radb daemon)")
		os.Exit(1)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := json.NewEncoder(conn).Encode(cmd); err != nil {
		fmt.Fprintf(os.Stderr, "發送指令失敗: %v\n", err)
		os.Exit(1)
	}

	var resp daemon.IPCResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "讀取回應失敗: %v\n", err)
		os.Exit(1)
	}

	return resp
}

// --- 直連模式指令 ---
// 直連模式不需要 Signal Server，支援 LAN TCP 直連和 P2P SDP 配對兩種方式。
// 所有直連模式指令皆使用 bridge 套件的完整 ADB 多工橋接（device transport + forward 攔截），
// 取代舊版 direct connect 的單設備 io.Copy 簡易轉發。

// cmdConnect 是直連模式的統一入口。
// 根據第一個引數分派：
//   - "pair" → cmdConnectPair（P2P SDP 配對）
//   - 其他   → TCP 直連（支援 --list 僅查設備）
func cmdConnect(args []string) {
	if len(args) > 0 && args[0] == "pair" {
		cmdConnectPair(args[1:])
		return
	}

	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	listOnly := fs.Bool("list", false, "只列出遠端設備")
	token := fs.String("token", envStr("RADB_DIRECT_TOKEN", ""), "認證 Token")
	proxyPort := fs.Int("port", envInt("RADB_PROXY_PORT", 15037), "本機 ADB proxy port")
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server port")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb connect <地址:port> [--list] [--token TOKEN]")
		fmt.Fprintln(os.Stderr, "      radb connect pair [選項]")
		os.Exit(1)
	}
	addr := fs.Arg(0)

	if *listOnly {
		cmdConnectList(addr, *token)
		return
	}
	cmdConnectDirect(addr, *token, *proxyPort, *adbPort)
}

// cmdConnectList 查詢遠端 Agent 的設備列表並印出。
// 等同舊版 `radb direct list`，透過 directsrv 的 JSON 協定查詢。
func cmdConnectList(addr, token string) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "連線 Agent 失敗: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if err := json.NewEncoder(conn).Encode(directsrv.Request{Action: "list", Token: token}); err != nil {
		fmt.Fprintf(os.Stderr, "發送請求失敗: %v\n", err)
		os.Exit(1)
	}

	var resp directsrv.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "讀取回應失敗: %v\n", err)
		os.Exit(1)
	}

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "查詢失敗: %s\n", resp.Error)
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

// cmdConnectDirect 建立 TCP 直連的全設備多工 ADB 轉發。
// 取代舊版 `radb direct connect` 的單設備 io.Copy，改為：
//  1. 查詢遠端設備清單（驗證連線可用）
//  2. 建立 ForwardManager（管理設備清單 + forward 攔截）
//  3. 建立 OpenChannelFunc（透過 directsrv 的 connect-server / connect-service）
//  4. 在本機建立 ADB proxy listener
//  5. 背景輪詢設備清單
//  6. 自動 adb connect
//  7. Accept loop（每個連線由 ForwardManager.HandleProxyConn 處理）
func cmdConnectDirect(addr, token string, proxyPort, adbPort int) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 1. 查詢設備（驗證連線可用）
	devices := queryDirectDevices(addr, token)
	if len(devices) == 0 {
		fmt.Fprintln(os.Stderr, "遠端無可用設備")
		os.Exit(1)
	}
	for _, d := range devices {
		fmt.Printf("  設備: %s [%s]\n", d.Serial, d.State)
	}

	// 2. 建立 ForwardManager
	fm := bridge.NewForwardManager()
	bridgeDevices := make([]bridge.DeviceInfo, 0)
	for _, d := range devices {
		if d.State == "device" {
			bridgeDevices = append(bridgeDevices, bridge.DeviceInfo{
				Serial: d.Serial, State: d.State,
			})
		}
	}
	fm.UpdateDevices(bridgeDevices)

	// 3. 建立 OpenChannelFunc（透過 directsrv）
	openCh := makeDirectOpenChannel(addr, token)

	// 4. 建立 proxy listener
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 proxy listener 失敗: %v\n", err)
		os.Exit(1)
	}
	defer ln.Close()

	actualPort := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("ADB proxy 已啟動: 127.0.0.1:%d\n", actualPort)
	fmt.Printf("使用方式: adb connect 127.0.0.1:%d\n", actualPort)
	fmt.Println("按 Ctrl+C 結束")

	// 5. 背景輪詢設備
	go pollDirectDevices(ctx, addr, token, fm)

	// 6. 自動 adb connect
	go autoADBConnect(fmt.Sprintf("127.0.0.1:%d", adbPort), fmt.Sprintf("127.0.0.1:%d", actualPort))

	// context 取消時關閉 listener，解除 Accept 阻塞
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	// 7. Accept loop
	var connID atomic.Int64
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		id := connID.Add(1)
		go fm.HandleProxyConn(ctx, conn, openCh, id)
	}

	fm.KillReverseForwardAll()
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
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "連線 Agent 失敗: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(conn).Encode(directsrv.Request{Action: "list", Token: token}); err != nil {
		fmt.Fprintf(os.Stderr, "發送請求失敗: %v\n", err)
		os.Exit(1)
	}

	var resp directsrv.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "讀取回應失敗: %v\n", err)
		os.Exit(1)
	}

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "查詢失敗: %s\n", resp.Error)
		os.Exit(1)
	}

	if resp.Hostname != "" {
		fmt.Printf("主機: %s\n", resp.Hostname)
	}
	return resp.Devices
}

// queryDirectDevicesQuiet 靜默查詢遠端設備清單（不印輸出、不 exit）。
// 供 pollDirectDevices 使用，失敗時回傳 nil。
func queryDirectDevicesQuiet(addr, token string) []directsrv.DeviceInfo {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(conn).Encode(directsrv.Request{Action: "list", Token: token}); err != nil {
		return nil
	}

	var resp directsrv.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil
	}
	if !resp.OK {
		return nil
	}
	return resp.Devices
}

// pollDirectDevices 定期查詢遠端設備清單並更新 ForwardManager。
// 間隔 3 秒輪詢，僅同步 State=="device" 的在線設備。
func pollDirectDevices(ctx context.Context, addr, token string, fm *bridge.ForwardManager) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			devices := queryDirectDevicesQuiet(addr, token)
			bridgeDevices := make([]bridge.DeviceInfo, 0)
			for _, d := range devices {
				if d.State == "device" {
					bridgeDevices = append(bridgeDevices, bridge.DeviceInfo{
						Serial: d.Serial, State: d.State,
					})
				}
			}
			fm.UpdateDevices(bridgeDevices)
		}
	}
}

// autoADBConnect 嘗試自動執行 `adb connect` 到 proxy port。
// 若本機 ADB server 沒有在運行，靜默失敗（使用者可手動 connect）。
// 等待 500ms 讓 proxy listener 就緒後再嘗試。
func autoADBConnect(adbAddr, target string) {
	time.Sleep(500 * time.Millisecond)
	dialer := adb.NewDialer(adbAddr)
	if err := dialer.Connect(target); err != nil {
		slog.Debug("auto adb connect failed", "target", target, "error", err)
	} else {
		slog.Debug("auto adb connect succeeded", "target", target)
	}
}

// cmdConnectPair 透過 P2P SDP 配對建立全設備多工 ADB 轉發。
// 取代舊版 `radb pair offer` 的單設備 io.Copy，改為：
//  1. 建立 PeerConnection + control DataChannel
//  2. 產生 Offer SDP → 壓縮編碼為邀請碼 token
//  3. 使用者手動將邀請碼傳給被控端，貼入回應碼
//  4. HandleAnswer → 等待 P2P 連線建立
//  5. 啟動 control channel 讀取迴圈（接收設備清單）
//  6. 建立 ADB proxy listener → ForwardManager.HandleProxyConn 多工轉發
func cmdConnectPair(args []string) {
	fs := flag.NewFlagSet("connect pair", flag.ExitOnError)
	stunURLs := fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN Server URL")
	turnURL := fs.String("turn", envStr("RADB_TURN_URL", ""), "TURN Server URL")
	turnUser := fs.String("turn-user", envStr("RADB_TURN_USER", ""), "TURN 使用者名稱")
	turnPass := fs.String("turn-pass", envStr("RADB_TURN_PASS", ""), "TURN 密碼")
	proxyPort := fs.Int("port", envInt("RADB_PROXY_PORT", 15037), "本機 ADB proxy port")
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server port")
	fs.Parse(args)

	// 建立 ICE config
	iceConfig := webrtc.ICEConfig{}
	if *stunURLs != "" {
		iceConfig.STUNServers = strings.Split(*stunURLs, ",")
	}
	if *turnURL != "" {
		iceConfig.TURNServers = []webrtc.TURNServer{
			{URL: *turnURL, Username: *turnUser, Credential: *turnPass},
		}
	}

	// 建立 PeerConnection
	pm, err := webrtc.NewPeerManager(iceConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 PeerConnection 失敗: %v\n", err)
		os.Exit(1)
	}
	defer pm.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 建立 control DataChannel
	controlCh, err := pm.OpenChannel("control")
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 control channel 失敗: %v\n", err)
		os.Exit(1)
	}

	// 產生 compact SDP offer
	fmt.Fprintln(os.Stderr, "正在收集 ICE 候選...")
	offerSDP, err := pm.CreateOffer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 Offer 失敗: %v\n", err)
		os.Exit(1)
	}

	compact := bridge.SDPToCompact(offerSDP)
	token, err := bridge.EncodeToken(compact)
	if err != nil {
		fmt.Fprintf(os.Stderr, "編碼 token 失敗: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "\n邀請碼（複製給被控端）:")
	fmt.Println(token)
	fmt.Fprintln(os.Stderr, "\n請輸入回應碼:")

	// 讀取 answer token
	var answerToken string
	fmt.Scanln(&answerToken)

	answerCompact, err := bridge.DecodeToken(strings.TrimSpace(answerToken))
	if err != nil {
		fmt.Fprintf(os.Stderr, "無效的回應碼: %v\n", err)
		os.Exit(1)
	}

	answerSDP := bridge.CompactToSDP(answerCompact)
	if err := pm.HandleAnswer(answerSDP); err != nil {
		fmt.Fprintf(os.Stderr, "處理回應失敗: %v\n", err)
		os.Exit(1)
	}

	// 等待連線建立
	connCh := make(chan struct{})
	pm.OnConnected(func(relayed bool) {
		if relayed {
			fmt.Fprintln(os.Stderr, "注意：連線透過 TURN 中繼（延遲較高）")
		}
		close(connCh)
	})

	select {
	case <-connCh:
	case <-time.After(30 * time.Second):
		fmt.Fprintln(os.Stderr, "連線逾時")
		os.Exit(1)
	case <-ctx.Done():
		return
	}

	fmt.Fprintln(os.Stderr, "P2P 連線已建立")

	// 建立 ForwardManager
	fm := bridge.NewForwardManager()

	// 啟動 control channel 讀取（更新設備清單）
	go func() {
		bridge.ControlReadLoop(ctx, controlCh, func(cm bridge.CtrlMessage) {
			switch cm.Type {
			case "hello":
				fmt.Fprintf(os.Stderr, "遠端主機: %s\n", cm.Hostname)
			case "devices":
				fm.UpdateDevices(cm.Devices)
				online := 0
				for _, d := range cm.Devices {
					if d.State == "device" {
						online++
						fmt.Fprintf(os.Stderr, "  設備: %s\n", d.Serial)
					}
				}
				fmt.Fprintf(os.Stderr, "在線設備: %d 台\n", online)
			}
		})
	}()

	// 建立 proxy listener
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *proxyPort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 proxy listener 失敗: %v\n", err)
		os.Exit(1)
	}
	defer ln.Close()

	actualPort := ln.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stderr, "ADB proxy 已啟動: 127.0.0.1:%d\n", actualPort)
	fmt.Fprintf(os.Stderr, "使用方式: adb connect 127.0.0.1:%d\n", actualPort)
	fmt.Fprintln(os.Stderr, "按 Ctrl+C 結束")

	// 自動 adb connect
	go autoADBConnect(fmt.Sprintf("127.0.0.1:%d", *adbPort), fmt.Sprintf("127.0.0.1:%d", actualPort))

	// context 取消時關閉 listener
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	// Accept loop
	var connID atomic.Int64
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		id := connID.Add(1)
		go fm.HandleProxyConn(ctx, conn, pm.OpenChannel, id)
	}

	fm.KillReverseForwardAll()
}

// cmdAgentPair 處理一次性 P2P 被控端連線。
// 接收主控端的邀請碼（compact SDP），建立 Answer 並等待 P2P 連線。
// 連線建立後透過 control channel 推送設備清單，並由 ServerHandler
// 處理所有 DataChannel（adb-server/adb-stream/adb-fwd）。
func cmdAgentPair(args []string) {
	fs := flag.NewFlagSet("agent pair", flag.ExitOnError)
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server 埠")
	stunURLs := fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN Server URL")
	turnURL := fs.String("turn", envStr("RADB_TURN_URL", ""), "TURN Server URL")
	turnUser := fs.String("turn-user", envStr("RADB_TURN_USER", ""), "TURN 使用者名稱")
	turnPass := fs.String("turn-pass", envStr("RADB_TURN_PASS", ""), "TURN 密碼")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb agent pair <邀請碼> [--adb-port PORT] [--stun URLS]")
		os.Exit(1)
	}
	offerToken := fs.Arg(0)

	// 解碼 compact SDP offer
	offerCompact, err := bridge.DecodeToken(strings.TrimSpace(offerToken))
	if err != nil {
		fmt.Fprintf(os.Stderr, "無效的邀請碼: %v\n", err)
		os.Exit(1)
	}

	offerSDP := bridge.CompactToSDP(offerCompact)

	// 建立 ICE config
	iceConfig := webrtc.ICEConfig{}
	if *stunURLs != "" {
		iceConfig.STUNServers = strings.Split(*stunURLs, ",")
	}
	if *turnURL != "" {
		iceConfig.TURNServers = []webrtc.TURNServer{
			{URL: *turnURL, Username: *turnUser, Credential: *turnPass},
		}
	}

	// 建立 PeerConnection
	pm, err := webrtc.NewPeerManager(iceConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 PeerConnection 失敗: %v\n", err)
		os.Exit(1)
	}
	defer pm.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	adbAddr := fmt.Sprintf("127.0.0.1:%d", *adbPort)
	handler := &bridge.ServerHandler{ADBAddr: adbAddr}

	// 設定 DataChannel 處理
	pm.OnChannel(func(label string, rwc io.ReadWriteCloser) {
		if label == "control" {
			// control channel — 啟動設備推送
			go bridge.DevicePushLoop(ctx, rwc, adbAddr, func(devices []bridge.DeviceInfo) {
				online := 0
				for _, d := range devices {
					if d.State == "device" {
						online++
					}
				}
				fmt.Fprintf(os.Stderr, "在線設備: %d 台\n", online)
			})
			return
		}
		// 其他 DataChannel 由 ServerHandler 分派
		if !handler.HandleChannel(ctx, label, rwc) {
			slog.Warn("unknown DataChannel label", "label", label)
			rwc.Close()
		}
	})

	// 處理 Offer
	fmt.Fprintln(os.Stderr, "正在處理邀請碼並收集 ICE 候選...")
	answerSDP, err := pm.HandleOffer(offerSDP)
	if err != nil {
		fmt.Fprintf(os.Stderr, "處理 Offer 失敗: %v\n", err)
		os.Exit(1)
	}

	// 產生 compact SDP answer
	answerCompact := bridge.SDPToCompact(answerSDP)
	answerTokenStr, err := bridge.EncodeToken(answerCompact)
	if err != nil {
		fmt.Fprintf(os.Stderr, "編碼回應碼失敗: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "\n回應碼（複製回主控端）:")
	fmt.Println(answerTokenStr)
	fmt.Fprintln(os.Stderr, "\n等待連線...")

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\n已停止")
}

// cmdDiscover 掃描區網上的 radb Agent（mDNS）。
// 等同舊版 `radb direct discover`，直接委派。
func cmdDiscover(args []string) {
	cmdDirectDiscover(args)
}

// --- 舊版 Direct 模式指令（隱藏別名，向後相容） ---
// 以下指令不顯示在 printUsage 中，但仍可正常使用。
// Agent 端開啟 TCP 服務，Client 端透過 TCP 直接連線並進行 ADB 轉發。

// cmdDirect 分派 direct 子命令：discover（mDNS 掃描）、list（查詢設備）、connect（TCP 直連轉發）。
func cmdDirect(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb direct <discover|list|connect> [選項]")
		os.Exit(1)
	}

	switch args[0] {
	case "discover":
		cmdDirectDiscover(args[1:])
	case "list":
		cmdDirectList(args[1:])
	case "connect":
		cmdDirectConnect(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: direct %s\n", args[0])
		os.Exit(1)
	}
}

func cmdDirectDiscover(_ []string) {
	fmt.Println("正在掃描 LAN 上的 radb Agent...")

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

func cmdDirectList(args []string) {
	fs := flag.NewFlagSet("direct list", flag.ExitOnError)
	token := fs.String("token", "", "認證 Token")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb direct list <agent地址:port> [--token TOKEN]")
		os.Exit(1)
	}
	addr := fs.Arg(0)

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "連線 Agent 失敗: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if err := json.NewEncoder(conn).Encode(directsrv.Request{Action: "list", Token: *token}); err != nil {
		fmt.Fprintf(os.Stderr, "發送請求失敗: %v\n", err)
		os.Exit(1)
	}

	var resp directsrv.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "讀取回應失敗: %v\n", err)
		os.Exit(1)
	}

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "查詢失敗: %s\n", resp.Error)
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

func cmdDirectConnect(args []string) {
	fs := flag.NewFlagSet("direct connect", flag.ExitOnError)
	serial := fs.String("serial", "", "設備序號")
	token := fs.String("token", "", "認證 Token")
	port := fs.Int("port", 15555, "本機監聽 port")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb direct connect <agent地址:port> --serial SERIAL [--token TOKEN] [--port PORT]")
		os.Exit(1)
	}
	if *serial == "" {
		fmt.Fprintln(os.Stderr, "錯誤：必須指定 --serial")
		os.Exit(1)
	}
	addr := fs.Arg(0)

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "連線 Agent 失敗: %v\n", err)
		os.Exit(1)
	}

	if err := json.NewEncoder(conn).Encode(directsrv.Request{Action: "connect", Serial: *serial, Token: *token}); err != nil {
		conn.Close()
		fmt.Fprintf(os.Stderr, "發送請求失敗: %v\n", err)
		os.Exit(1)
	}

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	var resp directsrv.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		conn.Close()
		fmt.Fprintf(os.Stderr, "讀取回應失敗: %v\n", err)
		os.Exit(1)
	}
	conn.SetDeadline(time.Time{}) // 清除 deadline

	if !resp.OK {
		conn.Close()
		fmt.Fprintf(os.Stderr, "連線設備失敗: %s\n", resp.Error)
		os.Exit(1)
	}

	// 建立本機 TCP 代理
	p, err := proxy.New(*port, conn)
	if err != nil {
		conn.Close()
		fmt.Fprintf(os.Stderr, "建立代理失敗: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	p.Start(ctx)

	fmt.Printf("ADB 轉發已建立 127.0.0.1:%d → %s\n", p.Port(), *serial)
	fmt.Printf("使用方式: adb -s 127.0.0.1:%d shell\n", p.Port())
	fmt.Println("按 Ctrl+C 結束")

	<-ctx.Done()
	p.Stop()
	fmt.Println("\n轉發已停止")
}

// --- Pair 模式指令（手動 SDP 交換） ---
//
// Pair 模式不需要任何 Server，透過手動複製貼上 SDP token 完成 WebRTC 打洞。
// 流程：Client 生成 Offer token → 人工傳遞給 Agent → Agent 回傳 Answer token → 連線建立。
// 與 Direct 模式不同，Pair 模式能跨 NAT（藉由 STUN/TURN ICE 穿透）。

// PairOffer 是 Client 生成的 offer token 結構，包含 SDP、目標設備序號及 session ID。
type PairOffer struct {
	SDP       string `json:"sdp"`
	Serial    string `json:"serial"`
	SessionID string `json:"session_id"`
}

// PairAnswer 是 Agent 回傳的 answer token 結構。
type PairAnswer struct {
	SDP string `json:"sdp"`
}

func cmdPair(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb pair <offer|answer> [選項]")
		os.Exit(1)
	}

	switch args[0] {
	case "offer":
		cmdPairOffer(args[1:])
	case "answer":
		cmdPairAnswer(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: pair %s\n", args[0])
		os.Exit(1)
	}
}

func cmdPairOffer(args []string) {
	fs := flag.NewFlagSet("pair offer", flag.ExitOnError)
	serial := fs.String("serial", "", "設備序號")
	stunURLs := fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN Server URL")
	port := fs.Int("port", 15555, "本機監聽 port")
	fs.Parse(args)

	if *serial == "" {
		fmt.Fprintln(os.Stderr, "錯誤：必須指定 --serial")
		os.Exit(1)
	}

	iceConfig := webrtc.ICEConfig{}
	if *stunURLs != "" {
		iceConfig.STUNServers = strings.Split(*stunURLs, ",")
	}

	pm, err := webrtc.NewPeerManager(iceConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 PeerConnection 失敗: %v\n", err)
		os.Exit(1)
	}
	defer pm.Close()

	sessionID := fmt.Sprintf("pair-%d", time.Now().UnixNano())
	label := fmt.Sprintf("adb/%s/%s", *serial, sessionID)

	channel, err := pm.OpenChannel(label)
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 DataChannel 失敗: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "正在收集 ICE 候選...")
	offerSDP, err := pm.CreateOffer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 Offer 失敗: %v\n", err)
		os.Exit(1)
	}

	// 編碼 offer token
	offerJSON, _ := json.Marshal(PairOffer{SDP: offerSDP, Serial: *serial, SessionID: sessionID})
	offerToken := base64.StdEncoding.EncodeToString(offerJSON)

	fmt.Fprintln(os.Stderr, "\nOffer（複製到 Agent 端）:")
	fmt.Println(offerToken)
	fmt.Fprintln(os.Stderr, "\n等待 Answer...")

	// 讀取 answer token
	var answerToken string
	fmt.Scanln(&answerToken)

	answerJSON, err := base64.StdEncoding.DecodeString(strings.TrimSpace(answerToken))
	if err != nil {
		fmt.Fprintf(os.Stderr, "無效的 Answer token: %v\n", err)
		os.Exit(1)
	}

	var answer PairAnswer
	if err := json.Unmarshal(answerJSON, &answer); err != nil {
		fmt.Fprintf(os.Stderr, "解析 Answer 失敗: %v\n", err)
		os.Exit(1)
	}

	if err := pm.HandleAnswer(answer.SDP); err != nil {
		fmt.Fprintf(os.Stderr, "處理 Answer 失敗: %v\n", err)
		os.Exit(1)
	}

	// 建立代理
	p, err := proxy.New(*port, channel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立代理失敗: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	p.Start(ctx)

	fmt.Fprintf(os.Stderr, "\n連線成功！ADB 轉發 127.0.0.1:%d → %s\n", p.Port(), *serial)
	fmt.Fprintf(os.Stderr, "使用方式: adb -s 127.0.0.1:%d shell\n", p.Port())
	fmt.Fprintln(os.Stderr, "按 Ctrl+C 結束")

	<-ctx.Done()
	p.Stop()
	fmt.Fprintln(os.Stderr, "\n轉發已停止")
}

func cmdPairAnswer(args []string) {
	fs := flag.NewFlagSet("pair answer", flag.ExitOnError)
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server 埠")
	stunURLs := fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN Server URL")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb pair answer <offer-token> [--adb-port PORT] [--stun URLS]")
		os.Exit(1)
	}
	offerToken := fs.Arg(0)

	// 解碼 offer token
	offerJSON, err := base64.StdEncoding.DecodeString(strings.TrimSpace(offerToken))
	if err != nil {
		fmt.Fprintf(os.Stderr, "無效的 Offer token: %v\n", err)
		os.Exit(1)
	}

	var offer PairOffer
	if err := json.Unmarshal(offerJSON, &offer); err != nil {
		fmt.Fprintf(os.Stderr, "解析 Offer 失敗: %v\n", err)
		os.Exit(1)
	}

	iceConfig := webrtc.ICEConfig{}
	if *stunURLs != "" {
		iceConfig.STUNServers = strings.Split(*stunURLs, ",")
	}

	pm, err := webrtc.NewPeerManager(iceConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 PeerConnection 失敗: %v\n", err)
		os.Exit(1)
	}
	defer pm.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 設定 DataChannel 處理：連線到本機 ADB 設備
	dialer := adb.NewDialer(fmt.Sprintf("127.0.0.1:%d", *adbPort))

	pm.OnChannel(func(label string, rwc io.ReadWriteCloser) {
		go func() {
			defer rwc.Close()
			parts := strings.SplitN(label, "/", 3)
			if len(parts) < 2 || parts[0] != "adb" {
				slog.Warn("無效的 DataChannel label", "label", label)
				return
			}
			serial := parts[1]

			slog.Info("開始 ADB 轉發", "serial", serial)
			adbConn, err := dialer.DialDevice(serial, 5555)
			if err != nil {
				slog.Error("連線 ADB 設備失敗", "serial", serial, "error", err)
				return
			}
			defer adbConn.Close()

			errc := make(chan error, 2)
			go func() { _, err := io.Copy(adbConn, rwc); errc <- err }()
			go func() { _, err := io.Copy(rwc, adbConn); errc <- err }()

			select {
			case err := <-errc:
				if err != nil {
					slog.Debug("ADB 轉發結束", "error", err)
				}
			case <-ctx.Done():
			}
			slog.Info("ADB 轉發已停止", "serial", serial)
		}()
	})

	// 處理 Offer
	fmt.Fprintln(os.Stderr, "正在處理 Offer 並收集 ICE 候選...")
	answerSDP, err := pm.HandleOffer(offer.SDP)
	if err != nil {
		fmt.Fprintf(os.Stderr, "處理 Offer 失敗: %v\n", err)
		os.Exit(1)
	}

	// 編碼 answer token
	answerJSON, _ := json.Marshal(PairAnswer{SDP: answerSDP})
	answerTokenStr := base64.StdEncoding.EncodeToString(answerJSON)

	fmt.Fprintln(os.Stderr, "\nAnswer（複製回 Client 端）:")
	fmt.Println(answerTokenStr)
	fmt.Fprintln(os.Stderr, "\n等待連線...")

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\n已停止")
}

// --- 環境變數讀取輔助函式 ---
// 所有 flag 的預設值皆可透過環境變數覆蓋（如 RADB_TOKEN、RADB_SERVER_URL 等）。

// envStr 從環境變數讀取字串，不存在時回傳 fallback。
func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envStrFallback 先嘗試 key，再嘗試 fallbackKey，最後回傳 fallback。
// 用於支援環境變數改名的向後相容（例如 RADB_SERVER_URL 取代舊的 RADB_SIGNAL_URL）。
func envStrFallback(key, fallbackKey, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := os.Getenv(fallbackKey); v != "" {
		return v
	}
	return fallback
}

// setupGUILog 在 GUI 模式下設置 crash log。
// 將 slog 和 panic 輸出重導到執行檔同目錄的 radb.log。
func setupGUILog() *os.File {
	exePath, err := os.Executable()
	if err != nil {
		return nil
	}
	logPath := filepath.Join(filepath.Dir(exePath), "radb.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil
	}

	// slog 寫入 log 檔
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Go runtime panic 輸出寫入 log 檔
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		slog.Warn("SetCrashOutput 失敗", "error", err)
	}

	// 讓 fmt.Fprintf(os.Stderr, ...) 也寫入 log 檔
	os.Stderr = f

	return f
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
