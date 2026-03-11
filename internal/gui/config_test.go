package gui

import (
	"context"
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
		TURNMode:   TURNModeCustom,
		TURNServer: "turn:relay.example.com:3478",
		TURNUser:   "myuser",
		TURNPass:   "mypass",
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
	if loaded.TURNMode != TURNModeCustom {
		t.Errorf("loaded TURNMode = %q, want %q", loaded.TURNMode, TURNModeCustom)
	}
	if loaded.TURNServer != "turn:relay.example.com:3478" {
		t.Errorf("loaded TURNServer = %q, want turn:relay.example.com:3478", loaded.TURNServer)
	}
	if loaded.TURNUser != "myuser" {
		t.Errorf("loaded TURNUser = %q, want myuser", loaded.TURNUser)
	}
	if loaded.TURNPass != "mypass" {
		t.Errorf("loaded TURNPass = %q, want mypass", loaded.TURNPass)
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

func TestDefaultConfig_TURNModeCloudflare(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.TURNMode != TURNModeCloudflare {
		t.Errorf("TURNMode = %q, want %q (Cloudflare by default)", cfg.TURNMode, TURNModeCloudflare)
	}
	if cfg.TURNServer != "" {
		t.Errorf("TURNServer = %q, want empty (Cloudflare mode uses API)", cfg.TURNServer)
	}
	if cfg.TURNUser != "" {
		t.Errorf("TURNUser = %q, want empty", cfg.TURNUser)
	}
	if cfg.TURNPass != "" {
		t.Errorf("TURNPass = %q, want empty", cfg.TURNPass)
	}
}

func TestParseICEConfig_STUNOnly(t *testing.T) {
	cfg := &AppConfig{STUNServer: "stun:stun.l.google.com:19302"}
	ice := parseICEConfig(cfg)
	if len(ice.STUNServers) != 1 || ice.STUNServers[0] != "stun:stun.l.google.com:19302" {
		t.Errorf("STUNServers = %v, want [stun:stun.l.google.com:19302]", ice.STUNServers)
	}
	if len(ice.TURNServers) != 0 {
		t.Errorf("TURNServers = %v, want empty", ice.TURNServers)
	}
}

func TestParseICEConfig_WithTURN(t *testing.T) {
	cfg := &AppConfig{
		STUNServer: "stun:stun.l.google.com:19302",
		TURNMode:   TURNModeCustom,
		TURNServer: "turn:relay.example.com:3478",
		TURNUser:   "user1",
		TURNPass:   "pass1",
	}
	ice := parseICEConfig(cfg)
	if len(ice.STUNServers) != 1 {
		t.Fatalf("STUNServers length = %d, want 1", len(ice.STUNServers))
	}
	if len(ice.TURNServers) != 1 {
		t.Fatalf("TURNServers length = %d, want 1", len(ice.TURNServers))
	}
	turn := ice.TURNServers[0]
	if turn.URL != "turn:relay.example.com:3478" {
		t.Errorf("TURN URL = %q, want turn:relay.example.com:3478", turn.URL)
	}
	if turn.Username != "user1" {
		t.Errorf("TURN Username = %q, want user1", turn.Username)
	}
	if turn.Credential != "pass1" {
		t.Errorf("TURN Credential = %q, want pass1", turn.Credential)
	}
}

func TestParseICEConfig_EmptyTURN(t *testing.T) {
	cfg := &AppConfig{STUNServer: "stun:stun.l.google.com:19302", TURNServer: ""}
	ice := parseICEConfig(cfg)
	if len(ice.TURNServers) != 0 {
		t.Errorf("TURNServers should be empty when TURNServer is blank, got %v", ice.TURNServers)
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

// TestResolveICEConfig_Custom 驗證自訂模式使用 AppConfig 中的 TURN 設定。
func TestResolveICEConfig_Custom(t *testing.T) {
	cfg := &AppConfig{
		STUNServer: "stun:stun.l.google.com:19302",
		TURNMode:   TURNModeCustom,
		TURNServer: "turn:my.turn.com:3478",
		TURNUser:   "myuser",
		TURNPass:   "mypass",
	}
	ice, err := resolveICEConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("resolveICEConfig failed: %v", err)
	}
	if len(ice.TURNServers) != 1 {
		t.Fatalf("TURNServers = %d, want 1", len(ice.TURNServers))
	}
	if ice.TURNServers[0].URL != "turn:my.turn.com:3478" {
		t.Errorf("TURN URL = %q, want turn:my.turn.com:3478", ice.TURNServers[0].URL)
	}
}

// TestResolveICEConfig_NoTURN 驗證空模式不啟用 TURN。
func TestResolveICEConfig_NoTURN(t *testing.T) {
	cfg := &AppConfig{
		STUNServer: "stun:stun.l.google.com:19302",
		TURNMode:   "",
	}
	ice, err := resolveICEConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("resolveICEConfig failed: %v", err)
	}
	if len(ice.TURNServers) != 0 {
		t.Errorf("TURNServers = %v, want empty", ice.TURNServers)
	}
}
