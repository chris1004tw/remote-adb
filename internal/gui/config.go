// config.go 實作 GUI 共用設定的持久化機制。
//
// 設定以 TOML 格式存放於使用者設定目錄（os.UserConfigDir()/radb/radb.toml），
// 包含各分頁共用的 ADB Port、Proxy Port、Direct Port、STUN Server。
// 啟動時自動載入，設定面板修改後即時儲存。
//
// 相關文件：.claude/CLAUDE.md「專案概述 — GUI」
package gui

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// AppConfig 儲存 GUI 各分頁共用的設定值。
// 各欄位對應 TOML 檔案中的 key，使用 snake_case 命名。
//
// 欄位說明：
//   - ADBPort：被控端的 ADB server port（預設 5037）
//   - ProxyPort：主控端 ADB proxy 的起始 port（預設 5555）
//   - DirectPort：區網直連被控端的 TCP 服務 port（預設 15555）
//   - STUNServer：WebRTC ICE 使用的 STUN/TURN 伺服器地址（預設 Google STUN）
type AppConfig struct {
	ADBPort    int    `toml:"adb_port"`
	ProxyPort  int    `toml:"proxy_port"`
	DirectPort int    `toml:"direct_port"`
	STUNServer string `toml:"stun_server"`
}

// DefaultConfig 回傳所有欄位皆為預設值的設定。
func DefaultConfig() *AppConfig {
	return &AppConfig{
		ADBPort:    5037,
		ProxyPort:  5555,
		DirectPort: 15555,
		STUNServer: "stun:stun.l.google.com:19302",
	}
}

// DefaultConfigPath 回傳設定檔的預設路徑。
// 路徑為 os.UserConfigDir()/radb/radb.toml。
// 若無法取得使用者設定目錄，回傳空字串。
func DefaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "radb", "radb.toml")
}

// LoadConfig 從指定路徑載入 TOML 設定檔。
// 若檔案不存在，回傳預設設定（不報錯）。
// 若檔案只包含部分欄位，未指定的欄位自動套用預設值。
func LoadConfig(path string) (*AppConfig, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SaveConfig 將設定寫入指定路徑的 TOML 檔案。
// 若父目錄不存在，會自動建立。
func SaveConfig(cfg *AppConfig, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	return enc.Encode(cfg)
}
