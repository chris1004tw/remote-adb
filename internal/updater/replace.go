// replace.go 負責以平台專屬策略安全地替換正在運行的 binary 檔案。
//
// 平台差異：
//   - Unix（Linux/macOS）：使用 os.Rename 進行原子替換（atomic rename）。
//     Unix 的 rename(2) 系統呼叫是原子操作，即使目標檔案正在被執行，
//     也能安全替換（因為 Unix 使用 inode，舊 inode 在所有 fd 關閉後才回收）。
//   - Windows：Windows 鎖定正在運行的 .exe 檔案，無法直接覆蓋或刪除。
//     因此採用「先重命名舊檔為 .old → 再將新檔移入」的策略。
//     .old 檔案會在下次程式啟動時由 CleanupOldBinaries 清理。
package updater

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ReplaceBinary 將 newPath 的檔案替換到 targetPath。
// 根據 runtime.GOOS 自動選擇平台專屬的替換策略。
func ReplaceBinary(targetPath, newPath string) error {
	if runtime.GOOS == "windows" {
		return replaceWindows(targetPath, newPath)
	}
	return replaceUnix(targetPath, newPath)
}

// replaceUnix 使用 atomic rename 替換 binary。
// Unix 的 rename(2) 是原子操作：對其他 process 來說，targetPath 要麼是舊檔案、
// 要麼是新檔案，不會出現中間狀態（如檔案不存在或寫到一半的情況）。
// 即使 radb 正在運行中也能安全替換，因為 Unix 的 inode 機制保證舊檔案
// 在所有 file descriptor 關閉前不會被真正釋放。
func replaceUnix(targetPath, newPath string) error {
	// 確保新檔案有執行權限（0755 = rwxr-xr-x）
	if err := os.Chmod(newPath, 0755); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}
	// moveFile 先嘗試 os.Rename（原子操作），失敗時 fallback 到 copy+remove
	if err := moveFile(newPath, targetPath); err != nil {
		return fmt.Errorf("failed to replace %s: %w", targetPath, err)
	}
	return nil
}

// replaceWindows 採用「重命名→移入」兩步策略替換 binary。
//
// Windows 不允許覆蓋或刪除正在運行的 .exe 檔案，但允許對其重命名。
// 因此步驟為：
//  1. 移除上次殘留的 .old 備份（如果有）
//  2. 將目前正在運行的 binary 重命名為 .old（此時檔名改變但 process 不受影響）
//  3. 將新 binary 移入原始路徑
//  4. 若步驟 3 失敗，嘗試將 .old 回滾到原始路徑
//
// .old 檔案因為可能仍被 process 鎖定而無法立即刪除，
// 會留到下次程式啟動時由 CleanupOldBinaries 清理。
func replaceWindows(targetPath, newPath string) error {
	oldPath := targetPath + ".old"

	// 清除上次更新殘留的 .old 檔案（忽略錯誤，可能不存在或仍被鎖定）
	os.Remove(oldPath)

	// 將目前的 binary 重命名為 .old（Windows 允許重命名運行中的 exe）
	if _, err := os.Stat(targetPath); err == nil {
		if err := os.Rename(targetPath, oldPath); err != nil {
			return fmt.Errorf("failed to backup %s: %w", targetPath, err)
		}
	}

	// 將新 binary 移入目標路徑（moveFile 支援跨磁碟機）
	if err := moveFile(newPath, targetPath); err != nil {
		// 移入失敗，嘗試將 .old 回滾到原始路徑以恢復原狀
		os.Rename(oldPath, targetPath)
		return fmt.Errorf("failed to replace %s: %w", targetPath, err)
	}
	return nil
}

// moveFile 將 src 移動到 dst，先嘗試 os.Rename（同磁碟機/檔案系統時為原子操作），
// 若失敗（例如跨磁碟機）則 fallback 到 copy + remove。
//
// 背景：Windows 的 os.Rename 無法跨磁碟機移動檔案（錯誤訊息為
// "The system cannot move the file to a different disk drive."），
// 而暫存目錄（os.TempDir）與執行檔可能不在同一磁碟機上。
// Unix 的 rename(2) 同樣不支援跨檔案系統移動（回傳 EXDEV）。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// fallback: 以 copy + remove 實現跨磁碟機/檔案系統移動
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("failed to copy file: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("failed to close destination file: %w", err)
	}

	// 複製成功後才移除來源
	os.Remove(src)
	return nil
}

// CleanupOldBinaries 清理指定目錄下的 .old 備份檔案。
//
// 呼叫時機：應在程式啟動時呼叫（此時舊 process 已結束，.old 檔案不再被鎖定）。
// 典型用法：在 main() 初始化階段呼叫 CleanupOldBinaries(filepath.Dir(os.Executable()))。
//
// 此函式刻意忽略所有錯誤（ReadDir 失敗、Remove 失敗），
// 因為清理是 best-effort 操作，不應影響程式的正常啟動。
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
