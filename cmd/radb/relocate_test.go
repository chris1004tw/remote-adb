package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestNeedsRelocate 測試搬遷判斷邏輯：
// - 資料夾名稱為 "radb"（不分大小寫）→ 不需搬遷
// - 其他名稱 → 需要搬遷
// - "radb" 作為名稱的一部分（如 "radb-tools"）→ 仍需搬遷
func TestNeedsRelocate(t *testing.T) {
	tests := []struct {
		name   string
		exeDir string
		want   bool
	}{
		{"already in radb", filepath.Join("some", "path", "radb"), false},
		{"not in radb", filepath.Join("some", "path", "Desktop"), true},
		{"case insensitive RADB", filepath.Join("some", "path", "RADB"), false},
		{"case insensitive Radb", filepath.Join("some", "path", "Radb"), false},
		{"radb as part of longer name", filepath.Join("some", "path", "radb-tools"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsRelocate(tt.exeDir)
			if got != tt.want {
				t.Errorf("needsRelocate(%q) = %v, want %v", tt.exeDir, got, tt.want)
			}
		})
	}
}

// TestCopyFile 測試檔案複製：
// - 正常複製：內容一致
// - 來源不存在：回傳 error
// - 目標目錄不存在：回傳 error
func TestCopyFile(t *testing.T) {
	t.Run("normal copy", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src.bin")
		dst := filepath.Join(dir, "dst.bin")
		content := []byte("hello world binary content")
		if err := os.WriteFile(src, content, 0644); err != nil {
			t.Fatal(err)
		}

		if err := copyFile(src, dst); err != nil {
			t.Fatalf("copyFile failed: %v", err)
		}

		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(content) {
			t.Errorf("copied content = %q, want %q", got, content)
		}
	})

	t.Run("source not found", func(t *testing.T) {
		dir := t.TempDir()
		err := copyFile(filepath.Join(dir, "nonexistent"), filepath.Join(dir, "dst"))
		if err == nil {
			t.Error("expected error for nonexistent source")
		}
	})

	t.Run("dest dir not found", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src.bin")
		os.WriteFile(src, []byte("data"), 0644)

		err := copyFile(src, filepath.Join(dir, "nonexistent", "dst"))
		if err == nil {
			t.Error("expected error for nonexistent dest directory")
		}
	})
}

// TestCleanupRelocateSource 測試搬遷後舊 exe 清理：
// - 環境變數指向有效檔案 → 刪除檔案
// - 環境變數為空 → 無操作
// - 環境變數指向不存在的檔案 → 無 panic
func TestCleanupRelocateSource(t *testing.T) {
	t.Run("env var set with valid file", func(t *testing.T) {
		dir := t.TempDir()
		oldExe := filepath.Join(dir, "old-radb.exe")
		os.WriteFile(oldExe, []byte("fake exe"), 0644)

		t.Setenv(relocateEnvKey, oldExe)
		cleanupRelocateSource()

		if _, err := os.Stat(oldExe); !os.IsNotExist(err) {
			t.Error("expected old exe to be deleted")
		}
	})

	t.Run("env var not set", func(t *testing.T) {
		t.Setenv(relocateEnvKey, "")
		cleanupRelocateSource() // should not panic
	})

	t.Run("env var set with nonexistent file", func(t *testing.T) {
		t.Setenv(relocateEnvKey, filepath.Join(t.TempDir(), "nonexistent"))
		cleanupRelocateSource() // should not panic
	})
}

func TestExecutablePath_FallbackOnEvalSymlinksError(t *testing.T) {
	origExe := osExecutable
	origEval := evalSymlinks
	defer func() {
		osExecutable = origExe
		evalSymlinks = origEval
	}()

	want := filepath.Join("C:", "temp", "radb.exe")
	osExecutable = func() (string, error) { return want, nil }
	evalSymlinks = func(string) (string, error) {
		return "", &os.PathError{Op: "EvalSymlinks", Path: want, Err: errors.New("Access is denied.")}
	}

	got, err := executablePath()
	if err != nil {
		t.Fatalf("executablePath returned error: %v", err)
	}
	if got != want {
		t.Fatalf("executablePath = %q, want %q", got, want)
	}
}

func TestExecutablePath_UsesResolvedPathWhenAvailable(t *testing.T) {
	origExe := osExecutable
	origEval := evalSymlinks
	defer func() {
		osExecutable = origExe
		evalSymlinks = origEval
	}()

	exe := filepath.Join("C:", "temp", "radb.exe")
	resolved := filepath.Join("C:", "temp", "radb", "radb.exe")
	osExecutable = func() (string, error) { return exe, nil }
	evalSymlinks = func(string) (string, error) { return resolved, nil }

	got, err := executablePath()
	if err != nil {
		t.Fatalf("executablePath returned error: %v", err)
	}
	if got != resolved {
		t.Fatalf("executablePath = %q, want %q", got, resolved)
	}
}
