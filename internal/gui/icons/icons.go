// Package icons 提供 GUI 使用的 Material Design 圖示。
//
// 圖示來源為 golang.org/x/exp/shiny/materialdesign/icons（IconVG 向量格式），
// 透過 gioui.org/widget.NewIcon 解析後供各 GUI 元件使用。
//
// 新增圖示時，在此檔案中加入對應的變數與初始化即可。
// 完整圖示清單見 https://fonts.google.com/icons
//
// 相關文件：.claude/CLAUDE.md「專案結構 — internal/gui/icons」
package icons

import (
	"gioui.org/widget"

	mdicons "golang.org/x/exp/shiny/materialdesign/icons"
)

// Gear 是齒輪圖示（Material Design "Settings"），用於設定面板按鈕。
var Gear *widget.Icon

func init() {
	var err error
	Gear, err = widget.NewIcon(mdicons.ActionSettings)
	if err != nil {
		panic("載入齒輪圖示失敗: " + err.Error())
	}
}
