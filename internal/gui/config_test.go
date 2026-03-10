package gui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.ADBPort != 5037 {
		t.Errorf("ADBPort = %d, want 5037", cfg.ADBPort)
	}
	if cfg.ProxyPort != 5555 {
		t.Errorf("ProxyPort = %d, want 5555", cfg.ProxyPort)
	}
	if cfg.DirectPort != 15555 {
		t.Errorf("DirectPort = %d, want 15555", cfg.DirectPort)
	}
	if cfg.STUNServer != "stun:stun.l.google.com:19302" {
		t.Errorf("STUNServer = %q, want default STUN", cfg.STUNServer)
	}
}

func TestSaveAndLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "radb.toml")

	// 儲存自訂設定
	cfg := &AppConfig{
		ADBPort:    5038,
		ProxyPort:  6666,
		DirectPort: 20000,
		STUNServer: "stun:custom.example.com:3478",
	}
	if err := SaveConfig(cfg, path); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// 確認檔案已建立
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not created: %v", err)
	}

	// 重新載入並驗證
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if loaded.ADBPort != 5038 {
		t.Errorf("loaded ADBPort = %d, want 5038", loaded.ADBPort)
	}
	if loaded.ProxyPort != 6666 {
		t.Errorf("loaded ProxyPort = %d, want 6666", loaded.ProxyPort)
	}
	if loaded.DirectPort != 20000 {
		t.Errorf("loaded DirectPort = %d, want 20000", loaded.DirectPort)
	}
	if loaded.STUNServer != "stun:custom.example.com:3478" {
		t.Errorf("loaded STUNServer = %q, want custom", loaded.STUNServer)
	}
}

func TestLoadConfig_FileNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.toml")

	// 檔案不存在時應回傳預設值，不報錯
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig should not fail for missing file: %v", err)
	}
	if cfg.ADBPort != 5037 {
		t.Errorf("should return default ADBPort, got %d", cfg.ADBPort)
	}
}

func TestLoadConfig_PartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "radb.toml")

	// 只寫入部分欄位，其餘應用預設值
	partial := []byte("adb_port = 9999\n")
	if err := os.WriteFile(path, partial, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.ADBPort != 9999 {
		t.Errorf("ADBPort = %d, want 9999", cfg.ADBPort)
	}
	// 未指定的欄位應為預設值
	if cfg.ProxyPort != 5555 {
		t.Errorf("ProxyPort = %d, want default 5555", cfg.ProxyPort)
	}
	if cfg.STUNServer != "stun:stun.l.google.com:19302" {
		t.Errorf("STUNServer = %q, want default", cfg.STUNServer)
	}
}

func TestSaveConfig_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub", "dir", "radb.toml")

	cfg := DefaultConfig()
	if err := SaveConfig(cfg, nested); err != nil {
		t.Fatalf("SaveConfig should create parent dirs: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("file not created at nested path: %v", err)
	}
}
