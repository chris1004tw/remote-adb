package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestGitHubSource_LatestRelease(t *testing.T) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	assetName := fmt.Sprintf("radb-v1.0.0-%s-%s%s", goos, goarch, ext)

	release := githubRelease{
		TagName: "v1.0.0",
		Assets: []githubAsset{
			{Name: assetName, BrowserDownloadURL: "https://example.com/" + assetName},
			{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums.txt"},
			{Name: "radb-v1.0.0-other-platform.tar.gz", BrowserDownloadURL: "https://example.com/other"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/test-owner/test-repo/releases/latest" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(release)
	}))
	defer srv.Close()

	src := &GitHubSource{
		Owner:      "test-owner",
		Repo:       "test-repo",
		HTTPClient: srv.Client(),
		BaseURL:    srv.URL,
	}

	info, err := src.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease 失敗: %v", err)
	}

	if info.TagName != "v1.0.0" {
		t.Errorf("TagName = %q, want %q", info.TagName, "v1.0.0")
	}
	if info.AssetName != assetName {
		t.Errorf("AssetName = %q, want %q", info.AssetName, assetName)
	}
	if info.ChecksumURL != "https://example.com/checksums.txt" {
		t.Errorf("ChecksumURL = %q, want checksums.txt URL", info.ChecksumURL)
	}
}

func TestGitHubSource_LatestRelease_NoMatchingAsset(t *testing.T) {
	release := githubRelease{
		TagName: "v1.0.0",
		Assets: []githubAsset{
			{Name: "radb-v1.0.0-fake-os-fake-arch.tar.gz", BrowserDownloadURL: "https://example.com/fake"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(release)
	}))
	defer srv.Close()

	src := &GitHubSource{
		Owner:      "test-owner",
		Repo:       "test-repo",
		HTTPClient: srv.Client(),
		BaseURL:    srv.URL,
	}

	_, err := src.LatestRelease(context.Background())
	if err == nil {
		t.Fatal("預期應回傳錯誤，但沒有")
	}
	if !strings.Contains(err.Error(), "找不到") {
		t.Errorf("錯誤訊息不符預期: %v", err)
	}
}

func TestGitHubSource_LatestRelease_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	src := &GitHubSource{
		Owner:      "test-owner",
		Repo:       "test-repo",
		HTTPClient: srv.Client(),
		BaseURL:    srv.URL,
	}

	_, err := src.LatestRelease(context.Background())
	if err == nil {
		t.Fatal("預期應回傳錯誤，但沒有")
	}
	if !strings.Contains(err.Error(), "尚無任何 release") {
		t.Errorf("錯誤訊息不符預期: %v", err)
	}
}

func TestGitHubSource_DownloadAsset(t *testing.T) {
	content := "test binary content"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(content))
	}))
	defer srv.Close()

	src := &GitHubSource{HTTPClient: srv.Client()}

	var buf strings.Builder
	err := src.DownloadAsset(context.Background(), srv.URL+"/asset", &buf)
	if err != nil {
		t.Fatalf("DownloadAsset 失敗: %v", err)
	}
	if buf.String() != content {
		t.Errorf("下載內容 = %q, want %q", buf.String(), content)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.0", "v1.0.1", -1},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.1.0", "v1.0.9", 1},
		{"v2.0.0", "v1.9.9", 1},
		{"1.0.0", "v1.0.0", 0},  // 有無 v prefix 皆可
		{"v0.1.0-dev", "v0.1.0", -1}, // pre-release 小於正式版
		{"v0.1.0", "v0.1.0-dev", 1},
		{"v0.1.0-alpha", "v0.1.0-beta", -1},
		{"dev", "v0.1.0", -1},        // dev 版本小於任何正式版
		{"v0.2.0", "dev", 1},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_vs_%s", tt.a, tt.b), func(t *testing.T) {
			got := CompareVersions(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
