package webrtc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFetchCloudflareTURN_Success 驗證正常回應能正確解析出 TURN 伺服器清單。
func TestFetchCloudflareTURN_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ref := r.Header.Get("Referer"); ref != CloudflareTURNReferer {
			t.Errorf("Referer = %q, want %q", ref, CloudflareTURNReferer)
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

	client := &http.Client{
		Transport: &rewriteTransport{target: srv.URL},
	}

	servers, err := FetchCloudflareTURN(context.Background(), client)
	if err != nil {
		t.Fatalf("FetchCloudflareTURN failed: %v", err)
	}

	if len(servers) != 3 {
		t.Fatalf("got %d TURN servers, want 3", len(servers))
	}

	if servers[0].URL != "turn:turn.cloudflare.com:3478?transport=udp" {
		t.Errorf("servers[0].URL = %q, want turn:turn.cloudflare.com:3478?transport=udp", servers[0].URL)
	}

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
		w.Write([]byte(`{"urls":["stun:stun.cloudflare.com:3478"],"username":"u","credential":"c"}`))
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: &rewriteTransport{target: srv.URL},
	}

	_, err := FetchCloudflareTURN(context.Background(), client)
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

	_, err := FetchCloudflareTURN(context.Background(), client)
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

	_, err := FetchCloudflareTURN(context.Background(), client)
	if err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}

// TestFetchCloudflareTURN_ContextCancel 驗證 context 取消時能正確中斷。
func TestFetchCloudflareTURN_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: &rewriteTransport{target: srv.URL},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := FetchCloudflareTURN(ctx, client)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// rewriteTransport 將所有請求導向指定的測試伺服器 URL，
// 用於在不修改 CloudflareTURNEndpoint 常數的情況下進行測試。
type rewriteTransport struct {
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	newReq := req.Clone(req.Context())
	newReq.URL.Scheme = "http"
	newReq.URL.Host = t.target[len("http://"):]
	return http.DefaultTransport.RoundTrip(newReq)
}
