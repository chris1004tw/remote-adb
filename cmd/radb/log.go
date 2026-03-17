// log.go 提供統一的日誌初始化功能。
//
// 日誌檔案存放於執行檔同目錄的 radb_logs/ 資料夾，
// 按版本與日期切分：radb_logs/v{version}-{date}.log。
// 啟動時自動清理超過 30 天的舊 log 檔。
//
// CLI 模式：同時輸出到 console（os.Stderr）與 log 檔（tee 模式）。
// GUI 模式：僅寫入 log 檔，並將 os.Stderr 重導至 log 檔。
package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/chris1004tw/remote-adb/internal/buildinfo"
)

// logMaxDays 是 log 檔案的最大保留天數。超過此天數的 .log 檔案會在啟動時被自動刪除。
const logMaxDays = 30

// setupLog 初始化日誌系統。
//
// 建立 radb_logs/ 資料夾（若不存在），產生 v{version}-{date}.log 檔案，
// 並設定 slog default handler。tee=true 時同時輸出到 console（CLI 模式），
// tee=false 時僅寫檔案且重導 os.Stderr（GUI 模式）。
// 回傳 log 檔案供呼叫端 defer Close()，失敗時回傳 nil。
func setupLog(tee bool) *os.File {
	exePath, err := os.Executable()
	if err != nil {
		return nil
	}

	logDir := filepath.Join(filepath.Dir(exePath), "radb_logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil
	}

	// 啟動時清理過期 log 檔
	cleanOldLogs(logDir, logMaxDays)

	logPath := filepath.Join(logDir, logFileName(buildinfo.Version, time.Now()))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil
	}

	// CLI 模式：console + 檔案雙寫；GUI 模式：僅寫檔案
	var w io.Writer = f
	if tee {
		w = io.MultiWriter(os.Stderr, f)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Go runtime panic 輸出寫入 log 檔
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		slog.Warn("SetCrashOutput failed", "error", err)
	}

	slog.Info("log initialized", "log_path", logPath, "pid", os.Getpid())
	_ = f.Sync()

	// GUI 模式：讓 fmt.Fprintf(os.Stderr, ...) 也寫入 log 檔
	if !tee {
		os.Stderr = f
	}

	return f
}

// logFileName 產生 log 檔名，格式為 v{version}-{date}.log。
// 自動處理版本字串的 "v" 前綴，避免 "vv1.2.3" 的情況。
func logFileName(version string, t time.Time) string {
	v := strings.TrimPrefix(version, "v")
	return fmt.Sprintf("v%s-%s.log", v, t.Format("2006-01-02"))
}

// cleanOldLogs 刪除 logDir 中修改時間超過 maxDays 天的 .log 檔案。
// 僅刪除副檔名為 .log 的檔案，不影響其他檔案或子目錄。
// 對不存在的目錄或讀取失敗的情況靜默忽略（啟動不應因清理失敗而中斷）。
func cleanOldLogs(logDir string, maxDays int) {
	cutoff := time.Now().AddDate(0, 0, -maxDays)
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".log" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(logDir, e.Name()))
		}
	}
}
