// locale.go 提供跨平台共用的語系解析邏輯。
//
// resolveEnvLanguage 從環境變數（LC_ALL / LANG）判斷語系，
// 供各平台的 detectSystemLanguage 實作使用。
//
// 相關文件：.claude/CLAUDE.md「GUI 多語系架構」
package gui

import "strings"

// resolveEnvLanguage 根據 LC_ALL 與 LANG 環境變數值判斷語系。
// 回傳 (語言代碼, true) 表示成功從環境變數判定；
// 回傳 ("", false) 表示兩者皆空，呼叫端應使用平台特定的 fallback 機制。
//
// 優先順序：LC_ALL > LANG。以 "zh" 開頭的值判定為中文（回傳 LangZhTW），
// 其餘回傳 LangEN。
func resolveEnvLanguage(lcAll, lang string) (string, bool) {
	for _, val := range []string{lcAll, lang} {
		if val != "" {
			if strings.HasPrefix(strings.ToLower(val), "zh") {
				return LangZhTW, true
			}
			return LangEN, true
		}
	}
	return "", false
}
