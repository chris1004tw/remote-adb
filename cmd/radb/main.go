// Package main 是 radb 的統一入口。
//
// radb 採用「模組 + 子命令」的階層式 CLI 結構，與 GUI 三分頁一一對應：
//
//	P2P 模式（簡易點對點連接）：
//	  p2p connect  — [主控端] 產生邀請碼並等待回應碼
//	  p2p agent    — [被控端] 輸入邀請碼並產生回應碼
//
//	Direct 模式（區網直連）：
//	  direct connect <addr>  — [主控端] TCP 直連指定的被控端 IP
//	  direct discover        — [主控端] 掃描區網內可用的被控端 (mDNS)
//	  direct agent           — [被控端] 在區網內開啟監聽，分享本機設備
//
//	Relay 模式（透過中繼伺服器）：
//	  relay daemon   — [主控端] 啟動背景服務
//	  relay bind     — [主控端] 透過伺服器綁定遠端設備
//	  relay unbind   — [主控端] 解除綁定設備
//	  relay list     — [主控端] 列出已綁定的設備
//	  relay hosts    — [主控端] 列出伺服器上所有可用的被控端
//	  relay status   — [主控端] 查詢 daemon 狀態
//	  relay agent    — [被控端] 連線至伺服器，分享本機 USB 設備
//	  relay server   — [伺服器] 啟動 WebSocket 信令伺服器
//
//	系統管理：
//	  update  — 從 GitHub Releases 下載最新版本並替換自身
//	  version — 顯示版本資訊
//
// 無引數執行時進入 GUI 模式（Gio 圖形介面）。
package main

