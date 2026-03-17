// helpers.go — 共用工具函式（ICE flag、環境變數讀取、DPM callback 等）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

// --- ICE 設定輔助函式 ---

// buildICEConfig 根據 CLI flag 建構 ICEConfig，支援 Cloudflare 免費 TURN。
//
// turnMode 對應：
//   - "cloudflare"（預設）：從 Cloudflare 公開端點取得免費 TURN 憑證
// iceFlags 封裝所有 ICE 相關的 CLI flag，避免 4 個子命令各自重複定義。
type iceFlags struct {
	connMode *string
	stunURLs *string
	turnMode *string
	turnURL  *string
	turnUser *string
	turnPass *string
}

// addICEFlags 向 FlagSet 註冊連線方式與 STUN/TURN 相關 flag 並回傳 iceFlags。
// 所有 flag 的預設值可透過環境變數覆蓋（RADB_CONN_MODE、RADB_STUN_URLS、RADB_TURN_MODE 等）。
func addICEFlags(fs *flag.FlagSet) *iceFlags {
	return &iceFlags{
		connMode: fs.String("conn-mode", envStr("RADB_CONN_MODE", "direct-first"), "連線方式 (direct-first/direct-only/relay-only)"),
		stunURLs: fs.String("stun", envStr("RADB_STUN_URLS", "stun:stun.l.google.com:19302"), "STUN Server URL"),
		turnMode: fs.String("turn-mode", envStr("RADB_TURN_MODE", "cloudflare"), "TURN 模式 (cloudflare/custom)"),
		turnURL:  fs.String("turn", envStr("RADB_TURN_URL", ""), "TURN Server URL（turn-mode=custom 時使用）"),
		turnUser: fs.String("turn-user", envStr("RADB_TURN_USER", ""), "TURN 使用者名稱"),
		turnPass: fs.String("turn-pass", envStr("RADB_TURN_PASS", ""), "TURN 密碼"),
	}
}

// build 根據 flag 值建構 ICE 設定（呼叫 buildICEConfig）。
func (f *iceFlags) build() webrtc.ICEConfig {
	return buildICEConfig(*f.connMode, *f.stunURLs, *f.turnMode, *f.turnURL, *f.turnUser, *f.turnPass)
}

// buildICEConfig 根據連線方式與 STUN/TURN 參數建構 ICE 設定。
//
// connMode 控制 ICE candidate 收集策略：
//   - "direct-first"（預設）：收集所有 candidate，優先嘗試直連
//   - "direct-only"：僅使用 STUN，不啟用 TURN 中繼
//   - "relay-only"：僅走 TURN 中繼（ICETransportPolicy=relay）
//
// turnMode 控制 TURN 伺服器來源（connMode 非 direct-only 時生效）：
//   - "cloudflare"：自動取得 Cloudflare 免費 TURN 憑證（預設）
//   - "custom"：使用 --turn/--turn-user/--turn-pass 指定的自訂 TURN
func buildICEConfig(connMode, stunURLs, turnMode, turnURL, turnUser, turnPass string) webrtc.ICEConfig {
	iceConfig := webrtc.ICEConfig{}

	// 僅中繼模式
	if connMode == "relay-only" {
		iceConfig.RelayOnly = true
	}

	if stunURLs != "" {
		iceConfig.STUNServers = strings.Split(stunURLs, ",")
	}

	// 僅直連模式：跳過 TURN
	if connMode == "direct-only" {
		return iceConfig
	}

	switch turnMode {
	case "cloudflare":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		servers, err := webrtc.FetchCloudflareTURN(ctx, nil)
		if err != nil {
			slog.Warn("Cloudflare TURN fetch failed, using STUN only", "error", err)
		} else {
			iceConfig.TURNServers = servers
			slog.Info("Cloudflare TURN credentials fetched", "servers", len(servers))
		}
	case "custom":
		if turnURL != "" {
			iceConfig.TURNServers = []webrtc.TURNServer{
				{URL: turnURL, Username: turnUser, Credential: turnPass},
			}
		}
	}

	return iceConfig
}

// cliDeviceProxyCallbacks 回傳 CLI 用的 DeviceProxyManager OnReady/OnRemoved callback。
// 設備上線時印出 proxy port 並自動 adb connect，離線時自動 adb disconnect。
func cliDeviceProxyCallbacks(adbAddr string) (onReady func(context.Context, string, int), onRemoved func(string, int)) {
	onReady = func(ctx context.Context, serial string, port int) {
		fmt.Fprintf(os.Stderr, "  設備 %s → 127.0.0.1:%d\n", serial, port)
		go adb.AutoConnect(ctx, adbAddr, fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	}
	onRemoved = func(serial string, port int) {
		fmt.Fprintf(os.Stderr, "  設備 %s 已離線（port %d 已釋放）\n", serial, port)
		go adb.AutoDisconnect(adbAddr, fmt.Sprintf("127.0.0.1:%d", port))
	}
	return
}

// --- ADB port flag 輔助函式 ---

// addADBPortFlag 向 FlagSet 註冊 --adb-port flag 並回傳指標。
// 統一 5 個子命令的 flag 定義與 help 文字，避免不一致。
func addADBPortFlag(fs *flag.FlagSet) *int {
	return fs.Int("adb-port", envInt("RADB_ADB_PORT", 5037), "本機 ADB server 埠")
}

// --- 環境變數讀取輔助函式 ---
// 所有 flag 的預設值皆可透過環境變數覆蓋（如 RADB_TOKEN、RADB_SERVER_URL 等）。

// envStr 從環境變數讀取字串，不存在時回傳 fallback。
func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envStrFallback 先嘗試 key，再嘗試 fallbackKey，最後回傳 fallback。
// 用於支援環境變數改名的向後相容（例如 RADB_SERVER_URL 取代舊的 RADB_SIGNAL_URL）。
func envStrFallback(key, fallbackKey, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := os.Getenv(fallbackKey); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
