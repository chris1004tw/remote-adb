package gui

import (
	"fmt"
	"testing"
	"time"

	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

// TestTURNCacheReady 測試快取已就緒時立即回傳結果。
func TestTURNCacheReady(t *testing.T) {
	tc := &turnCache{done: make(chan struct{})}
	tc.servers = []webrtc.TURNServer{
		{URL: "turn:test.example.com:3478", Username: "user", Credential: "pass"},
	}
	close(tc.done) // 模擬 fetch 已完成

	servers, warning := tc.getServers(time.Second)
	if warning != "" {
		t.Errorf("expected no warning, got: %s", warning)
	}
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if servers[0].URL != "turn:test.example.com:3478" {
		t.Errorf("unexpected URL: %s", servers[0].URL)
	}
}

// TestTURNCacheTimeout 測試快取未就緒時超時回傳警告。
func TestTURNCacheTimeout(t *testing.T) {
	tc := newTURNCache() // done channel 未 close

	servers, warning := tc.getServers(50 * time.Millisecond)
	if warning == "" {
		t.Error("expected warning for timeout, got empty")
	}
	if servers != nil {
		t.Errorf("expected nil servers on timeout, got %v", servers)
	}
}

// TestTURNCacheFetchError 測試 fetch 失敗時回傳警告。
func TestTURNCacheFetchError(t *testing.T) {
	tc := &turnCache{done: make(chan struct{})}
	tc.err = fmt.Errorf("network error")
	close(tc.done)

	servers, warning := tc.getServers(time.Second)
	if warning == "" {
		t.Error("expected warning for fetch error, got empty")
	}
	if servers != nil {
		t.Errorf("expected nil servers on error, got %v", servers)
	}
}

// TestTURNCacheAsyncReady 測試快取在 timeout 內完成時正確回傳。
func TestTURNCacheAsyncReady(t *testing.T) {
	tc := newTURNCache()

	// 模擬 50ms 後完成 fetch
	go func() {
		time.Sleep(50 * time.Millisecond)
		tc.mu.Lock()
		tc.servers = []webrtc.TURNServer{
			{URL: "turn:async.example.com:3478"},
		}
		tc.mu.Unlock()
		close(tc.done)
	}()

	servers, warning := tc.getServers(time.Second)
	if warning != "" {
		t.Errorf("expected no warning, got: %s", warning)
	}
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if servers[0].URL != "turn:async.example.com:3478" {
		t.Errorf("unexpected URL: %s", servers[0].URL)
	}
}
