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
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"

	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/gui"
	"github.com/chris1004tw/remote-adb/internal/updater"
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
		cmdDirect(os.Args[2:])
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

// cmdDirect 分派 Direct 模式子命令：connect（TCP 直連）、discover（mDNS 掃描）、agent（被控端）。
// Direct 模式在同一區網內透過 TCP 直連，不需要任何伺服器。
func cmdDirect(args []string) {
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
		slog.Warn("SetCrashOutput failed", "error", err)
	}

	// 啟動標記：協助確認主控端是否真的在寫此檔案。
	slog.Info("GUI log initialized", "log_path", logPath, "pid", os.Getpid())
	_ = f.Sync()

	// 讓 fmt.Fprintf(os.Stderr, ...) 也寫入 log 檔
	os.Stderr = f

	return f
}
