package updater

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
)

// mockSource 是用於測試的 Source 實作。
type mockSource struct {
	release *ReleaseInfo
	err     error
	assets  map[string]string // url -> content
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

func TestPickExtractedBinaryForSelf(t *testing.T) {
	t.Run("single extracted", func(t *testing.T) {
		got, err := pickExtractedBinaryForSelf([]string{"C:\\tmp\\radb.exe"}, "C:\\app\\radb-v0.2.9.exe")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "C:\\tmp\\radb.exe" {
			t.Fatalf("got %q, want %q", got, "C:\\tmp\\radb.exe")
		}
	})

	t.Run("match by extension", func(t *testing.T) {
		extracted := []string{
			filepath.FromSlash("/tmp/radb"),
			filepath.FromSlash("/tmp/radb.exe"),
		}
		got, err := pickExtractedBinaryForSelf(extracted, filepath.FromSlash("/app/radb-v0.2.9.exe"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filepath.Ext(got) != ".exe" {
			t.Fatalf("got %q, expected .exe binary", got)
		}
	})

	t.Run("no compatible binary", func(t *testing.T) {
		_, err := pickExtractedBinaryForSelf([]string{filepath.FromSlash("/tmp/radb")}, filepath.FromSlash("/app/radb.exe"))
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