import (
	"bufio"
	"context"
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
		freeConsole() // Windows: 脫離主控台避免閃爍（非 Windows 為空操作）
		if f := setupGUILog(); f != nil {
			defer f.Close()
		}
		gui.Run() // 無引數 → 啟動 GUI
		return
	}

	// 有引數 → CLI 模式（console subsystem，stdin/stdout/stderr 正常繼承）

	switch os.Args[1] {
	case "p2p":
		cmdP2P(os.Args[2:])
	case "direct":
		cmdDirectModule(os.Args[2:])
	case "relay":
		cmdRelay(os.Args[2:])
	case "update":
		cmdUpdate(os.Args[2:])
	case "version":
		fmt.Printf("radb %s (commit: %s, built: %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
	default:
		fmt.Fprintf(os.Stderr, "未知指令: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "用法: radb <模組> <子命令> [選項]\n\n")
	fmt.Fprintf(os.Stderr, "P2P 模式（簡易點對點連接）:\n")
	fmt.Fprintf(os.Stderr, "  p2p connect       [主控端] 產生邀請碼並等待回應碼\n")
	fmt.Fprintf(os.Stderr, "  p2p agent         [被控端] 輸入邀請碼並產生回應碼\n")
	fmt.Fprintf(os.Stderr, "\nDirect 模式（區網直連）:\n")
	fmt.Fprintf(os.Stderr, "  direct connect    [主控端] TCP 直連指定的被控端 IP\n")
	fmt.Fprintf(os.Stderr, "  direct discover   [主控端] 掃描區網內可用的被控端 (mDNS)\n")
	fmt.Fprintf(os.Stderr, "  direct agent      [被控端] 在區網內開啟監聽，分享本機設備\n")
	fmt.Fprintf(os.Stderr, "\nRelay 模式（透過中繼伺服器）:\n")
	fmt.Fprintf(os.Stderr, "  relay daemon      [主控端] 啟動背景服務\n")
	fmt.Fprintf(os.Stderr, "  relay bind        [主控端] 透過伺服器綁定遠端設備\n")
	fmt.Fprintf(os.Stderr, "  relay unbind      [主控端] 解除綁定設備\n")
	fmt.Fprintf(os.Stderr, "  relay list        [主控端] 列出已綁定的設備\n")
	fmt.Fprintf(os.Stderr, "  relay hosts       [主控端] 列出伺服器上所有可用的被控端\n")
	fmt.Fprintf(os.Stderr, "  relay status      [主控端] 查詢 daemon 狀態\n")
	fmt.Fprintf(os.Stderr, "  relay agent       [被控端] 連線至伺服器，分享本機 USB 設備\n")
	fmt.Fprintf(os.Stderr, "  relay server      [伺服器] 啟動 WebSocket 信令伺服器\n")
	fmt.Fprintf(os.Stderr, "\n系統管理:\n")
	fmt.Fprintf(os.Stderr, "  update            檢查並更新到最新版本\n")
	fmt.Fprintf(os.Stderr, "  version           顯示版本資訊\n")
}

// --- 模組路由 ---

// cmdP2P 分派 P2P 模式子命令：connect（主控端）、agent（被控端）。
// P2P 模式透過手動複製貼上 SDP token 完成 WebRTC 打洞，能跨 NAT 穿透。
func cmdP2P(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb p2p <connect|agent> [選項]")
		os.Exit(1)
	}
	switch args[0] {
	case "connect":
		cmdConnectPair(args[1:])
	case "agent":
		cmdAgentPair(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: p2p %s\n", args[0])
		os.Exit(1)
	}
}

// cmdDirectModule 分派 Direct 模式子命令：connect（TCP 直連）、discover（mDNS 掃描）、agent（被控端）。
// Direct 模式在同一區網內透過 TCP 直連，不需要任何伺服器。
func cmdDirectModule(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb direct <connect|discover|agent> [選項]")
		os.Exit(1)
	}
	switch args[0] {
	case "connect":
		cmdConnect(args[1:])
	case "discover":
		cmdDiscover()
	case "agent":
		cmdDirectAgent(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: direct %s\n", args[0])
		os.Exit(1)
	}
}

// cmdRelay 分派 Relay 模式子命令。
// Relay 模式透過中央 WebSocket 信令伺服器交換信令，再建立 WebRTC P2P 連線。
func cmdRelay(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "用法: radb relay <daemon|bind|unbind|list|hosts|status|agent|server> [選項]")
		os.Exit(1)
	}
	switch args[0] {
	case "daemon":
		cmdDaemon(args[1:])
	case "bind":
		cmdBind(args[1:])
	case "unbind":
		cmdUnbind(args[1:])
	case "list":
		cmdList()
	case "hosts":
		cmdHosts()
	case "status":
		cmdStatus()
	case "agent":
		cmdRelayAgent(args[1:])
	case "server":
		cmdServer(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: relay %s\n", args[0])
		os.Exit(1)
	}
}

// cmdServer 啟動 Relay Server（WebSocket 信令伺服器，radb relay server）。
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

// cmdRelayAgent 啟動 Relay 模式的遠端被控端。
// 連線到 Relay Server（WebSocket 信令），接收主控端的設備綁定請求，
// 透過 WebRTC P2P 轉發 ADB 連線。
func cmdRelayAgent(args []string) {
	fs := flag.NewFlagSet("relay agent", flag.ExitOnError)
	serverURL := fs.String("server", envStrFallback("RADB_SERVER_URL", "RADB_SIGNAL_URL", "ws://localhost:8080"), "Relay Server WebSocket 位址")
	token := fs.String("token", envStr("RADB_TOKEN", ""), "PSK Token")
	hostID := fs.String("host-id", envStr("RADB_HOST_ID", localHostname()), "主機識別名稱")
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server 埠")
	stunURLs := fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN Server URL")
	turnMode := fs.String("turn-mode", envStr("RADB_TURN_MODE", "cloudflare"), "TURN 模式 (cloudflare/custom/none)")
	turnURL := fs.String("turn", envStr("RADB_TURN_URL", ""), "TURN Server URL（turn-mode=custom 時使用）")
	turnUser := fs.String("turn-user", envStr("RADB_TURN_USER", ""), "TURN 使用者名稱")
	turnPass := fs.String("turn-pass", envStr("RADB_TURN_PASS", ""), "TURN 密碼")
	fs.Parse(args)

	if *token == "" {
		fmt.Fprintln(os.Stderr, "錯誤：必須設定 RADB_TOKEN 環境變數或使用 --token flag")
		os.Exit(1)
	}

	iceConfig := buildICEConfig(*stunURLs, *turnMode, *turnURL, *turnUser, *turnPass)

	slog.Info("啟動 radb relay agent", "version", buildinfo.Version, "host_id", *hostID)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a := agent.New(agent.Config{
		ServerURL: *serverURL,
		Token:     *token,
		HostID:    *hostID,
		ADBAddr:   fmt.Sprintf("127.0.0.1:%d", *adbPort),
		ICEConfig: iceConfig,
	})

	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("Agent 執行失敗", "error", err)
		os.Exit(1)
	}
	slog.Info("Agent 已關閉")
}

