// turn_cache.go 實作 Cloudflare TURN 憑證的預先取得與快取機制。
//
// GUI 啟動時在背景 goroutine 中呼叫 fetchCloudflareTURN 取得短效 TURN 憑證，
// 後續 P2P 連線（clientGenerateOffer / serverProcessOffer）透過 getServers
// 直接取用快取結果，避免使用者點擊按鈕時等待 HTTP 請求。
//
// 若在指定 timeout 內背景取得尚未完成，或取得失敗，getServers 回傳空結果
// 並附帶警告訊息，呼叫端應繼續連線（僅 STUN）並向使用者顯示警告。
//
// 相關文件：.claude/CLAUDE.md「Cloudflare TURN 自動取得」
package gui

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

// turnCache 在背景預先取得 Cloudflare TURN 憑證，供 P2P 連線使用。
//
// 設計意圖：Cloudflare TURN 憑證取得需要 HTTP 請求（~200-500ms），
// 若在使用者點擊「產生邀請碼」時才發起請求，會造成明顯延遲。
// 改為啟動時預先取得，使用時直接取用快取。
type turnCache struct {
	done    chan struct{} // fetch 完成時 close
	mu      sync.Mutex
	servers []webrtc.TURNServer
	err     error
}

// newTURNCache 建立新的 TURN 憑證快取。
func newTURNCache() *turnCache {
	return &turnCache{done: make(chan struct{})}
}

// startFetch 啟動背景 goroutine 取得 Cloudflare TURN 憑證。
// 取得結果（成功或失敗）存入快取，完成後 close(done) 通知等待者。
func (tc *turnCache) startFetch() {
	go func() {
		servers, err := webrtc.FetchCloudflareTURN(context.Background(), nil)
		tc.mu.Lock()
		tc.servers = servers
		tc.err = err
		tc.mu.Unlock()
		close(tc.done)
		if err != nil {
			slog.Warn("failed to pre-fetch Cloudflare TURN credentials", "error", err)
		} else {
			slog.Info("pre-fetched Cloudflare TURN credentials", "servers", len(servers))
		}
	}()
}

// getServers 回傳快取的 TURN 伺服器清單。
// 若背景取得尚未完成：
//   - timeout > 0：最多等待 timeout 時間
//   - timeout <= 0：無限等待直到背景取得完成
//
// 回傳值：
//   - servers：TURN 伺服器清單（取得失敗或超時時為 nil）
//   - warning：非空字串表示 TURN 不可用，應向使用者顯示警告
func (tc *turnCache) getServers(timeout time.Duration) ([]webrtc.TURNServer, string) {
	if timeout <= 0 {
		<-tc.done
	} else {
		select {
		case <-tc.done:
			// fetch 已完成
		case <-time.After(timeout):
			return nil, msg().Pair.WarnTURNUnavailable
		}
	}

	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.err != nil {
		return nil, msg().Pair.WarnTURNUnavailable
	}
	return tc.servers, ""
}
