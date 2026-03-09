package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ReplaceBinary 將 newPath 的檔案替換到 targetPath。
// 在 Windows 上，會先將 targetPath rename 為 .old，再將 newPath 移入。
// 在 Unix 上，直接用 rename（atomic）。
func ReplaceBinary(targetPath, newPath string) error {
	if runtime.GOOS == "windows" {
		return replaceWindows(targetPath, newPath)
	}
	return replaceUnix(targetPath, newPath)
}

func replaceUnix(targetPath, newPath string) error {
	// 確保新檔案有執行權限
	if err := os.Chmod(newPath, 0755); err != nil {
		return fmt.Errorf("設定權限失敗: %w", err)
	}
	// os.Rename 在 Unix 上是 atomic 操作
	if err := os.Rename(newPath, targetPath); err != nil {
		return fmt.Errorf("替換 %s 失敗: %w", targetPath, err)
	}
	return nil
}

func replaceWindows(targetPath, newPath string) error {
	oldPath := targetPath + ".old"

	// 先移除之前的 .old（如果存在）
	os.Remove(oldPath)

	// 將目前的 binary rename 為 .old
	if _, err := os.Stat(targetPath); err == nil {
		if err := os.Rename(targetPath, oldPath); err != nil {
			return fmt.Errorf("備份 %s 失敗: %w", targetPath, err)
		}
	}

	// 將新 binary 移入
	if err := os.Rename(newPath, targetPath); err != nil {
		// 嘗試回滾
		os.Rename(oldPath, targetPath)
		return fmt.Errorf("替換 %s 失敗: %w", targetPath, err)
	}
	return nil
}

// CleanupOldBinaries 清理指定目錄下的 .old 備份檔案。
func CleanupOldBinaries(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".old") {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
