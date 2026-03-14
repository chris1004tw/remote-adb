// setup.go 提供 ADB 環境的自動偵測、下載與啟動功能。
//
// 目的：讓使用者不需手動安裝 Android Platform Tools，radb 會自動處理。
// 下載的 ADB 會快取在 ~/.radb/platform-tools/ 目錄下，避免重複下載。
// 此功能主要供 GUI 模式使用（CLI 使用者通常已有 ADB 環境）。

package adb

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// adbBinaryName 回傳當前平台的 adb 二進位檔名。
func adbBinaryName() string {
	if runtime.GOOS == "windows" {
		return "adb.exe"
	}
	return "adb"
}

// platformToolsURL 回傳當前平台的 Google platform-tools 下載 URL。
func platformToolsURL() string {
	return fmt.Sprintf(
		"https://dl.google.com/android/repository/platform-tools-latest-%s.zip",
		runtime.GOOS,
	)
}

// adbDataDir 回傳 ~/.radb/platform-tools/ 路徑。
func adbDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	return filepath.Join(home, ".radb", "platform-tools"), nil
}

// IsADBServerRunning 嘗試連線 ADB server 確認是否運行中。
func IsADBServerRunning(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// FindADBBinary 在 PATH 和本地快取目錄中尋找 adb 二進位檔。
// 找到時回傳完整路徑，找不到回傳空字串。
func FindADBBinary() string {
	name := adbBinaryName()

	// 先查 PATH
	if p, err := exec.LookPath(name); err == nil {
		return p
	}

	// 再查本地快取目錄
	dir, err := adbDataDir()
	if err != nil {
		return ""
	}
	localPath := filepath.Join(dir, name)
	if _, err := os.Stat(localPath); err == nil {
		return localPath
	}

	return ""
}

// StartADBServer 用指定的 adb 二進位啟動 adb server。
func StartADBServer(adbPath string) error {
	cmd := exec.Command(adbPath, "start-server")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start ADB server: %w", err)
	}
	return nil
}

// EnsureADB 確保 ADB 可用。依序嘗試：
//  1. 檢查 ADB server 是否已在運行
//  2. 在 PATH 或本地快取中尋找 adb 二進位
//  3. 從 Google 下載 platform-tools
//  4. 啟動 adb server
//
// progressFn 回呼用於通知呼叫端目前狀態（可為 nil）。
func EnsureADB(ctx context.Context, addr string, progressFn func(string)) error {
	report := func(s string) {
		if progressFn != nil {
			progressFn(s)
		}
	}

	// 1. ADB server 已在運行
	if IsADBServerRunning(addr) {
		return nil
	}

	// 2. 尋找 adb 二進位
	report("尋找 ADB...")
	adbPath := FindADBBinary()

	// 3. 找不到 → 下載
	if adbPath == "" {
		dir, err := adbDataDir()
		if err != nil {
			return err
		}
		if err := downloadPlatformTools(ctx, dir, report); err != nil {
			return err
		}
		adbPath = filepath.Join(dir, adbBinaryName())
	}

	// 4. 啟動 adb server
	report("啟動 ADB server...")
	if err := StartADBServer(adbPath); err != nil {
		return err
	}

	// 確認啟動成功
	time.Sleep(500 * time.Millisecond)
	if !IsADBServerRunning(addr) {
		return fmt.Errorf("ADB server started but not reachable")
	}

	return nil
}

// downloadPlatformTools 從 Google 下載並解壓 platform-tools 到 destDir。
func downloadPlatformTools(ctx context.Context, destDir string, report func(string)) error {
	url := platformToolsURL()
	report("正在下載 ADB 工具...")

	// 建立暫存檔
	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	zipPath := destDir + ".zip"

	// 下載
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(zipPath)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(zipPath)
		return fmt.Errorf("download write failed: %w", err)
	}
	f.Close()

	// 解壓
	report("正在解壓 ADB 工具...")
	if err := extractADBFromZip(zipPath, destDir); err != nil {
		os.Remove(zipPath)
		return err
	}

	// 清理 zip
	os.Remove(zipPath)

	return nil
}

// extractADBFromZip 從 zip 中只解壓需要的 ADB 檔案。
func extractADBFromZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// 只解壓 adb 執行所需的檔案（白名單），忽略其他如 fastboot 等工具。
	// Windows 需要額外的 DLL 才能運行 adb。
	needFiles := map[string]bool{
		"adb":              true,
		"adb.exe":          true,
		"AdbWinApi.dll":    true,
		"AdbWinUsbApi.dll": true,
	}

	for _, f := range r.File {
		// zip 內路徑格式：platform-tools/adb
		base := filepath.Base(f.Name)
		if !needFiles[base] {
			continue
		}
		// 防止 zip slip 攻擊：惡意 zip 可能包含 "../" 路徑，
		// 嘗試將檔案解壓到目標目錄之外（如覆蓋系統檔案）。
		if strings.Contains(f.Name, "..") {
			continue
		}

		destPath := filepath.Join(destDir, base)
		if err := extractZipFile(f, destPath); err != nil {
			return err
		}
	}

	return nil
}

// extractZipFile 解壓單一 zip 檔案條目。
func extractZipFile(f *zip.File, destPath string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open zip entry: %w", err)
	}
	defer rc.Close()

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, rc); err != nil {
		return fmt.Errorf("extract write failed: %w", err)
	}

	return nil
}
