package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "test.tar.gz")

	// 建立測試用 tar.gz
	createTestTarGz(t, archivePath, map[string]string{
		"radb":       "radb-binary",
		"radb-agent": "agent-binary",
		"README.md":  "should be skipped",
	})

	destDir := filepath.Join(dir, "out")
	os.MkdirAll(destDir, 0755)

	extracted, err := ExtractArchive(archivePath, destDir)
	if err != nil {
		t.Fatalf("ExtractArchive 失敗: %v", err)
	}

	if len(extracted) != 2 {
		t.Fatalf("提取檔案數 = %d, want 2", len(extracted))
	}

	// 驗證內容
	content, err := os.ReadFile(filepath.Join(destDir, "radb"))
	if err != nil {
		t.Fatalf("讀取 radb 失敗: %v", err)
	}
	if string(content) != "radb-binary" {
		t.Errorf("radb 內容 = %q, want %q", string(content), "radb-binary")
	}

	// 確認 README.md 沒有被提取
	if _, err := os.Stat(filepath.Join(destDir, "README.md")); !os.IsNotExist(err) {
		t.Error("不應提取 README.md")
	}
}

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "test.zip")

	// 建立測試用 zip
	createTestZip(t, archivePath, map[string]string{
		"radb.exe":       "radb-binary",
		"radb-agent.exe": "agent-binary",
		"other.txt":      "should be skipped",
	})

	destDir := filepath.Join(dir, "out")
	os.MkdirAll(destDir, 0755)

	extracted, err := ExtractArchive(archivePath, destDir)
	if err != nil {
		t.Fatalf("ExtractArchive 失敗: %v", err)
	}

	if len(extracted) != 2 {
		t.Fatalf("提取檔案數 = %d, want 2", len(extracted))
	}

	content, err := os.ReadFile(filepath.Join(destDir, "radb.exe"))
	if err != nil {
		t.Fatalf("讀取 radb.exe 失敗: %v", err)
	}
	if string(content) != "radb-binary" {
		t.Errorf("radb.exe 內容 = %q, want %q", string(content), "radb-binary")
	}
}

func TestExtractArchive_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "evil.tar.gz")

	// 建立含 path traversal 的 tar.gz
	createTestTarGz(t, archivePath, map[string]string{
		"../../../etc/passwd": "evil content",
		"radb":                "good-binary",
	})

	destDir := filepath.Join(dir, "out")
	os.MkdirAll(destDir, 0755)

	extracted, err := ExtractArchive(archivePath, destDir)
	if err != nil {
		t.Fatalf("ExtractArchive 失敗: %v", err)
	}

	// 只應提取 radb，path traversal 的 entry 應被跳過
	if len(extracted) != 1 {
		t.Fatalf("提取檔案數 = %d, want 1", len(extracted))
	}
}

func TestExtractArchive_UnsupportedFormat(t *testing.T) {
	_, err := ExtractArchive("test.unknown", t.TempDir())
	if err == nil {
		t.Fatal("預期應回傳錯誤，但沒有")
	}
}

// --- 測試輔助函式 ---

func createTestTarGz(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0755,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
}

func createTestZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
}