// cmdDirectAgent 啟動 Direct 模式的區網被控端。
// 在指定 port 開啟 TCP 直連服務 + mDNS 廣播，供同一區網的主控端連線。
func cmdDirectAgent(args []string) {
	fs := flag.NewFlagSet("direct agent", flag.ExitOnError)
	port := fs.Int("port", envInt("RADB_DIRECT_PORT", 9000), "TCP 監聽埠")
	token := fs.String("token", envStr("RADB_DIRECT_TOKEN", ""), "認證 Token")
	hostID := fs.String("host-id", envStr("RADB_HOST_ID", localHostname()), "主機識別名稱")
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server 埠")
	fs.Parse(args)

	slog.Info("啟動 radb direct agent", "version", buildinfo.Version, "host_id", *hostID, "port", *port)

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
			slog.Error("設備追蹤失敗", "error", err)
		}
	}()

	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	fmt.Printf("Direct Agent 已啟動: %s\n", addr)
	fmt.Println("按 Ctrl+C 結束")

	if err := dsrv.Serve(ctx, addr); err != nil && ctx.Err() == nil {
		slog.Error("Direct Server 錯誤", "error", err)
		os.Exit(1)
	}
	slog.Info("Direct Agent 已關閉")
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

// cmdDaemon 啟動本機背景服務（radb relay daemon）。
// Daemon 負責：連線到 Relay Server → 維護可用主機列表 → 透過 IPC 接受 CLI 指令（relay bind/unbind/list/status/hosts）
// → 建立 WebRTC P2P 連線 → 啟動 TCP 代理供本機 ADB 使用。
// IPC 在 Windows 上使用 TCP 127.0.0.1:15554，在 Unix 上使用 ~/.radb/daemon.sock。
func cmdDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	serverURL := fs.String("server", envStrFallback("RADB_SERVER_URL", "RADB_SIGNAL_URL", "ws://localhost:8080"), "Server 位址")
	token := fs.String("token", envStr("RADB_TOKEN", ""), "PSK Token")
	portStart := fs.Int("port-start", envInt("RADB_PORT_START", 15555), "Port 起始值")
	portEnd := fs.Int("port-end", envInt("RADB_PORT_END", 15655), "Port 結束值")
	stunURLs := fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN URLs")
	turnMode := fs.String("turn-mode", envStr("RADB_TURN_MODE", "cloudflare"), "TURN 模式 (cloudflare/custom/none)")
	turnURL := fs.String("turn", envStr("RADB_TURN_URL", ""), "TURN URL（turn-mode=custom 時使用）")
	turnUser := fs.String("turn-user", envStr("RADB_TURN_USER", ""), "TURN 使用者")
	turnPass := fs.String("turn-pass", envStr("RADB_TURN_PASS", ""), "TURN 密碼")
	fs.Parse(args)

	if *token == "" {
		fmt.Fprintln(os.Stderr, "錯誤：必須設定 RADB_TOKEN 環境變數或使用 --token flag")
		os.Exit(1)
	}

	iceConfig := buildICEConfig(*stunURLs, *turnMode, *turnURL, *turnUser, *turnPass)

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
		fmt.Fprintln(os.Stderr, "請確認 daemon 是否已啟動 (radb relay daemon)")
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

