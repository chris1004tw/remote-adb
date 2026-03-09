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
	defaultOwner = "chris1004tw"
	defaultRepo  = "remote-adb"
)

// ReleaseInfo 包含 GitHub Release 的關鍵資訊。
type ReleaseInfo struct {
	TagName     string // 例如 "v0.2.0"
	AssetURL    string // archive 的瀏覽器下載 URL
	AssetName   string // 例如 "radb-v0.2.0-linux-amd64.tar.gz"
	ChecksumURL string // checksums.txt 的下載 URL（可能為空）
}

// Source 定義版本來源的介面，方便測試 mock。
type Source interface {
	LatestRelease(ctx context.Context) (*ReleaseInfo, error)
	DownloadAsset(ctx context.Context, url string, dest io.Writer) error
}

// GitHubSource 透過 GitHub Releases API 取得最新版本。
type GitHubSource struct {
	Owner      string
	Repo       string
	HTTPClient *http.Client
	BaseURL    string // 可覆寫，用於測試
}

// NewGitHubSource 建立預設的 GitHub 來源。
func NewGitHubSource() *GitHubSource {
	return &GitHubSource{
		Owner:      defaultOwner,
		Repo:       defaultRepo,
		HTTPClient: http.DefaultClient,
		BaseURL:    "https://api.github.com",
	}
}

// githubRelease 是 GitHub API 回應的子集。
type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// LatestRelease 從 GitHub API 取得最新 release 資訊。
func (g *GitHubSource) LatestRelease(ctx context.Context) (*ReleaseInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", g.BaseURL, g.Owner, g.Repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("建立請求失敗: %w", err)
	}
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

	// 匹配當前平台的 asset
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}

	expectedSuffix := fmt.Sprintf("-%s-%s%s", goos, goarch, ext)

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

// DownloadAsset 下載指定 URL 的檔案內容到 dest。
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
// 接受有無 "v" prefix 的格式。
func CompareVersions(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")

	// 分離 pre-release（例如 "1.0.0-dev" → "1.0.0", "dev"）
	aParts, aPre := splitPreRelease(a)
	bParts, bPre := splitPreRelease(b)

	partsA := parseVersionParts(aParts)
	partsB := parseVersionParts(bParts)

	// 補齊長度
	for len(partsA) < 3 {
		partsA = append(partsA, 0)
	}
	for len(partsB) < 3 {
		partsB = append(partsB, 0)
	}

	for i := 0; i < 3; i++ {
		if partsA[i] < partsB[i] {
			return -1
		}
		if partsA[i] > partsB[i] {
			return 1
		}
	}

	// 數字部分相同，比較 pre-release
	// 有 pre-release 的版本比沒有的小（例如 1.0.0-dev < 1.0.0）
	if aPre != "" && bPre == "" {
		return -1
	}
	if aPre == "" && bPre != "" {
		return 1
	}
	return strings.Compare(aPre, bPre)
}

func splitPreRelease(v string) (string, string) {
	idx := strings.IndexByte(v, '-')
	if idx == -1 {
		return v, ""
	}
	return v[:idx], v[idx+1:]
}

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
