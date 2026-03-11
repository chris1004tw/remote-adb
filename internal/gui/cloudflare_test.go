package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFetchCloudflareTURN_Success 驗證正常回應能正確解析出 TURN 伺服器清單。
func TestFetchCloudflareTURN_Success(t *testing.T) {
	// 模擬 Cloudflare 回應（包含 STUN + TURN 混合 URL）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 驗證 Referer header
		if ref := r.Header.Get("Referer"); ref != cloudflareTURNReferer {
			t.Errorf("Referer = %q, want %q", ref, cloudflareTURNReferer)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"urls": [
				"stun:stun.cloudflare.com:3478",
				"turn:turn.cloudflare.com:3478?transport=udp",
				"turn:turn.cloudflare.com:3478?transport=tcp",
				"turns:turn.cloudflare.com:5349?transport=tcp"
			],
			"username": "testuser123",
			"credential": "testcred456"
		}`))
	}))
	defer srv.Close()

	// 替換端點為測試伺服器（透過自訂 RoundTripper 攔截）
	client := &http.Client{
		Transport: &rewriteTransport{target: srv.URL},
	}

	servers, err := fetchCloudflareTURN(context.Background(), client)
	if err != nil {
		t.Fatalf("fetchCloudflareTURN failed: %v", err)
	}

	// 應該只有 TURN/TURNS URL（3 個），不包含 STUN
	if len(servers) != 3 {
		t.Fatalf("got %d TURN servers, want 3", len(servers))
	}

	// 驗證第一個 TURN URL
	if servers[0].URL != "turn:turn.cloudflare.com:3478?transport=udp" {
		t.Errorf("servers[0].URL = %q, want turn:turn.cloudflare.com:3478?transport=udp", servers[0].URL)
	}

	// 驗證所有伺服器共用同一帳密
	for i, s := range servers {
		if s.Username != "testuser123" {
			t.Errorf("servers[%d].Username = %q, want testuser123", i, s.Username)
		}
		if s.Credential != "testcred456" {
			t.Errorf("servers[%d].Credential = %q, want testcred456", i, s.Credential)
		}
	}
}

// TestFetchCloudflareTURN_NoTURNURLs 驗證回應中缺少 TURN URL 時回傳錯誤。
func TestFetchCloudflareTURN_NoTURNURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 只有 STUN，沒有 TURN
		w.Write([]byte(`{"urls":["stun:stun.cloudflare.com:3478"],"username":"u","credential":"c"}`))
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: &rewriteTransport{target: srv.URL},
	}

	_, err := fetchCloudflareTURN(context.Background(), client)
	if err == nil {
		t.Fatal("expected error when no TURN URLs in response")
	}
}

// TestFetchCloudflareTURN_HTTPError 驗證非 200 回應時回傳錯誤。
func TestFetchCloudflareTURN_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: &rewriteTransport{target: srv.URL},
	}

	_, err := fetchCloudflareTURN(context.Background(), client)
	if err == nil {
		t.Fatal("expected error on 403 response")
	}
}

// TestFetchCloudflareTURN_InvalidJSON 驗證無效 JSON 回應時回傳錯誤。
func TestFetchCloudflareTURN_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: &rewriteTransport{target: srv.URL},
	}

	_, err := fetchCloudflareTURN(context.Background(), client)
	if err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}

// TestFetchCloudflareTURN_ContextCancel 驗證 context 取消時能正確中斷。
func TestFetchCloudflareTURN_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 不回應，讓 context cancel 生效
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: &rewriteTransport{target: srv.URL},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	_, err := fetchCloudflareTURN(ctx, client)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// rewriteTransport 將所有請求導向指定的測試伺服器 URL，
// 用於在不修改 cloudflareTURNEndpoint 常數的情況下進行測試。
type rewriteTransport struct {
	target string // 測試伺服器 URL（如 http://127.0.0.1:PORT）
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// 保留原始 header，只替換 URL
	newReq := req.Clone(req.Context())
	newReq.URL.Scheme = "http"
	newReq.URL.Host = t.target[len("http://"):]
	return http.DefaultTransport.RoundTrip(newReq)
}
