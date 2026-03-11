// locale_unix.go 在 Linux 等非 Windows / 非 macOS 平台偵測系統語系。
//
// 檢查 LC_ALL / LANG 環境變數，若開頭為 "zh" 則判定為中文。
// 環境變數皆未設定時 fallback 到 zh-TW。
//
// macOS 有獨立的偵測邏輯（locale_darwin.go），因 GUI 應用程式
// 從 Finder 啟動時環境變數可能未設定，需額外讀取 AppleLanguages。
//
//go:build !windows && !darwin

package gui

import "os"

// detectSystemLanguage 偵測 Linux 系統語系。
// 優先檢查 LC_ALL，再檢查 LANG 環境變數。
// 以 "zh" 開頭（如 zh_TW.UTF-8、zh_CN.UTF-8）視為中文，回傳 "zh-TW"。
// 環境變數皆未設定時 fallback 到 zh-TW。
func detectSystemLanguage() string {
	if lang, ok := resolveEnvLanguage(os.Getenv("LC_ALL"), os.Getenv("LANG")); ok {
		return lang
	}
	return LangZhTW
}
