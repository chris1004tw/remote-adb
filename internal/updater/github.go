// github.go 實作透過 GitHub Releases API 取得最新版本資訊和下載 asset 的邏輯。
//
// 呼叫流程：
//  1. LatestRelease() 呼叫 GET /repos/{owner}/{repo}/releases/latest
//  2. 從回應的 assets 列表中，根據 runtime.GOOS 和 runtime.GOARCH 匹配對應平台的 archive
//  3. DownloadAsset() 透過 asset 的 browser_download_url 下載檔案內容
package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
)

const (
	defaultOwner = "chris1004tw"  // GitHub 倉庫擁有者
	defaultRepo  = "remote-adb"  // GitHub 倉庫名稱
)

// ReleaseInfo 包含 GitHub Release 的關鍵資訊。
type ReleaseInfo struct {
	TagName     string // 版本標籤，例如 "v0.2.0"
	AssetURL    string // 對應平台 archive 的瀏覽器下載 URL
	AssetName   string // 對應平台 archive 的檔名，例如 "radb-v0.2.0-linux-amd64.tar.gz"
	ChecksumURL string // checksums.txt 的下載 URL（release 中未附帶時為空字串）
}

// Source 定義版本來源的介面。
// 抽象化設計讓 Updater 不直接依賴 GitHub API，方便單元測試時注入 mock 來源。
type Source interface {
	// LatestRelease 查詢最新的 release 資訊
	LatestRelease(ctx context.Context) (*ReleaseInfo, error)
	// DownloadAsset 下載指定 URL 的內容到 dest writer
	DownloadAsset(ctx context.Context, url string, dest io.Writer) error
}

// GitHubSource 透過 GitHub Releases API 取得最新版本。
type GitHubSource struct {
	Owner      string       // GitHub 倉庫擁有者
	Repo       string       // GitHub 倉庫名稱
	HTTPClient *http.Client // HTTP 客戶端，可替換以注入自訂 transport
	BaseURL    string       // API base URL，預設為 "https://api.github.com"；測試時可指向 httptest.Server
}

// NewGitHubSource 建立使用預設 owner/repo 和 http.DefaultClient 的 GitHub 來源。
func NewGitHubSource() *GitHubSource {
	return &GitHubSource{
		Owner:      defaultOwner,
		Repo:       defaultRepo,
		HTTPClient: http.DefaultClient,
		BaseURL:    "https://api.github.com",
	}
}

// githubRelease 是 GitHub Releases API 回應 JSON 的子集，僅映射所需欄位。
type githubRelease struct {
	TagName string        `json:"tag_name"` // release 的 Git tag（例如 "v0.2.0"）
	Assets  []githubAsset `json:"assets"`   // 附帶的檔案列表
}

// githubAsset 代表 release 中的單一附件檔案。
type githubAsset struct {
	Name               string `json:"name"`                 // 檔名（例如 "radb-v0.2.0-linux-amd64.tar.gz"）
	BrowserDownloadURL string `json:"browser_download_url"` // 可直接下載的 URL
}

// LatestRelease 從 GitHub API 取得最新 release 資訊。
//
// 呼叫 GET /repos/{owner}/{repo}/releases/latest，此 endpoint 會回傳
// 標記為 "Latest" 的 release（不含 draft 與 prerelease）。
//
// 平台匹配邏輯：
//   - 根據 runtime.GOOS 和 runtime.GOARCH 組出 suffix，例如 "-linux-amd64.tar.gz"
//   - Windows 平台使用 .zip 格式（因為 Windows 原生不支援 tar.gz），其他平台使用 .tar.gz
//   - 同時掃描 assets 列表中是否有 "checksums.txt"，供後續 SHA256 驗證使用
func (g *GitHubSource) LatestRelease(ctx context.Context) (*ReleaseInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", g.BaseURL, g.Owner, g.Repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("建立請求失敗: %w", err)
	}
	// 使用 GitHub API v3 推薦的 Accept header
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("請求 GitHub API 失敗: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("尚無任何 release")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 回應 %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("解析 release 回應失敗: %w", err)
	}

	info := &ReleaseInfo{TagName: release.TagName}

	// 依據當前平台決定 archive 格式：Windows 用 .zip，其他平台用 .tar.gz
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}

	// 組出預期的 asset 檔名 suffix，例如 "-darwin-arm64.tar.gz"
	expectedSuffix := fmt.Sprintf("-%s-%s%s", goos, goarch, ext)

	// 遍歷所有 asset，找到匹配平台的 archive 和 checksums.txt
	for _, a := range release.Assets {
		if strings.HasSuffix(a.Name, expectedSuffix) {
			info.AssetURL = a.BrowserDownloadURL
			info.AssetName = a.Name
		}
		if a.Name == "checksums.txt" {
			info.ChecksumURL = a.BrowserDownloadURL
		}
	}

	if info.AssetURL == "" {
		return nil, fmt.Errorf("找不到 %s/%s 平台的 release asset", goos, goarch)
	}

	return info, nil
}