// --- Direct 模式指令（radb direct *） ---
// Direct 模式不需要任何伺服器，在同一區網透過 TCP 直連。
// 所有 Direct 模式指令皆使用 bridge 套件的完整 ADB 多工橋接（device transport + forward 攔截）。

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
	bridgeDevices := make([]bridge.DeviceInfo, 0)
	for _, d := range devices {
		bridgeDevices = append(bridgeDevices, bridge.DeviceInfo{
			Serial: d.Serial, State: d.State,
		})
	}
	dpm.UpdateDevices(bridgeDevices)

	fmt.Println("按 Ctrl+C 結束")

	// 背景輪詢設備
	go pollDirectDevicesDPM(ctx, addr, token, dpm)

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

// cliDeviceProxyCallbacks 回傳 CLI 用的 DeviceProxyManager OnReady/OnRemoved callback。
// 設備上線時印出 proxy port 並自動 adb connect，離線時自動 adb disconnect。
func cliDeviceProxyCallbacks(adbAddr string) (onReady func(string, int), onRemoved func(string, int)) {
	onReady = func(serial string, port int) {
		fmt.Fprintf(os.Stderr, "  設備 %s → 127.0.0.1:%d\n", serial, port)
		go autoADBConnect(adbAddr, fmt.Sprintf("127.0.0.1:%d", port))
	}
	onRemoved = func(serial string, port int) {
		fmt.Fprintf(os.Stderr, "  設備 %s 已離線（port %d 已釋放）\n", serial, port)
		go func() {
			dialer := adb.NewDialer(adbAddr)
			dialer.Disconnect(fmt.Sprintf("127.0.0.1:%d", port))
		}()
	}
	return
}

// queryDirectDevicesQuiet 靜默查詢遠端設備清單（不印輸出、不 exit）。
// 供 pollDirectDevicesDPM 輪詢使用，失敗時回傳 nil。
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

// pollDirectDevicesDPM 定期查詢遠端設備清單並更新 DeviceProxyManager。
// 間隔 3 秒輪詢，DeviceProxyManager 內部負責過濾 offline 設備並管理 per-device proxy。
func pollDirectDevicesDPM(ctx context.Context, addr, token string, dpm *bridge.DeviceProxyManager) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			devices := queryDirectDevicesQuiet(addr, token)
			bridgeDevices := make([]bridge.DeviceInfo, 0, len(devices))
			for _, d := range devices {
				bridgeDevices = append(bridgeDevices, bridge.DeviceInfo{
					Serial: d.Serial, State: d.State,
				})
			}
			dpm.UpdateDevices(bridgeDevices)
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

