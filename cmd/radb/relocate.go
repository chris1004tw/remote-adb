// relocate.go 提供 GUI 模式的自動搬遷功能。
//
// 當使用者雙擊 radb.exe 執行時，若 exe 不在名為 "radb" 的資料夾中，
// 會自動建立 radb/ 資料夾、將自身複製進去並重新啟動，
// 實現「自包含可攜資料夾」的目標。結構如下：
//
//	radb/
//	├── radb.exe          ← 執行檔本體
//	├── radb.toml         ← 設定檔
//	├── radb_logs/        ← 日誌檔案
//	└── platform-tools/   ← ADB 工具
//
// 僅在 GUI 模式（無引數）觸發，CLI 模式不受影響。
// 搬遷後透過 RADB_RELOCATE_CLEANUP 環境變數通知新 instance 刪除舊 exe。
//
// 相關文件：.claude/CLAUDE.md「專案概述」
package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// relocateEnvKey 是搬遷後用於通知新 instance 清理舊 exe 的環境變數名。
const relocateEnvKey = "RADB_RELOCATE_CLEANUP"

var (
	osExecutable = os.Executable
	evalSymlinks = filepath.EvalSymlinks
)

// needsRelocate 判斷指定的 exe 目錄是否需要搬遷。
// 如果目錄名稱已經是 "radb"（不分大小寫），則不需要搬遷。
func needsRelocate(exeDir string) bool {
	return !strings.EqualFold(filepath.Base(exeDir), "radb")
}

// cleanupRelocateSource 在啟動時檢查 RADB_RELOCATE_CLEANUP 環境變數，
// 若存在則嘗試刪除舊 exe 檔案。舊 process 可能尚未完全退出，
// 因此最多重試 5 次（每次間隔 200ms）。
func cleanupRelocateSource() {
	oldPath := os.Getenv(relocateEnvKey)
	if oldPath == "" {
		return
	}
	os.Unsetenv(relocateEnvKey)

	for i := 0; i < 5; i++ {
		if err := os.Remove(oldPath); err == nil {
			slog.Info("cleaned up old executable after relocation", "path", oldPath)
			return
		}
		if i < 4 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	slog.Warn("failed to clean up old executable after relocation", "path", oldPath)
}

// maybeRelocate 檢查 exe 是否需要搬遷到 radb/ 資料夾。
// 若需要搬遷，建立資料夾、複製 exe、啟動新 instance 並回傳 true（呼叫端應 exit）。
// 若不需要搬遷或搬遷失敗，回傳 false（繼續正常啟動）。
func maybeRelocate() bool {
	exePath, err := executablePath()
	if err != nil {
		return false
	}

	exeDir := filepath.Dir(exePath)
	exeName := filepath.Base(exePath)

	if !needsRelocate(exeDir) {
		return false
	}

	// 開發環境中（exe 同目錄有 go.mod）不搬遷
	if _, err := os.Stat(filepath.Join(exeDir, "go.mod")); err == nil {
		return false
	}

	// 建立 radb/ 資料夾
	newDir := filepath.Join(exeDir, "radb")
	if err := os.MkdirAll(newDir, 0755); err != nil {
		slog.Error("failed to create radb directory", "path", newDir, "error", err)
		return false
	}

	// 複製 exe 到新位置
	newPath := filepath.Join(newDir, exeName)
	if err := copyFile(exePath, newPath); err != nil {
		slog.Error("failed to copy executable to radb directory", "error", err)
		return false
	}

	// 啟動新 instance，透過環境變數通知清理舊 exe
	cmd := exec.Command(newPath)
	cmd.Env = append(os.Environ(), relocateEnvKey+"="+exePath)
	if err := cmd.Start(); err != nil {
		slog.Error("failed to start relocated executable", "error", err)
		os.Remove(newPath)
		return false
	}

	return true
}

// executablePath 回傳目前執行檔的可用路徑。
// Windows 上 filepath.EvalSymlinks 對正在執行的 .exe 偶發會回傳
// "Access is denied"，此時退回原始 os.Executable() 結果即可。
func executablePath() (string, error) {
	exePath, err := osExecutable()
	if err != nil {
		return "", err
	}
	resolved, err := evalSymlinks(exePath)
	if err == nil {
		return resolved, nil
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		slog.Debug("EvalSymlinks failed, using original executable path", "path", exePath, "error", err)
		return exePath, nil
	}
	return "", err
}

// copyFile 複製檔案從 src 到 dst，保留 0755 執行權限。
// 用於搬遷 exe 和跨磁碟機遷移時的檔案複製。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
