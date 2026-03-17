// setup.go 提供 ADB 環境的自動偵測、下載與啟動功能。
//
// 目的：讓使用者不需手動安裝 Android Platform Tools，radb 會自動處理。
// 下載的 ADB 會快取在 exe 同目錄的 platform-tools/ 下，避免重複下載。
// 不搜尋系統 PATH，確保使用 radb 自帶的 ADB，避免版本不一致問題。

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
	"sync"
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

const (
	defaultADBServerPort  = "5037"
	adbServerStopTimeout  = 5 * time.Second
	adbServerStartTimeout = 5 * time.Second
)

var (
	findADBBinaryFunc         = FindADBBinary
	downloadPlatformToolsFunc = downloadPlatformTools
	startADBServerFunc        = StartADBServer
	killADBServerFunc         = KillADBServer
	ensureADBMu               sync.Mutex
)

// exeDir 快取 exe 所在目錄路徑，避免重複呼叫 os.Executable()。
var exeDir = sync.OnceValue(func() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
})

// adbDataDir 回傳 exe 同目錄下的 platform-tools/ 路徑。
// 所有 ADB 工具集中在 exe 旁邊，實現自包含可攜部署。
func adbDataDir() (string, error) {
	return filepath.Join(exeDir(), "platform-tools"), nil
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

// FindADBBinary 在 exe 同目錄的 platform-tools/ 中尋找 adb 二進位檔。
// 不搜尋系統 PATH，確保使用 radb 自帶的 ADB。
// 找到時回傳完整路徑，找不到回傳空字串。
func FindADBBinary() string {
	dir, err := adbDataDir()
	if err != nil {
		return ""
	}
	localPath := filepath.Join(dir, adbBinaryName())
	if _, err := os.Stat(localPath); err == nil {
		return localPath
	}
	return ""
}

func adbServerPort(addr string) string {
	if addr == "" {
		return defaultADBServerPort
	}
	if !strings.Contains(addr, ":") {
		return addr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return defaultADBServerPort
	}
	return port
}

func adbServerCommandArgs(addr, subcommand string) []string {
	port := adbServerPort(addr)
	if port == "" || port == defaultADBServerPort {
		return []string{subcommand}
	}
	return []string{"-P", port, subcommand}
}

// StartADBServer 用指定的 adb 二進位啟動 adb server。
func StartADBServer(adbPath, addr string) error {
	cmd := exec.Command(adbPath, adbServerCommandArgs(addr, "start-server")...)
	configureCommandWindow(cmd)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start ADB server: %w", err)
	}
	return nil
}

// KillADBServer 用指定的 adb 二進位停止 adb server。
func KillADBServer(adbPath, addr string) error {
	cmd := exec.Command(adbPath, adbServerCommandArgs(addr, "kill-server")...)
	configureCommandWindow(cmd)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kill ADB server: %w", err)
	}
	return nil
}

func waitADBServerState(ctx context.Context, addr string, wantRunning bool, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()

	for {
		if isADBServerRunningFunc(addr) == wantRunning {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			state := "stop"
			if wantRunning {
				state = "start"
			}
			return fmt.Errorf("ADB server did not %s within %v", state, timeout)
		case <-ticker.C:
		}
	}
}

// EnsureADB 確保 radb 自帶的 ADB 可用。依序執行：
//  1. 尋找或下載 bundled platform-tools
//  2. 一律先停止目前的 ADB server
//  3. 用 bundled adb 重新啟動 server
//
// progressFn 回呼用於通知呼叫端目前狀態（可為 nil）。
func EnsureADB(ctx context.Context, addr string, progressFn func(string)) error {
	report := func(s string) {
		if progressFn != nil {
			progressFn(s)
		}
	}

	ensureADBMu.Lock()
	defer ensureADBMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	// 1. 尋找 bundled adb 二進位
	report("尋找 ADB...")
	adbPath := findADBBinaryFunc()

	// 2. 找不到 → 下載 platform-tools
	if adbPath == "" {
		dir, err := adbDataDir()
		if err != nil {
			return err
		}
		if err := downloadPlatformToolsFunc(ctx, dir, report); err != nil {
			return err
		}
		adbPath = findADBBinaryFunc()
		if adbPath == "" {
			adbPath = filepath.Join(dir, adbBinaryName())
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 3. 一律先停止目前的 server，清掉既有 transport / 殘留狀態。
	report("停止現有 ADB server...")
	wasRunning := isADBServerRunningFunc(addr)
	if err := killADBServerFunc(adbPath, addr); err != nil && isADBServerRunningFunc(addr) {
		return err
	}
	if wasRunning {
		if err := waitADBServerState(ctx, addr, false, adbServerStopTimeout); err != nil {
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 4. 用 bundled adb 啟動 server。
	report("啟動 ADB server...")
	if err := startADBServerFunc(adbPath, addr); err != nil {
		return err
	}

	if err := waitADBServerState(ctx, addr, true, adbServerStartTimeout); err != nil {
		return err
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