// cmdConnectPair 透過 P2P SDP 配對建立全設備多工 ADB 轉發（radb p2p connect）。
// 流程：
//  1. 建立 PeerConnection + control DataChannel
//  2. 產生 Offer SDP → 壓縮編碼為邀請碼 token
//  3. 使用者手動將邀請碼傳給被控端，貼入回應碼
//  4. HandleAnswer → 等待 P2P 連線建立
//  5. 啟動 control channel 讀取迴圈（接收設備清單）
//  6. 建立 DeviceProxyManager（每台設備獨立 proxy port）
func cmdConnectPair(args []string) {
	fs := flag.NewFlagSet("p2p connect", flag.ExitOnError)
	stunURLs := fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN Server URL")
	turnMode := fs.String("turn-mode", envStr("RADB_TURN_MODE", "cloudflare"), "TURN 模式 (cloudflare/custom/none)")
	turnURL := fs.String("turn", envStr("RADB_TURN_URL", ""), "TURN Server URL（turn-mode=custom 時使用）")
	turnUser := fs.String("turn-user", envStr("RADB_TURN_USER", ""), "TURN 使用者名稱")
	turnPass := fs.String("turn-pass", envStr("RADB_TURN_PASS", ""), "TURN 密碼")
	portStart := fs.Int("port", envInt("RADB_PROXY_PORT", 5555), "本機 ADB proxy port 起始值")
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server port")
	fs.Parse(args)

	// 建立 ICE config（支援 Cloudflare 免費 TURN）
	iceConfig := buildICEConfig(*stunURLs, *turnMode, *turnURL, *turnUser, *turnPass)

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

	// 讀取回應碼（重試迴圈：空輸入或無效 token 時重新要求，
	// 因為邀請碼僅限使用一次，不能因誤按 Enter 就放棄）
	scanner := bufio.NewScanner(os.Stdin)
	var answerCompact bridge.CompactSDP
	for {
		var answerToken string
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				answerToken = line
				break
			}
		}
		if answerToken == "" {
			// stdin EOF（管線結束），無法重試
			fmt.Fprintln(os.Stderr, "未輸入回應碼")
			os.Exit(1)
		}
		compact, decErr := bridge.DecodeToken(answerToken)
		if decErr != nil {
			fmt.Fprintf(os.Stderr, "無效的回應碼（%v），請重新輸入:\n", decErr)
			continue
		}
		answerCompact = compact
		break
	}

	// 先註冊回呼再啟動 ICE，避免 LAN 環境下 ICE 在毫秒內完成導致回呼未觸發
	connCh := make(chan struct{})
	pm.OnConnected(func(relayed bool) {
		if relayed {
			fmt.Fprintln(os.Stderr, "注意：連線透過 TURN 中繼（延遲較高）")
		}
		close(connCh)
	})

	answerSDP := bridge.CompactToSDP(answerCompact)
	if err := pm.HandleAnswer(answerSDP); err != nil {
		fmt.Fprintf(os.Stderr, "處理回應失敗: %v\n", err)
		os.Exit(1)
	}

	// 等待連線建立
	select {
	case <-connCh:
	case <-time.After(30 * time.Second):
		fmt.Fprintln(os.Stderr, "連線逾時")
		os.Exit(1)
	case <-ctx.Done():
		return
	}

	fmt.Fprintln(os.Stderr, "P2P 連線已建立")

	// 建立 per-device proxy 管理器
	adbAddr := fmt.Sprintf("127.0.0.1:%d", *adbPort)
	onReady, onRemoved := cliDeviceProxyCallbacks(adbAddr)
	dpm := bridge.NewDeviceProxyManager(bridge.DeviceProxyConfig{
		PortStart: *portStart,
		OpenCh:    pm.OpenChannel,
		ADBAddr:   adbAddr,
		OnReady:   onReady,
		OnRemoved: onRemoved,
	})
	defer dpm.Close()

	fmt.Fprintln(os.Stderr, "按 Ctrl+C 結束")

	// 啟動 control channel 讀取（驅動 DeviceProxyManager 的設備增減）
	go func() {
		bridge.ControlReadLoop(ctx, controlCh, func(cm bridge.CtrlMessage) {
			switch cm.Type {
			case "hello":
				fmt.Fprintf(os.Stderr, "遠端主機: %s\n", cm.Hostname)
			case "devices":
				dpm.UpdateDevices(cm.Devices)
			}
		})
	}()

	<-ctx.Done()
}

