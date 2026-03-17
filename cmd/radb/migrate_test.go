package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMigrateFile 測試單檔遷移邏輯：
// - 舊路徑存在、新路徑不存在 → 搬移（舊刪除、新建立）
// - 舊路徑不存在 → 無操作
// - 兩者皆存在 → 保留新檔、刪除舊檔
func TestMigrateFile(t *testing.T) {
	t.Run("old exists new does not", func(t *testing.T) {
		dir := t.TempDir()
		oldPath := filepath.Join(dir, "old", "config.toml")
		newPath := filepath.Join(dir, "new", "config.toml")
		os.MkdirAll(filepath.Dir(oldPath), 0755)
		os.WriteFile(oldPath, []byte("old content"), 0644)

		migrateFile(oldPath, newPath)

		if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
			t.Error("expected old file to be removed")
		}
		got, err := os.ReadFile(newPath)
		if err != nil {
			t.Fatal("expected new file to exist:", err)
		}
		if string(got) != "old content" {
			t.Errorf("new file content = %q, want %q", got, "old content")
		}
	})

	t.Run("old does not exist", func(t *testing.T) {
		dir := t.TempDir()
		newPath := filepath.Join(dir, "new.toml")

		migrateFile(filepath.Join(dir, "nonexistent"), newPath)

		if _, err := os.Stat(newPath); !os.IsNotExist(err) {
			t.Error("expected new file to not exist")
		}
	})

	t.Run("both exist keeps new", func(t *testing.T) {
		dir := t.TempDir()
		oldPath := filepath.Join(dir, "old.toml")
		newPath := filepath.Join(dir, "new.toml")

		os.WriteFile(oldPath, []byte("old"), 0644)
		os.WriteFile(newPath, []byte("new"), 0644)

		migrateFile(oldPath, newPath)

		if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
			t.Error("expected old file to be removed")
		}
		got, err := os.ReadFile(newPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "new" {
			t.Errorf("new file content = %q, want %q", got, "new")
		}
	})
}

// TestMigrateDir 測試目錄遷移邏輯：
// - 舊目錄存在、新目錄不存在 → 搬移（含子檔案）
// - 舊目錄不存在 → 無操作
// - 兩者皆存在 → 保留新目錄、刪除舊目錄
func TestMigrateDir(t *testing.T) {
	t.Run("old exists new does not", func(t *testing.T) {
		dir := t.TempDir()
		oldDir := filepath.Join(dir, "old-pt")
		newDir := filepath.Join(dir, "new-pt")

		os.MkdirAll(oldDir, 0755)
		os.WriteFile(filepath.Join(oldDir, "adb.exe"), []byte("fake adb"), 0755)

		migrateDir(oldDir, newDir)

		if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
			t.Error("expected old dir to be removed")
		}
		got, err := os.ReadFile(filepath.Join(newDir, "adb.exe"))
		if err != nil {
			t.Fatal("expected new dir to contain adb.exe:", err)
		}
		if string(got) != "fake adb" {
			t.Errorf("adb.exe content = %q, want %q", got, "fake adb")
		}
	})

	t.Run("old does not exist", func(t *testing.T) {
		dir := t.TempDir()
		newDir := filepath.Join(dir, "new")

		migrateDir(filepath.Join(dir, "nonexistent"), newDir)

		if _, err := os.Stat(newDir); !os.IsNotExist(err) {
			t.Error("expected new dir to not exist")
		}
	})

	t.Run("both exist keeps new", func(t *testing.T) {
		dir := t.TempDir()
		oldDir := filepath.Join(dir, "old-pt")
		newDir := filepath.Join(dir, "new-pt")

		os.MkdirAll(oldDir, 0755)
		os.WriteFile(filepath.Join(oldDir, "old-adb"), []byte("old"), 0755)
		os.MkdirAll(newDir, 0755)
		os.WriteFile(filepath.Join(newDir, "new-adb"), []byte("new"), 0755)

		migrateDir(oldDir, newDir)

		if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
			t.Error("expected old dir to be removed")
		}
		got, err := os.ReadFile(filepath.Join(newDir, "new-adb"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "new" {
			t.Errorf("content = %q, want %q", got, "new")
		}
	})
}

// TestRemoveEmptyDir 測試空目錄清理：
// - 空目錄 → 刪除
// - 非空目錄 → 保留
// - 不存在的目錄 → 無 panic
func TestRemoveEmptyDir(t *testing.T) {
	t.Run("empty dir removed", func(t *testing.T) {
		dir := t.TempDir()
		emptyDir := filepath.Join(dir, "empty")
		os.MkdirAll(emptyDir, 0755)

		removeEmptyDir(emptyDir)

		if _, err := os.Stat(emptyDir); !os.IsNotExist(err) {
			t.Error("expected empty dir to be removed")
		}
	})

	t.Run("non-empty dir kept", func(t *testing.T) {
		dir := t.TempDir()
		nonEmpty := filepath.Join(dir, "notempty")
		os.MkdirAll(nonEmpty, 0755)
		os.WriteFile(filepath.Join(nonEmpty, "file"), []byte("data"), 0644)

		removeEmptyDir(nonEmpty)

		if _, err := os.Stat(nonEmpty); err != nil {
			t.Error("expected non-empty dir to be kept")
		}
	})

	t.Run("nonexistent dir no panic", func(t *testing.T) {
		removeEmptyDir(filepath.Join(t.TempDir(), "nonexistent"))
	})
}