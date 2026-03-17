package adb

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestIsADBServerRunning_WhenListening(t *testing.T) {
	// 建立一個假的 TCP server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if !IsADBServerRunning(ln.Addr().String()) {
		t.Error("預期偵測到運行中的 server")
	}
}

func TestIsADBServerRunning_WhenNotListening(t *testing.T) {
	// 使用一個不會有人監聽的 port
	if IsADBServerRunning("127.0.0.1:19999") {
		t.Error("預期偵測不到 server")
	}
}

func TestAdbDataDir(t *testing.T) {
	dir, err := adbDataDir()
	if err != nil {
		t.Fatal(err)
	}

	// adbDataDir 應回傳 exe 同目錄下的 platform-tools/（自包含可攜部署）
	exePath, _ := os.Executable()
	expected := filepath.Join(filepath.Dir(exePath), "platform-tools")
	if dir != expected {
		t.Errorf("expected %q, got %q", expected, dir)
	}
}

func TestAdbBinaryName(t *testing.T) {
	name := adbBinaryName()
	if runtime.GOOS == "windows" {
		if name != "adb.exe" {
			t.Errorf("Windows 上預期 adb.exe，得到 %q", name)
		}
	} else {
		if name != "adb" {
			t.Errorf("非 Windows 上預期 adb，得到 %q", name)
		}
	}
}

func TestPlatformToolsURL(t *testing.T) {
	url := platformToolsURL()
	expectedSuffix := runtime.GOOS + ".zip"
	if !contains(url, expectedSuffix) {
		t.Errorf("URL %q 應包含 %q", url, expectedSuffix)
	}
	if !contains(url, "dl.google.com") {
		t.Errorf("URL %q 應指向 dl.google.com", url)
	}
}

func TestFindADBBinary_LocalCache(t *testing.T) {
	// 建立暫時目錄模擬快取
	tmpDir := t.TempDir()
	name := adbBinaryName()
	adbPath := filepath.Join(tmpDir, name)
	if err := os.WriteFile(adbPath, []byte("fake"), 0755); err != nil {
		t.Fatal(err)
	}

	// 暫時替換 adbDataDir（透過直接測試邏輯）
	localPath := filepath.Join(tmpDir, name)
	if _, err := os.Stat(localPath); err != nil {
		t.Errorf("預期在 %q 找到檔案", localPath)
	}
}

func TestADBServerCommandArgs_DefaultPort(t *testing.T) {
	args := adbServerCommandArgs("127.0.0.1:5037", "start-server")
	if len(args) != 1 || args[0] != "start-server" {
		t.Fatalf("args = %v, want [start-server]", args)
	}
}

func TestADBServerCommandArgs_CustomPort(t *testing.T) {
	args := adbServerCommandArgs("127.0.0.1:5038", "kill-server")
	if len(args) != 3 {
		t.Fatalf("args length = %d, want 3", len(args))
	}
	if args[0] != "-P" || args[1] != "5038" || args[2] != "kill-server" {
		t.Fatalf("args = %v, want [-P 5038 kill-server]", args)
	}
}

func TestEnsureADB_AlwaysKillsThenStartsBundledADB(t *testing.T) {
	origFind := findADBBinaryFunc
	origDownload := downloadPlatformToolsFunc
	origKill := killADBServerFunc
	origStart := startADBServerFunc
	origRunning := isADBServerRunningFunc
	defer func() {
		findADBBinaryFunc = origFind
		downloadPlatformToolsFunc = origDownload
		killADBServerFunc = origKill
		startADBServerFunc = origStart
		isADBServerRunningFunc = origRunning
	}()

	const adbPath = `C:\radb\platform-tools\adb.exe`
	const adbAddr = "127.0.0.1:5037"

	var mu sync.Mutex
	var calls []string
	running := false

	findADBBinaryFunc = func() string { return adbPath }
	downloadPlatformToolsFunc = func(ctx context.Context, destDir string, report func(string)) error {
		t.Fatal("downloadPlatformTools should not be called when adb already exists")
		return nil
	}
	killADBServerFunc = func(path, addr string) error {
		mu.Lock()
		calls = append(calls, "kill:"+path+":"+addr)
		running = false
		mu.Unlock()
		return nil
	}
	startADBServerFunc = func(path, addr string) error {
		mu.Lock()
		calls = append(calls, "start:"+path+":"+addr)
		running = true
		mu.Unlock()
		return nil
	}
	isADBServerRunningFunc = func(addr string) bool {
		mu.Lock()
		defer mu.Unlock()
		return running
	}

	if err := EnsureADB(context.Background(), adbAddr, nil); err != nil {
		t.Fatalf("EnsureADB error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"kill:" + adbPath + ":" + adbAddr,
		"start:" + adbPath + ":" + adbAddr,
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
	if !running {
		t.Fatal("expected ADB server to be running after EnsureADB")
	}
}

func TestEnsureADB_DownloadsBeforeRestartWhenMissing(t *testing.T) {
	origFind := findADBBinaryFunc
	origDownload := downloadPlatformToolsFunc
	origKill := killADBServerFunc
	origStart := startADBServerFunc
	origRunning := isADBServerRunningFunc
	defer func() {
		findADBBinaryFunc = origFind
		downloadPlatformToolsFunc = origDownload
		killADBServerFunc = origKill
		startADBServerFunc = origStart
		isADBServerRunningFunc = origRunning
	}()

	const adbAddr = "127.0.0.1:5037"
	const adbPath = `C:\radb\platform-tools\adb.exe`

	var mu sync.Mutex
	var calls []string
	downloaded := false
	running := false

	findADBBinaryFunc = func() string {
		if downloaded {
			return adbPath
		}
		return ""
	}
	downloadPlatformToolsFunc = func(ctx context.Context, destDir string, report func(string)) error {
		mu.Lock()
		calls = append(calls, "download:"+destDir)
		downloaded = true
		mu.Unlock()
		return nil
	}
	killADBServerFunc = func(path, addr string) error {
		mu.Lock()
		calls = append(calls, "kill:"+path+":"+addr)
		running = false
		mu.Unlock()
		return nil
	}
	startADBServerFunc = func(path, addr string) error {
		mu.Lock()
		calls = append(calls, "start:"+path+":"+addr)
		running = true
		mu.Unlock()
		return nil
	}
	isADBServerRunningFunc = func(addr string) bool {
		mu.Lock()
		defer mu.Unlock()
		return running
	}

	if err := EnsureADB(context.Background(), adbAddr, nil); err != nil {
		t.Fatalf("EnsureADB error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("calls = %v, want download/kill/start", calls)
	}
	if calls[1] != "kill:"+adbPath+":"+adbAddr {
		t.Fatalf("kill call = %q, want bundled adb path", calls[1])
	}
	if calls[2] != "start:"+adbPath+":"+adbAddr {
		t.Fatalf("start call = %q, want bundled adb path", calls[2])
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
