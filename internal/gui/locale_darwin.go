// locale_darwin.go 在 macOS 平台偵測系統語系。
//
// macOS GUI 應用程式從 Finder 啟動時，LANG / LC_ALL 環境變數可能未設定，
// 因此需額外讀取 macOS 系統偏好設定中的 AppleLanguages 作為 fallback。
//
// 偵測優先順序：
//  1. LC_ALL / LANG 環境變數（終端啟動時通常有設定）
//  2. defaults read -g AppleLanguages（macOS 系統偏好設定的首選語言）
//  3. fallback 到 zh-TW
//
// 相關文件：.claude/CLAUDE.md「GUI 多語系架構」
//
//go:build darwin

package gui

import (
	"os"
	"os/exec"
	"strings"
)

// detectSystemLanguage 偵測 macOS 系統語系。
// 依序檢查環境變數與 macOS 系統偏好設定，判斷是否為中文語系。
func detectSystemLanguage() string {
	// 1. 環境變數（終端啟動時通常有設定）
	if lang, ok := resolveEnvLanguage(os.Getenv("LC_ALL"), os.Getenv("LANG")); ok {
		return lang
	}

	// 2. macOS 系統偏好設定 AppleLanguages（GUI 從 Finder 啟動時的 fallback）
	//    輸出格式：(\n    "zh-Hant-TW",\n    "en-US"\n)
	//    取第一個非空語言條目（使用者的首選語言）
	if out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			lang := strings.Trim(strings.TrimSpace(line), `"',()`)
			if lang == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(lang), "zh") {
				return LangZhTW
			}
			return LangEN // 首選語言非中文
		}
	}

	// 3. 偵測失敗，fallback 到繁中
	return LangZhTW
}
