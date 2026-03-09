package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceBinary(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "radb")
	newPath := filepath.Join(dir, "radb_new")

	// 建立原始 binary
	os.WriteFile(targetPath, []byte("old"), 0755)
	// 建立新 binary
	os.WriteFile(newPath, []byte("new"), 0755)

	if err := ReplaceBinary(targetPath, newPath); err != nil {
		t.Fatalf("ReplaceBinary 失敗: %v", err)
	}

	// 驗證替換結果
	content, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("讀取替換後檔案失敗: %v", err)
	}
	if string(content) != "new" {
		t.Errorf("內容 = %q, want %q", string(content), "new")
	}

	// 新檔案應該已被移走
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error("新檔案應該已被 rename 移走")
	}
}

func TestReplaceBinary_TargetNotExist(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "radb")
	newPath := filepath.Join(dir, "radb_new")

	// 只有新 binary，目標不存在
	os.WriteFile(newPath, []byte("new"), 0755)

	if err := ReplaceBinary(targetPath, newPath); err != nil {
		t.Fatalf("ReplaceBinary 失敗: %v", err)
	}

	content, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("讀取檔案失敗: %v", err)
	}
	if string(content) != "new" {
		t.Errorf("內容 = %q, want %q", string(content), "new")
	}
}

func TestCleanupOldBinaries(t *testing.T) {
	dir := t.TempDir()

	// 建立一些 .old 和正常檔案
	os.WriteFile(filepath.Join(dir, "radb.exe.old"), []byte("old"), 0644)
	os.WriteFile(filepath.Join(dir, "radb-agent.exe.old"), []byte("old"), 0644)
	os.WriteFile(filepath.Join(dir, "radb.exe"), []byte("current"), 0644)

	CleanupOldBinaries(dir)

	// .old 應該被刪除
	if _, err := os.Stat(filepath.Join(dir, "radb.exe.old")); !os.IsNotExist(err) {
		t.Error("radb.exe.old 應該被刪除")
	}
	if _, err := os.Stat(filepath.Join(dir, "radb-agent.exe.old")); !os.IsNotExist(err) {
		t.Error("radb-agent.exe.old 應該被刪除")
	}

	// 正常檔案不應被刪除
	if _, err := os.Stat(filepath.Join(dir, "radb.exe")); err != nil {
		t.Error("radb.exe 不應被刪除")
	}
}
