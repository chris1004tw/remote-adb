// locale_unix.go 在非 Windows 平台偵測系統語系。
//
// 檢查 LANG / LC_ALL 環境變數，若開頭為 "zh" 則判定為中文。
//
//go:build !windows

package gui

import (
	"os"
	"strings"
)

// detectSystemLanguage 偵測 Unix/macOS 系統語系。
// 優先檢查 LC_ALL，再檢查 LANG 環境變數。
// 以 "zh" 開頭（如 zh_TW.UTF-8、zh_CN.UTF-8）視為中文，回傳 "zh-TW"。
// 其餘回傳 "en"。
func detectSystemLanguage() string {
	for _, key := range []string{"LC_ALL", "LANG"} {
		if val := os.Getenv(key); val != "" {
			if strings.HasPrefix(strings.ToLower(val), "zh") {
				return LangZhTW
			}
			return LangEN
		}
	}
	return LangEN
}
