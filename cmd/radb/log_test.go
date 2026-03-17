package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLogFileName 測試 log 檔名產生邏輯：
// - 版本帶 "v" 前綴時不重複加 v
// - 版本為 "dev" 時加 "v" 前綴
// - 日期格式為 YYYY-MM-DD
func TestLogFileName(t *testing.T) {
	fixed := time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		version string
		want    string
	}{
		{"v1.2.3", "v1.2.3-2026-03-17.log"},
		{"dev", "vdev-2026-03-17.log"},
		{"0.5.0", "v0.5.0-2026-03-17.log"},
	}
	for _, tt := range tests {
		got := logFileName(tt.version, fixed)
		if got != tt.want {
			t.Errorf("logFileName(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
}

// TestCleanOldLogs 測試清理超過 maxDays 天的 log 檔案：
// - 超過期限的 .log 檔被刪除
// - 未超過期限的 .log 檔保留
// - 非 .log 檔不受影響
func TestCleanOldLogs(t *testing.T) {
	dir := t.TempDir()

	// 建立測試檔案
	oldLog := filepath.Join(dir, "v0.1.0-2026-01-01.log")
	newLog := filepath.Join(dir, "v0.1.0-2026-03-17.log")
	notLog := filepath.Join(dir, "data.txt")

	for _, f := range []string{oldLog, newLog, notLog} {
		if err := os.WriteFile(f, []byte("test"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// 將 oldLog 的修改時間設為 31 天前
	oldTime := time.Now().AddDate(0, 0, -31)
	os.Chtimes(oldLog, oldTime, oldTime)

	cleanOldLogs(dir, 30)

	// oldLog 應被刪除
	if _, err := os.Stat(oldLog); !os.IsNotExist(err) {
		t.Error("expected old log to be deleted")
	}
	// newLog 應保留
	if _, err := os.Stat(newLog); err != nil {
		t.Error("expected new log to be kept")
	}
	// 非 .log 檔應保留
	if _, err := os.Stat(notLog); err != nil {
		t.Error("expected non-log file to be kept")
	}
}

// TestCleanOldLogs_EmptyDir 測試對空目錄不 panic。
func TestCleanOldLogs_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	cleanOldLogs(dir, 30) // 不應 panic
}

// TestCleanOldLogs_NonExistentDir 測試對不存在的目錄不 panic。
func TestCleanOldLogs_NonExistentDir(t *testing.T) {
	cleanOldLogs(filepath.Join(t.TempDir(), "nonexistent"), 30) // 不應 panic
}
