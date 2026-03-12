// cloudflare.go 實作從 Cloudflare Speed Test 公開端點取得免費 TURN 憑證。
//
// Cloudflare 提供 speed.cloudflare.com/turn-creds 端點，回傳短效的 TURN 憑證，
// 包含 STUN + TURN URLs、username 和 credential。每次呼叫產生新的憑證。
//
// 此端點需要帶 Referer header（https://speed.cloudflare.com/），否則回傳 403。
// 回傳的 urls 陣列同時包含 STUN 和 TURN URL，本模組僅擷取 TURN URL 供 ICEConfig 使用。
//
// 相關文件：.claude/CLAUDE.md「Cloudflare TURN 自動取得」
package webrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CloudflareTURNEndpoint 是 Cloudflare Speed Test 提供的免費 TURN 憑證端點。
const CloudflareTURNEndpoint = "https://speed.cloudflare.com/turn-creds"

// CloudflareTURNReferer 是存取端點所需的 Referer header 值。
// Cloudflare 端點會驗證此 header，缺少時回傳 403 Forbidden。
const CloudflareTURNReferer = "https://speed.cloudflare.com/"

// cloudflareTURNResponse 是 Cloudflare TURN 憑證 API 的 JSON 回應結構。
type cloudflareTURNResponse struct {
	URLs       []string `json:"urls"`       // STUN + TURN URL 混合陣列
	Username   string   `json:"username"`   // 短效使用者名稱
	Credential string   `json:"credential"` // 短效密碼
}

// FetchCloudflareTURN 從 Cloudflare 公開端點取得免費 TURN 憑證。
//
// 回傳的 TURNServer 切片僅包含 TURN/TURNS URL（過濾掉 STUN URL），
// 因為 STUN 伺服器由 ICEConfig.STUNServers 獨立設定。
//
// 參數：
//   - ctx：用於取消 HTTP 請求的 context
//   - client：HTTP 客戶端（傳入 nil 使用 http.DefaultClient）
//
// 回傳：
//   - []TURNServer：TURN 伺服器清單（通常 1 個，含多個 URL）
//   - error：HTTP 或 JSON 解析錯誤
func FetchCloudflareTURN(ctx context.Context, client *http.Client) ([]TURNServer, error) {
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CloudflareTURNEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Referer", CloudflareTURNReferer)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Cloudflare TURN creds: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Cloudflare TURN API returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var creds cloudflareTURNResponse
	if err := json.Unmarshal(body, &creds); err != nil {
		return nil, fmt.Errorf("parse Cloudflare TURN response: %w", err)
	}

	// 從混合 URL 陣列中擷取 TURN/TURNS URL（過濾 STUN）
	var turnURLs []string
	for _, u := range creds.URLs {
		if strings.HasPrefix(u, "turn:") || strings.HasPrefix(u, "turns:") {
			turnURLs = append(turnURLs, u)
		}
	}

	if len(turnURLs) == 0 {
		return nil, fmt.Errorf("Cloudflare TURN response contains no TURN URLs")
	}

	// 所有 TURN URL 共用同一組帳密，每個 URL 各建一個 TURNServer
	servers := make([]TURNServer, len(turnURLs))
	for i, u := range turnURLs {
		servers[i] = TURNServer{
			URL:        u,
			Username:   creds.Username,
			Credential: creds.Credential,
		}
	}

	return servers, nil
}
