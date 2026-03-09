package updater

import (
	"context"
	"fmt"
	"io"
	"testing"
)

// mockSource 是用於測試的 Source 實作。
type mockSource struct {
	release  *ReleaseInfo
	err      error
	assets   map[string]string // url -> content
}

func (m *mockSource) LatestRelease(ctx context.Context) (*ReleaseInfo, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.release, nil
}

func (m *mockSource) DownloadAsset(ctx context.Context, url string, dest io.Writer) error {
	content, ok := m.assets[url]
	if !ok {
		return fmt.Errorf("asset not found: %s", url)
	}
	_, err := dest.Write([]byte(content))
	return err
}

func TestUpdater_Check_HasUpdate(t *testing.T) {
	u := &Updater{
		Source: &mockSource{
			release: &ReleaseInfo{
				TagName:   "v1.0.0",
				AssetURL:  "https://example.com/asset",
				AssetName: "radb-v1.0.0-test.tar.gz",
			},
		},
	}

	// buildinfo.Version 預設為 "dev"，應偵測到更新
	result, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check 失敗: %v", err)
	}

	if !result.HasUpdate {
		t.Error("預期 HasUpdate = true")
	}
	if result.LatestVersion != "v1.0.0" {
		t.Errorf("LatestVersion = %q, want %q", result.LatestVersion, "v1.0.0")
	}
}

func TestUpdater_Check_Error(t *testing.T) {
	u := &Updater{
		Source: &mockSource{
			err: fmt.Errorf("network error"),
		},
	}

	_, err := u.Check(context.Background())
	if err == nil {
		t.Fatal("預期應回傳錯誤，但沒有")
	}
}
