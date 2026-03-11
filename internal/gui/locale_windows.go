// locale_windows.go 在 Windows 平台偵測系統語系。
//
// 使用 kernel32.dll 的 GetUserDefaultUILanguage 取得系統 UI 語言，
// 根據 LANGID 判斷是否為中文（zh-TW / zh-CN / zh-HK）。
//
//go:build windows

package gui

import "syscall"

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procGetUserDefaultUILanguage = kernel32.NewProc("GetUserDefaultUILanguage")
)

// detectSystemLanguage 偵測 Windows 系統語系。
// LANGID 為 16-bit，低 10 bits 為 primary language + sublanguage。
// 中文 primary language ID = 0x04：
//   - 0x0404 = zh-TW（繁體中文-台灣）
//   - 0x0804 = zh-CN（簡體中文-中國）
//   - 0x0C04 = zh-HK（繁體中文-香港）
//   - 0x1004 = zh-SG（簡體中文-新加坡）
//   - 0x1404 = zh-MO（繁體中文-澳門）
//
// 中文語系回傳 "zh-TW"（因本軟體的中文翻譯為繁體），其餘回傳 "en"。
func detectSystemLanguage() string {
	ret, _, _ := procGetUserDefaultUILanguage.Call()
	langID := uint16(ret)
	// Primary language ID（低 10 bits 的低 6 bits）
	primaryLang := langID & 0x3FF
	if primaryLang == 0x04 { // LANG_CHINESE
		return LangZhTW
	}
	return LangEN
}