// DownloadAsset 下載指定 URL 的檔案內容到 dest writer。
// 使用 io.Copy 串流式寫入，避免將整個檔案載入記憶體。
// 此方法同時用於下載 archive 和 checksums.txt。
func (g *GitHubSource) DownloadAsset(ctx context.Context, url string, dest io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("建立下載請求失敗: %w", err)
	}

	resp, err := g.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("下載失敗: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下載回應 %d", resp.StatusCode)
	}

	if _, err := io.Copy(dest, resp.Body); err != nil {
		return fmt.Errorf("寫入檔案失敗: %w", err)
	}
	return nil
}

// CompareVersions 比較兩個 semver 版本字串。
// 回傳 -1 (a < b)、0 (a == b)、1 (a > b)。
//
// 比較規則（符合 Semantic Versioning 2.0.0 規範）：
//  1. 容許有或沒有 "v" 前綴（"v1.2.3" 與 "1.2.3" 等價）
//  2. 先逐段比較 major.minor.patch 的數字大小，不足三段的補 0
//  3. 數字部分相同時，比較 pre-release 標識：
//     - 有 pre-release 的版本小於沒有的（例如 "1.0.0-dev" < "1.0.0"）
//     - 兩者都有 pre-release 時，以字串字典序比較
func CompareVersions(a, b string) int {
	// 移除 "v" 前綴以統一格式
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")

	// 分離 pre-release 標識（例如 "1.0.0-dev" → 數字部分 "1.0.0"、pre-release "dev"）
	aParts, aPre := splitPreRelease(a)
	bParts, bPre := splitPreRelease(b)

	partsA := parseVersionParts(aParts)
	partsB := parseVersionParts(bParts)

	// 補齊至三段（major.minor.patch），缺少的視為 0
	for len(partsA) < 3 {
		partsA = append(partsA, 0)
	}
	for len(partsB) < 3 {
		partsB = append(partsB, 0)
	}

	// 逐段比較 major → minor → patch
	for i := 0; i < 3; i++ {
		if partsA[i] < partsB[i] {
			return -1
		}
		if partsA[i] > partsB[i] {
			return 1
		}
	}

	// 數字部分完全相同，根據 pre-release 決定：
	// 有 pre-release 的版本比沒有的小（例如 1.0.0-dev < 1.0.0）
	if aPre != "" && bPre == "" {
		return -1
	}
	if aPre == "" && bPre != "" {
		return 1
	}
	// 兩者都有或都沒有 pre-release，以字典序比較
	return strings.Compare(aPre, bPre)
}

// splitPreRelease 將版本字串以第一個 '-' 分割為數字部分和 pre-release 標識。
// 例如 "1.0.0-beta.1" → ("1.0.0", "beta.1")，"1.0.0" → ("1.0.0", "")
func splitPreRelease(v string) (string, string) {
	idx := strings.IndexByte(v, '-')
	if idx == -1 {
		return v, ""
	}
	return v[:idx], v[idx+1:]
}

// parseVersionParts 將 "1.2.3" 這樣的點分隔字串解析為整數切片 [1, 2, 3]。
// 無法解析為整數的段落視為 0。
func parseVersionParts(v string) []int {
	parts := strings.Split(v, ".")
	result := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			result = append(result, 0)
			continue
		}
		result = append(result, n)
	}
	return result
}
