// Package buildinfo 提供建置時注入的版本資訊，供 CLI 的 --version flag 與日誌輸出使用。
//
// 版本注入方式：透過 go build 的 -ldflags 在編譯期間覆寫本套件的變數值。
// CI/CD 流程（GitHub Actions release workflow）會自動帶入 git tag、commit hash 與建置時間。
//
// 完整注入範例：
//
//	go build -ldflags="\
//	  -X github.com/chris1004tw/remote-adb/internal/buildinfo.Version=v1.2.3 \
//	  -X github.com/chris1004tw/remote-adb/internal/buildinfo.Commit=abc1234 \
//	  -X github.com/chris1004tw/remote-adb/internal/buildinfo.Date=2026-03-10T12:00:00Z" \
//	  ./cmd/radb
//
// 若未透過 ldflags 注入（例如本地 go run），變數會保留預設值（"dev" / "unknown"），
// 方便開發時區分正式版本與開發版本。
package buildinfo

// 建置時注入的版本資訊變數。
// 由 -ldflags "-X ..." 在編譯期覆寫，未注入時使用預設值。
var (
	// Version 是語義化版本號（Semantic Versioning），格式為 "vMAJOR.MINOR.PATCH"。
	// 正式發布時由 git tag 決定（例如 "v1.2.3"），開發階段預設為 "dev"。
	Version = "dev"

	// Commit 是建置時的 git commit hash（短格式），用於精確追溯程式碼版本。
	Commit = "unknown"

	// Date 是建置時間（ISO 8601 格式），用於判斷執行檔的新舊程度。
	Date = "unknown"
)
