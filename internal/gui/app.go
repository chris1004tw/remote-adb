// Package gui 實作 radb 的 Gio GUI 介面。
//
// 本套件使用 gioui.org (Gio) 建構跨平台 GUI，提供三個主要分頁：
//
//  1. 簡易連線（tab_pair.go）：透過 WebRTC SDP 手動交換邀請碼/回應碼，
//     實現跨 NAT P2P 連線，不需要中央伺服器。
//  2. 區網直連（tab_lan.go）：透過 mDNS 自動發現 LAN 上的 Agent，
//     使用 TCP 直連進行 ADB 轉發，適合同一網段的開發場景。
//  3. Relay 伺服器（tab_signal.go）：透過中央 Signaling Server 進行
//     WebSocket 信令交換 + WebRTC P2P 連線，適合跨網路的正式部署。
//
// CJK 字型策略：
//   - Windows：載入微軟正黑體（msjh.ttc），路徑從 %WINDIR%\Fonts 取得
//   - macOS：載入 AppleGothic.ttf
//   - Linux：嘗試 Noto Sans CJK（多個常見路徑）
//   - 若系統字型不可用，退回 Go 內建字型（gofont），此時中文會顯示為方框
package gui

import (
	"crypto/rand"
	"fmt"
	"image"
	"image/color"
	"log"
	"os"
	"runtime"
	"strings"
	"time"

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

// Run 啟動 GUI 主視窗，阻塞直到視窗關閉。
// Gio 的 app.Main() 必須在主 goroutine 呼叫，因此視窗邏輯放在獨立 goroutine 中執行。
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

// newThemeWithCJK 建立帶 CJK（中日韓）字型的 Gio Theme。
//
// 策略：根據 runtime.GOOS 嘗試載入系統 CJK 字型，找到第一個可用的即停止。
// CJK 字型放在字型集合的前方優先使用，Go 內建 gofont 作為 Latin 字元的 fallback。
// 支援 .ttf（單字型）和 .ttc（字型集合）兩種格式。
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

// eventLoop 是 GUI 的主事件迴圈。
// 建立三個分頁（簡易連線 / 區網直連 / Relay 伺服器），
// 持續處理視窗事件直到使用者關閉視窗。
// 關閉時會呼叫每個分頁的 cleanup() 釋放資源（設有 3 秒強制退出保護）。
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
			{title: "Relay 伺服器", layoutFn: st.layout},
		},
	}

	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			// 安全保險：若 cleanup 卡住，3 秒後強制退出
			go func() {
				time.Sleep(3 * time.Second)
				os.Exit(1)
			}()
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
// tabBar 是頂部分頁切換列元件，管理多個 tabItem。
// 點擊分頁按鈕會切換 selected 索引，由 layout() 方法渲染對應分頁的內容。

// tabItem 代表單一分頁：標題 + 點擊狀態 + 內容繪製函式。
type tabItem struct {
	title    string
	btn      widget.Clickable
	layoutFn func(gtx layout.Context, th *material.Theme) layout.Dimensions
}

// tabBar 管理分頁列的選取狀態。
type tabBar struct {
	items    []tabItem
	selected int // 目前選取的分頁索引
}

// 分頁按鈕的顏色
var (
	colorTabActive   = color.NRGBA{R: 33, G: 150, B: 243, A: 255} // 藍色
	colorTabInactive = color.NRGBA{R: 96, G: 96, B: 96, A: 255}   // 灰色
	colorModeActive  = color.NRGBA{R: 0, G: 121, B: 107, A: 255}  // 深青色（子模式選擇）
	colorModeInactive = color.NRGBA{R: 158, G: 158, B: 158, A: 255} // 淺灰色（子模式未選）
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
// 以下函式提供各分頁共用的 UI 繪製元件。

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

// tokenBox 繪製帶邊框、固定最大高度、可捲動的文字區塊。
//
// 設計理由：SDP token 經 base64 編碼後可能長達數百字元，若不限制高度會
// 嚴重撐開版面，影響其他 UI 元素的可見性。透過 maxHeight 參數限制最大高度，
// 超出部分可由使用者捲動檢視。
// 典型用途：顯示邀請碼/回應碼（唯讀）或讓使用者貼入對方的 token。
func tokenBox(gtx layout.Context, th *material.Theme, label string, editor *widget.Editor, hint string, maxHeight unit.Dp) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// 標籤
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body1(th, label)
				lbl.TextSize = unit.Sp(14)
				return lbl.Layout(gtx)
			})
		}),
		// 帶邊框的限高 editor
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			maxH := gtx.Dp(maxHeight)
			if gtx.Constraints.Max.Y > maxH {
				gtx.Constraints.Max.Y = maxH
			}
			// 邊框背景
			return layout.Background{}.Layout(gtx,
				func(gtx layout.Context) layout.Dimensions {
					rect := clip.Rect{Max: gtx.Constraints.Min}
					paint.FillShape(gtx.Ops, color.NRGBA{R: 240, G: 240, B: 240, A: 255}, rect.Op())
					return layout.Dimensions{Size: gtx.Constraints.Min}
				},
				func(gtx layout.Context) layout.Dimensions {
					return layout.UniformInset(unit.Dp(6)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						ed := material.Editor(th, editor, hint)
						ed.TextSize = unit.Sp(12)
						return ed.Layout(gtx)
					})
				},
			)
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

// generateToken 產生 8 字元的隨機 hex token。
func generateToken() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// fallback: 用時間戳
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
	}
	return fmt.Sprintf("%x", b)
}

// parseICEConfig 解析逗號分隔的 STUN/TURN URL 字串為 ICEConfig。
// 用於將 GUI 輸入框中的 STUN 伺服器設定傳給 WebRTC 層。
func parseICEConfig(stunURLs string) webrtc.ICEConfig {
	cfg := webrtc.ICEConfig{}
	if stunURLs != "" {
		cfg.STUNServers = strings.Split(stunURLs, ",")
	}
	return cfg
}
