// cloudflare.go 委託 webrtc.FetchCloudflareTURN 取得 Cloudflare 免費 TURN 憑證。
//
// 核心實作已搬移至 internal/webrtc/cloudflare.go，本檔案僅保留 gui 套件內部的
// 便捷常數（供舊測試或內部參考），實際取得邏輯完全由 webrtc 套件提供。
//
// 相關文件：.claude/CLAUDE.md「Cloudflare TURN 自動取得」
package gui

import "github.com/chris1004tw/remote-adb/internal/webrtc"

// cloudflareTURNReferer 保留供 gui 內部參考（與 webrtc.CloudflareTURNReferer 相同）。
const cloudflareTURNReferer = webrtc.CloudflareTURNReferer
