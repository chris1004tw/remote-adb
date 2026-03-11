package gui

import "testing"

// TestSetLanguage_Explicit 驗證明確指定語言代碼時的行為。
func TestSetLanguage_Explicit(t *testing.T) {
	tests := []struct {
		name string
		lang string
		want string
	}{
		{"繁體中文", LangZhTW, LangZhTW},
		{"英文", LangEN, LangEN},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetLanguage(tt.lang)
			if got := currentLangCode(); got != tt.want {
				t.Errorf("SetLanguage(%q): currentLangCode() = %q, want %q", tt.lang, got, tt.want)
			}
		})
	}
}

// TestSetLanguage_Auto 驗證自動偵測模式回傳有效語言代碼。
func TestSetLanguage_Auto(t *testing.T) {
	SetLanguage(LangAuto)
	code := currentLangCode()
	if code != LangZhTW && code != LangEN {
		t.Errorf("SetLanguage(LangAuto): currentLangCode() = %q, want LangZhTW or LangEN", code)
	}
}

// TestDetectSystemLanguage_ReturnsValidCode 驗證平台偵測函式回傳有效值。
func TestDetectSystemLanguage_ReturnsValidCode(t *testing.T) {
	lang := detectSystemLanguage()
	if lang != LangZhTW && lang != LangEN {
		t.Errorf("detectSystemLanguage() = %q, want LangZhTW or LangEN", lang)
	}
	t.Logf("detectSystemLanguage() = %q (platform: current)", lang)
}

// TestResolveEnvLanguage 驗證環境變數語系解析邏輯。
// 此為跨平台測試，不依賴特定 OS API。
func TestResolveEnvLanguage(t *testing.T) {
	tests := []struct {
		name   string
		lcAll  string
		lang   string
		want   string
		wantOK bool // true 表示應由環境變數決定，false 表示需要 fallback
	}{
		{"LC_ALL 繁中", "zh_TW.UTF-8", "", LangZhTW, true},
		{"LC_ALL 簡中", "zh_CN.UTF-8", "", LangZhTW, true},
		{"LC_ALL 英文", "en_US.UTF-8", "", LangEN, true},
		{"LC_ALL 日文", "ja_JP.UTF-8", "", LangEN, true},
		{"LANG 繁中", "", "zh_TW.UTF-8", LangZhTW, true},
		{"LANG 英文", "", "en_US.UTF-8", LangEN, true},
		{"兩者皆空需 fallback", "", "", "", false},
		{"LC_ALL 優先於 LANG", "en_US.UTF-8", "zh_TW.UTF-8", LangEN, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolveEnvLanguage(tt.lcAll, tt.lang)
			if ok != tt.wantOK {
				t.Errorf("resolveEnvLanguage(%q, %q): ok = %v, want %v", tt.lcAll, tt.lang, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("resolveEnvLanguage(%q, %q) = %q, want %q", tt.lcAll, tt.lang, got, tt.want)
			}
		})
	}
}
