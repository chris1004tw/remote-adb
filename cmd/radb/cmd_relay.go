// cmd_relay.go — Relay 模式子命令（server / agent / daemon / bind / unbind / list / status / hosts）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea" // TUI 互動式選單框架

	"github.com/chris1004tw/remote-adb/internal/agent"
	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/cli"
	"github.com/chris1004tw/remote-adb/internal/daemon"
	signalpkg "github.com/chris1004tw/remote-adb/internal/signal" // 別名避免與 os/signal 衝突

	"log/slog"
)

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

	slog.Info("starting radb server", "version", buildinfo.Version, "host", *host, "port", *port)

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
		slog.Info("server listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("received shutdown signal, gracefully shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown failed", "error", err)
	}
	slog.Info("server stopped")
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
	ice := addICEFlags(fs)
	fs.Parse(args)

	if *token == "" {
		fmt.Fprintln(os.Stderr, "錯誤：必須設定 RADB_TOKEN 環境變數或使用 --token flag")
		os.Exit(1)
	}

	iceConfig := ice.build()

	slog.Info("starting radb relay agent", "version", buildinfo.Version, "host_id", *hostID)

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
		slog.Error("agent run failed", "error", err)
		os.Exit(1)
	}
	slog.Info("agent stopped")
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
	ice := addICEFlags(fs)
	fs.Parse(args)

	if *token == "" {
		fmt.Fprintln(os.Stderr, "錯誤：必須設定 RADB_TOKEN 環境變數或使用 --token flag")
		os.Exit(1)
	}

	iceConfig := ice.build()

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
		m := cli.NewModel(sendIPCCommand)
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
// 連線建立由 IPCDial 負責，命令收發委派給 daemon.SendCommand（共用邏輯）。
func sendIPCCommand(cmd daemon.IPCCommand) daemon.IPCResponse {
	conn, err := daemon.IPCDial()
	if err != nil {
		fmt.Fprintf(os.Stderr, "無法連線到 daemon: %v\n", err)
		fmt.Fprintln(os.Stderr, "請確認 daemon 是否已啟動 (radb relay daemon)")
		os.Exit(1)
	}
	defer conn.Close()

	resp, err := daemon.SendCommand(conn, cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	return resp
}
