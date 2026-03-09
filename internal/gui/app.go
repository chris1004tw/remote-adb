// Package gui 實作 radb 的 Gio GUI 介面。
package gui

import (
	"fmt"
	"image"
	"image/color"
	"log"
	"os"
	"runtime"
	"strings"

	"gioui.org/app"
	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

// Run 啟動 GUI 主視窗。阻塞直到視窗關閉。
func Run() {
	go func() {
		w := new(app.Window)
		w.Option(app.Title("radb — 遠端 ADB 工具"))
		w.Option(app.Size(unit.Dp(580), unit.Dp(600)))
		if err := eventLoop(w); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()
	app.Main()
}

// newThemeWithCJK 建立帶 CJK 字型的 Theme。
// 優先載入 AppleGothic（macOS），fallback 到 Microsoft JhengHei（Windows）。
func newThemeWithCJK() *material.Theme {
	th := material.NewTheme()

	// 依優先順序嘗試的系統 CJK 字型路徑
	var fontPaths []string
	switch runtime.GOOS {
	case "darwin":
		fontPaths = []string{
			"/System/Library/Fonts/Supplemental/AppleGothic.ttf",
			"/System/Library/Fonts/AppleGothic.ttf",
		}
	case "windows":
		winDir := os.Getenv("WINDIR")
		if winDir == "" {
			winDir = `C:\Windows`
		}
		fontPaths = []string{
			winDir + `\Fonts\msjh.ttc`,
		}
	default: // Linux
		fontPaths = []string{
			"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
			"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
		}
	}

	var cjkFaces []font.FontFace
	for _, p := range fontPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		faces, err := opentype.ParseCollection(data)
		if err != nil {
			// 嘗試單一字型格式
			face, err2 := opentype.Parse(data)
			if err2 != nil {
				continue
			}
			cjkFaces = append(cjkFaces, font.FontFace{Face: face})
		} else {
			cjkFaces = append(cjkFaces, faces...)
		}
		break // 找到一個就夠了
	}

	if len(cjkFaces) > 0 {
		// CJK 字型放前面優先使用，Go 內建字型作為 fallback
		allFaces := append(cjkFaces, gofont.Collection()...)
		th.Shaper = text.NewShaper(text.NoSystemFonts(), text.WithCollection(allFaces))
	}

	return th
}

func eventLoop(w *app.Window) error {
	theme := newThemeWithCJK()
	var ops op.Ops

	// 建立三個分頁
	pt := newPairTab(w)
	lt := newLANTab(w)
	st := newSignalTab(w)

	tabs := &tabBar{
		items: []tabItem{
			{title: "簡易連線", layoutFn: pt.layout},
			{title: "區網直連", layoutFn: lt.layout},
			{title: "中央伺服器", layoutFn: st.layout},
		},
	}

	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			pt.cleanup()
			lt.cleanup()
			st.cleanup()
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			tabs.layout(gtx, theme)
			e.Frame(&ops)
		}
	}
}

// --- tabBar 分頁元件 ---

type tabItem struct {
	title    string
	btn      widget.Clickable
	layoutFn func(gtx layout.Context, th *material.Theme) layout.Dimensions
}

type tabBar struct {
	items    []tabItem
	selected int
}

// 分頁按鈕的顏色
var (
	colorTabActive   = color.NRGBA{R: 33, G: 150, B: 243, A: 255} // 藍色
	colorTabInactive = color.NRGBA{R: 96, G: 96, B: 96, A: 255}   // 灰色
	colorDivider     = color.NRGBA{R: 200, G: 200, B: 200, A: 255}
)

func (t *tabBar) layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// 分頁按鈕列
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(1)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				children := make([]layout.FlexChild, len(t.items))
				for i := range t.items {
					idx := i
					children[i] = layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						for t.items[idx].btn.Clicked(gtx) {
							t.selected = idx
						}
						btn := material.Button(th, &t.items[idx].btn, t.items[idx].title)
						btn.CornerRadius = 0
						if idx == t.selected {
							btn.Background = colorTabActive
						} else {
							btn.Background = colorTabInactive
						}
						return btn.Layout(gtx)
					})
				}
				return layout.Flex{}.Layout(gtx, children...)
			})
		}),

		// 分隔線
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			size := image.Pt(gtx.Constraints.Max.X, gtx.Dp(unit.Dp(1)))
			paint.FillShape(gtx.Ops, colorDivider, clip.Rect{Max: size}.Op())
			return layout.Dimensions{Size: size}
		}),

		// 內容區域
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				if t.selected < len(t.items) {
					return t.items[t.selected].layoutFn(gtx, th)
				}
				return layout.Dimensions{}
			})
		}),
	)
}

// --- 共用 UI 輔助函式 ---

// labeledEditor 繪製「標籤 + 輸入框」的一列。
func labeledEditor(gtx layout.Context, th *material.Theme, label string, editor *widget.Editor, hint string) layout.Dimensions {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body1(th, label)
				lbl.TextSize = unit.Sp(14)
				return lbl.Layout(gtx)
			})
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			ed := material.Editor(th, editor, hint)
			ed.TextSize = unit.Sp(14)
			return ed.Layout(gtx)
		}),
	)
}

// statusText 繪製狀態文字。
func statusText(gtx layout.Context, th *material.Theme, text string, c color.NRGBA) layout.Dimensions {
	lbl := material.Body2(th, text)
	lbl.Color = c
	return lbl.Layout(gtx)
}

// parsePort 解析 port 字串，失敗時返回預設值。
func parsePort(s string, fallback int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}

// parseICEConfig 解析 STUN URL 字串為 ICEConfig。
func parseICEConfig(stunURLs string) webrtc.ICEConfig {
	cfg := webrtc.ICEConfig{}
	if stunURLs != "" {
		cfg.STUNServers = strings.Split(stunURLs, ",")
	}
	return cfg
}
