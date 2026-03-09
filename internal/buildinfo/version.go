// Package buildinfo 提供建置時注入的版本資訊。
// 透過 ldflags 注入：
//
//	go build -ldflags="-X github.com/chris1004tw/remote-adb/internal/buildinfo.Version=v1.0.0"
package buildinfo

// 這些變數由 ldflags 在建置時注入，未注入時使用預設值。
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)
