package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/cli"
	"github.com/chris1004tw/remote-adb/internal/daemon"
	"github.com/chris1004tw/remote-adb/internal/updater"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

func main() {
	// 清理上次更新留下的 .old 備份檔案
	if selfPath, err := os.Executable(); err == nil {
		updater.CleanupOldBinaries(filepath.Dir(selfPath))
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
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
	fmt.Fprintf(os.Stderr, "子命令:\n")
	fmt.Fprintf(os.Stderr, "  daemon    啟動背景服務\n")
	fmt.Fprintf(os.Stderr, "  bind      綁定遠端設備\n")
	fmt.Fprintf(os.Stderr, "  unbind    解除綁定\n")
	fmt.Fprintf(os.Stderr, "  list      列出已綁定設備\n")
	fmt.Fprintf(os.Stderr, "  status    查詢 daemon 狀態\n")
	fmt.Fprintf(os.Stderr, "  hosts     列出可用主機\n")
	fmt.Fprintf(os.Stderr, "  update    檢查並更新到最新版本\n")
	fmt.Fprintf(os.Stderr, "  version   顯示版本\n")
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
	signalURL := fs.String("signal", envStr("RADB_SIGNAL_URL", "ws://localhost:8080"), "Signal Server 位址")
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
		SignalURL: *signalURL,
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

	fmt.Printf("Signal Server: %s\n", status.SignalURL)
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

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
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
