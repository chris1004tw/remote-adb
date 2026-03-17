// migrate.go 負責將舊版路徑的資料遷移至 exe 所在目錄。
//
// radb 從自包含可攜架構開始，將所有資料集中到 exe 同目錄，
// 此模組負責一次性遷移舊路徑的 platform-tools 和設定檔：
//
//   - ~/.radb/platform-tools/ → <exe-dir>/platform-tools/
//   - os.UserConfigDir()/radb/radb.toml → <exe-dir>/radb.toml
//
// 遷移為冪等操作：舊路徑不存在即跳過，新路徑已存在則刪除舊路徑。
// 遷移完成後清理空的舊目錄。遷移失敗不中斷啟動（僅 log 警告）。
//
// 相關文件：.claude/CLAUDE.md「專案概述」
package main

import (
	"log/slog"
	"os"
	"path/filepath"
)

// migrateOldData 將舊版路徑的資料遷移至 exe 所在目錄。
// GUI/CLI 啟動時皆會呼叫，冪等操作——舊路徑不在即跳過。
func migrateOldData() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	exeDir := filepath.Dir(exePath)

	// 遷移 platform-tools (~/.radb/platform-tools/ → <exe-dir>/platform-tools/)
	if home, err := os.UserHomeDir(); err == nil {
		oldPT := filepath.Join(home, ".radb", "platform-tools")
		newPT := filepath.Join(exeDir, "platform-tools")
		migrateDir(oldPT, newPT)
		removeEmptyDir(filepath.Join(home, ".radb"))
	}

	// 遷移 config (os.UserConfigDir()/radb/radb.toml → <exe-dir>/radb.toml)
	if configDir, err := os.UserConfigDir(); err == nil {
		oldConfig := filepath.Join(configDir, "radb", "radb.toml")
		newConfig := filepath.Join(exeDir, "radb.toml")
		migrateFile(oldConfig, newConfig)
		removeEmptyDir(filepath.Join(configDir, "radb"))
	}
}

// migrateFile 將單一檔案從 oldPath 遷移到 newPath。
// 若 oldPath 不存在，靜默返回。若 newPath 已存在，刪除 oldPath（保留新檔）。
// 搬移失敗時嘗試 copy + delete（處理跨磁碟機情況）。
func migrateFile(oldPath, newPath string) {
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		return
	}

	if _, err := os.Stat(newPath); err == nil {
		// 新位置已存在，刪除舊檔
		os.Remove(oldPath)
		slog.Info("removed old config (new location already exists)", "old", oldPath)
		return
	}

	// 建立目標目錄
	if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
		slog.Warn("failed to create directory for migration", "path", filepath.Dir(newPath), "error", err)
		return
	}

	// 搬移檔案（os.Rename 跨磁碟機會失敗，改用 copy + delete）
	if err := os.Rename(oldPath, newPath); err != nil {
		if err := copyFile(oldPath, newPath); err != nil {
			slog.Warn("failed to migrate file", "old", oldPath, "new", newPath, "error", err)
			return
		}
		os.Remove(oldPath)
	}
	slog.Info("migrated file", "old", oldPath, "new", newPath)
}

// migrateDir 將目錄從 oldPath 遷移到 newPath。
// 若 oldPath 不存在，靜默返回。若 newPath 已存在，刪除 oldPath（保留新目錄）。
func migrateDir(oldPath, newPath string) {
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		return
	}

	if _, err := os.Stat(newPath); err == nil {
		// 新位置已存在，刪除舊目錄
		os.RemoveAll(oldPath)
		slog.Info("removed old directory (new location already exists)", "old", oldPath)
		return
	}

	// 建立目標父目錄
	if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
		slog.Warn("failed to create parent directory for migration", "path", filepath.Dir(newPath), "error", err)
		return
	}

	if err := os.Rename(oldPath, newPath); err != nil {
		// os.Rename 跨磁碟機會失敗，記錄警告讓使用者手動處理
		slog.Warn("failed to migrate directory (cross-device move not supported)", "old", oldPath, "new", newPath, "error", err)
		return
	}
	slog.Info("migrated directory", "old", oldPath, "new", newPath)
}

// removeEmptyDir 若目錄為空則刪除。非空、不存在、或刪除失敗時靜默忽略。
func removeEmptyDir(path string) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}
	if len(entries) == 0 {
		os.Remove(path)
	}
}
