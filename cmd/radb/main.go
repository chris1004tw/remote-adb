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
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/agent"
	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/cli"
	"github.com/chris1004tw/remote-adb/internal/daemon"
	"github.com/chris1004tw/remote-adb/internal/directsrv"
	"github.com/chris1004tw/remote-adb/internal/gui"
	"github.com/chris1004tw/remote-adb/internal/proxy"
	signalpkg "github.com/chris1004tw/remote-adb/internal/signal"
	"github.com/chris1004tw/remote-adb/internal/updater"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

func main() {
	// 清理上次更新留下的 .old 備份檔案
	if selfPath, err := os.Executable(); err == nil {
		updater.CleanupOldBinaries(filepath.Dir(selfPath))
	}

	if len(os.Args) < 2 {
		gui.Run() // 無引數 → 啟動 GUI
		return
	}

	// 有引數 → CLI 模式（Windows 附加父行程主控台）
	attachParentConsole()

	switch os.Args[1] {
	case "server":
		cmdServer(os.Args[2:])
	case "agent":
		cmdAgent(os.Args[2:])
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
	fmt.Fprintf(os.Stderr, "  server    啟動信令伺服器\n")
	fmt.Fprintf(os.Stderr, "  agent     啟動遠端代理\n")
	fmt.Fprintf(os.Stderr, "  daemon    啟動背景服務\n")
	fmt.Fprintf(os.Stderr, "  bind      綁定遠端設備\n")
	fmt.Fprintf(os.Stderr, "  unbind    解除綁定\n")
	fmt.Fprintf(os.Stderr, "  list      列出已綁定設備\n")
	fmt.Fprintf(os.Stderr, "  status    查詢 daemon 狀態\n")
	fmt.Fprintf(os.Stderr, "  hosts     列出可用主機\n")
	fmt.Fprintf(os.Stderr, "\nDirect 模式（無需 Server）:\n")
	fmt.Fprintf(os.Stderr, "  direct    TCP 直連（discover/list/connect）\n")
	fmt.Fprintf(os.Stderr, "  pair      手動 SDP 配對（offer/answer）\n")
	fmt.Fprintf(os.Stderr, "\n其他:\n")
	fmt.Fprintf(os.Stderr, "  update    檢查並更新到最新版本\n")
	fmt.Fprintf(os.Stderr, "  version   顯示版本\n")
}

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

	// 無 flag 時啟動互動式 TUI
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

// --- Direct 模式指令 ---

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

// PairOffer 是 Client 生成的 offer token 結構。
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

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envStrFallback(key, fallbackKey, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := os.Getenv(fallbackKey); v != "" {
		return v
	}
	return fallback
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