// cmdAgentPair 處理一次性 P2P 被控端連線（radb p2p agent）。
// 接收主控端的邀請碼（compact SDP），建立 Answer 並等待 P2P 連線。
// 連線建立後透過 control channel 推送設備清單，並由 ServerHandler
// 處理所有 DataChannel（adb-server/adb-stream/adb-fwd）。
func cmdAgentPair(args []string) {
	fs := flag.NewFlagSet("p2p agent", flag.ExitOnError)
	adbPort := fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server 埠")
	stunURLs := fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN Server URL")
	turnMode := fs.String("turn-mode", envStr("RADB_TURN_MODE", "cloudflare"), "TURN 模式 (cloudflare/custom/none)")
	turnURL := fs.String("turn", envStr("RADB_TURN_URL", ""), "TURN Server URL（turn-mode=custom 時使用）")
	turnUser := fs.String("turn-user", envStr("RADB_TURN_USER", ""), "TURN 使用者名稱")
	turnPass := fs.String("turn-pass", envStr("RADB_TURN_PASS", ""), "TURN 密碼")
	fs.Parse(args)

	// 支援兩種用法：直接帶邀請碼（radb p2p agent <token>）或互動輸入
	var offerToken string
	if fs.NArg() >= 1 {
		offerToken = fs.Arg(0)
	} else {
		fmt.Fprintln(os.Stderr, "請輸入邀請碼:")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				offerToken = line
				break
			}
		}
		if offerToken == "" {
			fmt.Fprintln(os.Stderr, "未輸入邀請碼")
			os.Exit(1)
		}
	}

	// 解碼 compact SDP offer
	offerCompact, err := bridge.DecodeToken(strings.TrimSpace(offerToken))
	if err != nil {
		fmt.Fprintf(os.Stderr, "無效的邀請碼: %v\n", err)
		os.Exit(1)
	}

	offerSDP := bridge.CompactToSDP(offerCompact)

	// 建立 ICE config（支援 Cloudflare 免費 TURN）
	iceConfig := buildICEConfig(*stunURLs, *turnMode, *turnURL, *turnUser, *turnPass)

	// 建立 PeerConnection
	pm, err := webrtc.NewPeerManager(iceConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "建立 PeerConnection 失敗: %v\n", err)
		os.Exit(1)
	}
	defer pm.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 偵測 PeerConnection 斷線（主控端關閉時自動結束 agent）
	pm.OnDisconnect(func() {
		fmt.Fprintln(os.Stderr, "\nP2P 連線已中斷")
		cancel()
	})

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

// --- ICE 設定輔助函式 ---

// buildICEConfig 根據 CLI flag 建構 ICEConfig，支援 Cloudflare 免費 TURN。
//
// turnMode 對應：
//   - "cloudflare"（預設）：從 Cloudflare 公開端點取得免費 TURN 憑證
//   - "custom"：使用 --turn/--turn-user/--turn-pass 指定的自訂 TURN
//   - "none" 或 ""：不使用 TURN，僅 STUN
func buildICEConfig(stunURLs, turnMode, turnURL, turnUser, turnPass string) webrtc.ICEConfig {
	iceConfig := webrtc.ICEConfig{}
	if stunURLs != "" {
		iceConfig.STUNServers = strings.Split(stunURLs, ",")
	}

	switch turnMode {
	case "cloudflare":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		servers, err := webrtc.FetchCloudflareTURN(ctx, nil)
		if err != nil {
			slog.Warn("Cloudflare TURN 取得失敗，僅使用 STUN", "error", err)
		} else {
			iceConfig.TURNServers = servers
			slog.Info("已取得 Cloudflare TURN 憑證", "servers", len(servers))
		}
	case "custom":
		if turnURL != "" {
			iceConfig.TURNServers = []webrtc.TURNServer{
				{URL: turnURL, Username: turnUser, Credential: turnPass},
			}
		}
	}

	return iceConfig
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
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil
	}

	// slog 寫入 log 檔
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Go runtime panic 輸出寫入 log 檔
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		slog.Warn("SetCrashOutput 失敗", "error", err)
	}

	// 啟動標記：協助確認主控端是否真的在寫此檔案。
	slog.Info("GUI log initialized", "log_path", logPath, "pid", os.Getpid())
	_ = f.Sync()

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
