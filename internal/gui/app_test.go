package gui

import (
	"os"
	"runtime"
	"runtime/debug"
	"testing"
)

// TestLoadCJKFaces_GCRestored 驗證 loadCJKFaces 在字型解析後正確恢復 GC 設定。
// 無論解析成功或失敗，GC percent 都必須回到呼叫前的值，
// 避免 GC 被永久停用導致記憶體洩漏。
func TestLoadCJKFaces_GCRestored(t *testing.T) {
	// 記錄呼叫前的 GOGC 值
	original := debug.SetGCPercent(100)
	debug.SetGCPercent(original) // 恢復

	t.Run("font_exists", func(t *testing.T) {
		// 取得當前平台的實際字型路徑
		paths := platformFontPaths()
		if len(paths) == 0 {
			t.Skip("no known CJK font paths for this platform")
		}

		// 確認至少一個路徑存在
		found := false
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				found = true
				break
			}
		}
		if !found {
			t.Skip("CJK font file not found on this system")
		}

		before := debug.SetGCPercent(100)
		debug.SetGCPercent(before)

		faces := loadCJKFaces(paths)
		if len(faces) == 0 {
			t.Fatal("expected CJK faces to be loaded")
		}

		after := debug.SetGCPercent(100)
		debug.SetGCPercent(after)
		if before != after {
			t.Errorf("GC percent changed: before=%d, after=%d", before, after)
		}
	})

	t.Run("font_not_found", func(t *testing.T) {
		before := debug.SetGCPercent(100)
		debug.SetGCPercent(before)

		faces := loadCJKFaces([]string{"/nonexistent/font.ttc"})
		if len(faces) != 0 {
			t.Errorf("expected nil faces for nonexistent path, got %d", len(faces))
		}

		after := debug.SetGCPercent(100)
		debug.SetGCPercent(after)
		if before != after {
			t.Errorf("GC percent changed: before=%d, after=%d", before, after)
		}
	})

	t.Run("invalid_font_data", func(t *testing.T) {
		// 建立暫存檔案，內容為無效的字型資料
		tmp, err := os.CreateTemp("", "fakefont-*.ttc")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write([]byte("not a font file")); err != nil {
			t.Fatal(err)
		}
		tmp.Close()

		before := debug.SetGCPercent(100)
		debug.SetGCPercent(before)

		faces := loadCJKFaces([]string{tmp.Name()})
		if len(faces) != 0 {
			t.Errorf("expected nil faces for invalid font, got %d", len(faces))
		}

		after := debug.SetGCPercent(100)
		debug.SetGCPercent(after)
		if before != after {
			t.Errorf("GC percent changed: before=%d, after=%d", before, after)
		}
	})
}

// TestLoadCJKFaces_ReturnsMultipleFaces 驗證 TTC 字型集合正確回傳多個字型面。
// msjh.ttc 包含 Microsoft JhengHei + Microsoft JhengHei UI 兩個字型面。
func TestLoadCJKFaces_ReturnsMultipleFaces(t *testing.T) {
	paths := platformFontPaths()
	found := false
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			found = true
			break
		}
	}
	if !found {
		t.Skip("CJK font file not found on this system")
	}

	faces := loadCJKFaces(paths)
	if len(faces) < 1 {
		t.Fatal("expected at least 1 CJK face")
	}
	t.Logf("loaded %d CJK faces", len(faces))
}

// platformFontPaths 回傳當前平台的 CJK 字型候選路徑（與 newThemeWithCJK 一致）。
func platformFontPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/System/Library/Fonts/PingFang.ttc"}
	case "windows":
		winDir := os.Getenv("WINDIR")
		if winDir == "" {
			winDir = `C:\Windows`
		}
		return []string{winDir + `\Fonts\msjh.ttc`}
	default:
		return []string{
			"/usr/share/fonts/opentype/noto/NotoSansCJKTC-Regular.otf",
			"/usr/share/fonts/noto-cjk/NotoSansCJKTC-Regular.otf",
			"/usr/share/fonts/google-noto-cjk-tc/NotoSansCJKTC-Regular.otf",
			"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
			"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
		}
	}
}
