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
// CJK 字型策略（所有平台統一手動載入 CJK 字型作為首選）：
//   - macOS：載入 PingFang.ttc，不使用 NoSystemFonts，系統字型作為 fallback。
//     確保 Latin + CJK 使用一致 metrics，避免混合 SF Pro + PingFang 造成
//     基線不一致、按鈕文字偏移、字元間距不均。
//   - Windows：手動載入微軟正黑體（msjh.ttc），路徑從 %WINDIR%\Fonts 取得
//   - Linux：手動嘗試 Noto Sans CJK（多個常見路徑）
//   - 若系統字型不可用，退回 Go 內建字型（gofont），此時中文會顯示為方框
package gui

import (
	"context"
	"crypto/rand"
	"fmt"
	"image"
	"image/color"
	"log"
	"log/slog"
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

	"github.com/chris1004tw/remote-adb/internal/gui/icons"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

// Run 啟動 GUI 主視窗，阻塞直到視窗關閉。
// Gio 的 app.Main() 必須在主 goroutine 呼叫，因此視窗邏輯放在獨立 goroutine 中執行。
func Run() {
	go func() {
		w := new(app.Window)
		w.Option(app.Title(msg().App.WindowTitle))
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
// CJK 字型策略（所有平台統一手動載入 CJK 字型作為首選）：
//   - macOS：載入 PingFang.ttc，**不使用 NoSystemFonts**，系統字型作為 fallback。
//     這確保 Latin + CJK 都使用 PingFang 的一致 metrics（基線、advance width），
//     避免混合 SF Pro + PingFang 時造成基線不一致、按鈕文字偏移、字元間距不均。
//     系統字型 fallback 覆蓋 ParseCollection 可能遺漏的字形，不會缺字。
//   - Windows：手動載入微軟正黑體（msjh.ttc），搭配 NoSystemFonts 避免掃描延遲。
//   - Linux：手動載入 Noto Sans CJK，搭配 NoSystemFonts。
//
// 所有平台手動載入時，CJK 字型放在集合前方優先使用，
// Go 內建 gofont 作為 Latin 字元的 fallback。
func newThemeWithCJK() *material.Theme {
	th := material.NewTheme()

	var fontPaths []string
	switch runtime.GOOS {
	case "darwin":
		fontPaths = []string{
			"/System/Library/Fonts/PingFang.ttc",
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
		// 優先使用 Noto Sans CJK TC（繁體中文）OTF，找不到才 fallback 到通用 TTC
		fontPaths = []string{
			"/usr/share/fonts/opentype/noto/NotoSansCJKTC-Regular.otf",
			"/usr/share/fonts/noto-cjk/NotoSansCJKTC-Regular.otf",
			"/usr/share/fonts/google-noto-cjk-tc/NotoSansCJKTC-Regular.otf",
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
		if runtime.GOOS == "darwin" {
			// macOS：保留系統字型 fallback，避免 ParseCollection
			// 解析不完整時缺字（方框）。與 NoSystemFonts 版本的差異：
			// 系統字型掃描仍會索引 /System/Library/Fonts，
			// 當 collection 中找不到字形時自動回退。
			th.Shaper = text.NewShaper(text.WithCollection(allFaces))
		} else {
			// Windows/Linux：使用 NoSystemFonts 避免系統字型掃描延遲
			th.Shaper = text.NewShaper(text.NoSystemFonts(), text.WithCollection(allFaces))
		}
	}

	// 全域暗色模式
	th.Palette.Bg = colorPanelBg
	th.Palette.Fg = colorPanelText

	return th
}

// eventLoop 是 GUI 的主事件迴圈。
// 建立三個分頁（簡易連線 / 區網直連 / Relay 伺服器），
// 持續處理視窗事件直到使用者關閉視窗。
// 關閉時會呼叫每個分頁的 cleanup() 釋放資源（設有 3 秒強制退出保護）。
func eventLoop(w *app.Window) error {
	theme := newThemeWithCJK()
	var ops op.Ops

	// 建立設定面板（載入持久化設定）
	sp := newSettingsPanel(w)

	// 初始化介面語言（從設定檔或系統偵測）
	SetLanguage(sp.config.Language)

	// 建立三個分頁，傳入共用設定
	pt := newPairTab(w, sp.config)
	lt := newLANTab(w, sp.config)
	st := newSignalTab(w, sp.config)

	tabs := &tabBar{
		items: []tabItem{
			{title: msg().App.TabPair, layoutFn: pt.layout},
			{title: msg().App.TabLAN, layoutFn: lt.layout},
			{title: msg().App.TabSignal, layoutFn: st.layout},
		},
	}

	// 啟動時自動檢查更新（背景 goroutine，不阻塞 UI）
	sp.startCheckUpdate()

	// 齒輪按鈕（右下角）
	var gearBtn widget.Clickable

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

			// 即時更新分頁標題（語言切換後下一幀生效）
			tabs.items[0].title = msg().App.TabPair
			tabs.items[1].title = msg().App.TabLAN
			tabs.items[2].title = msg().App.TabSignal

			// 處理齒輪按鈕點擊 → 開啟獨立設定子視窗
			for gearBtn.Clicked(gtx) {
				sp.openWindow()
			}

			// 深色背景
			paint.FillShape(gtx.Ops, colorPanelBg,
				clip.Rect{Max: gtx.Constraints.Max}.Op())

			// 使用 Stack 疊加：底層=分頁內容，上層=齒輪按鈕+橫幅
			layout.Stack{}.Layout(gtx,
				// 底層：分頁內容
				layout.Expanded(func(gtx layout.Context) layout.Dimensions {
					return tabs.layout(gtx, theme)
				}),
				// 齒輪按鈕（右下角）
				layout.Stacked(func(gtx layout.Context) layout.Dimensions {
					// Stacked 子元素的 Min=0，需設為 Max 才能讓 SE 定位正確
					gtx.Constraints.Min = gtx.Constraints.Max
					// 更新橫幅可見時，齒輪往上移避免被遮住
					bottomInset := unit.Dp(12)
					if sp.bannerVisible() {
						bottomInset = unit.Dp(56)
					}
					return layout.SE.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{
							Right:  unit.Dp(12),
							Bottom: bottomInset,
						}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							btn := material.IconButton(theme, &gearBtn, icons.Gear, msg().App.GearTooltip)
							btn.Size = unit.Dp(24)
							btn.Background = colorTabInactive
							btn.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
							btn.Inset = layout.UniformInset(unit.Dp(8))
							return btn.Layout(gtx)
						})
					})
				}),
				// 更新通知橫幅（底部）
				layout.Stacked(func(gtx layout.Context) layout.Dimensions {
					return sp.layoutBanner(gtx, theme)
				}),
			)

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
	colorTabActive    = color.NRGBA{R: 33, G: 150, B: 243, A: 255}  // 藍色
	colorTabInactive  = color.NRGBA{R: 96, G: 96, B: 96, A: 255}    // 灰色
	colorModeActive   = color.NRGBA{R: 0, G: 121, B: 107, A: 255}   // 深青色（子模式選擇）
	colorModeInactive = color.NRGBA{R: 158, G: 158, B: 158, A: 255} // 淺灰色（子模式未選）
	colorDivider      = color.NRGBA{R: 200, G: 200, B: 200, A: 255}
	colorEditorBg     = color.NRGBA{R: 64, G: 64, B: 64, A: 255}    // 輸入框背景（深色面板上稍亮）
	colorPanelBg      = color.NRGBA{R: 45, G: 45, B: 45, A: 255}    // 設定面板背景
	colorPanelText    = color.NRGBA{R: 240, G: 240, B: 240, A: 255} // 面板主要文字
	colorPanelHint    = color.NRGBA{R: 200, G: 200, B: 200, A: 255} // 面板次要文字 / 區塊標題
	colorPanelDivider = color.NRGBA{R: 80, G: 80, B: 80, A: 255}    // 面板分隔線
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
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx, children...)
			})
		}),

		// 分隔線
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			size := image.Pt(gtx.Constraints.Max.X, gtx.Dp(unit.Dp(1)))
			paint.FillShape(gtx.Ops, colorPanelDivider, clip.Rect{Max: size}.Op())
			return layout.Dimensions{Size: size}
		}),

		// 內容區域（僅上下 padding，水平 padding 由各分頁自行處理，
		// 讓子模式按鈕列能與主分頁等寬對齊）
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
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
// 輸入框使用深色背景 + 藍色底線，讓使用者一眼辨識可編輯區域。
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
			// 深色背景 + 藍色底線，明確標示可編輯區域
			return layout.Background{}.Layout(gtx,
				func(gtx layout.Context) layout.Dimensions {
					sz := gtx.Constraints.Min
					paint.FillShape(gtx.Ops,
						colorEditorBg,
						clip.Rect{Max: sz}.Op())
					lineH := gtx.Dp(unit.Dp(2))
					paint.FillShape(gtx.Ops, colorTabActive,
						clip.Rect{Min: image.Pt(0, sz.Y-lineH), Max: sz}.Op())
					return layout.Dimensions{Size: sz}
				},
				func(gtx layout.Context) layout.Dimensions {
					return layout.UniformInset(unit.Dp(6)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						ed := material.Editor(th, editor, hint)
						ed.TextSize = unit.Sp(14)
						ed.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
						ed.HintColor = color.NRGBA{R: 160, G: 160, B: 160, A: 255}
						return ed.Layout(gtx)
					})
				},
			)
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
			// 設定最小高度 = 最大高度，確保空白時文字框仍有固定大小，
			// 不會因為沒有內容而縮成一行高度。
			gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
			// 邊框背景
			return layout.Background{}.Layout(gtx,
				func(gtx layout.Context) layout.Dimensions {
					rect := clip.Rect{Max: gtx.Constraints.Min}
					paint.FillShape(gtx.Ops, colorEditorBg, rect.Op())
					return layout.Dimensions{Size: gtx.Constraints.Min}
				},
				func(gtx layout.Context) layout.Dimensions {
					return layout.UniformInset(unit.Dp(6)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						ed := material.Editor(th, editor, hint)
						ed.TextSize = unit.Sp(12)
						ed.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
						ed.HintColor = color.NRGBA{R: 160, G: 160, B: 160, A: 255}
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

// relayBanner 回傳一個 layout.Widget，繪製 TURN 中繼連線警告橫幅。
// 橘色背景 + 白色文字，提醒使用者目前連線透過 Cloudflare 中繼伺服器。
func relayBanner(th *material.Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			bgColor := color.NRGBA{R: 255, G: 152, B: 0, A: 255} // Material Orange 500
			return layout.Stack{}.Layout(gtx,
				layout.Expanded(func(gtx layout.Context) layout.Dimensions {
					size := image.Pt(gtx.Constraints.Min.X, gtx.Constraints.Min.Y)
					rr := gtx.Dp(unit.Dp(4))
					defer clip.RRect{Rect: image.Rectangle{Max: size}, SE: rr, SW: rr, NE: rr, NW: rr}.Push(gtx.Ops).Pop()
					paint.Fill(gtx.Ops, bgColor)
					return layout.Dimensions{Size: size}
				}),
				layout.Stacked(func(gtx layout.Context) layout.Dimensions {
					return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(th, msg().Common.RelayNotice)
						lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
						return lbl.Layout(gtx)
					})
				}),
			)
		})
	}
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

// parseICEConfig 根據 AppConfig 的 STUN/TURN 設定建構 ICEConfig。
// STUN 設定為逗號分隔的 URL 字串；自訂 TURN 設定包含 URL、帳號、密碼三個欄位。
// 注意：此函式僅處理 custom 模式的 TURN，Cloudflare 模式請用 resolveICEConfig。
func parseICEConfig(cfg *AppConfig) webrtc.ICEConfig {
	ice := webrtc.ICEConfig{}
	if cfg.STUNServer != "" {
		ice.STUNServers = strings.Split(cfg.STUNServer, ",")
	}
	if cfg.TURNMode == TURNModeCustom && cfg.TURNServer != "" {
		ice.TURNServers = []webrtc.TURNServer{
			{URL: cfg.TURNServer, Username: cfg.TURNUser, Credential: cfg.TURNPass},
		}
	}
	return ice
}

// resolveICEConfig 根據 AppConfig 建構 ICEConfig，支援 Cloudflare 自動取得 TURN 憑證。
//
// 與 parseICEConfig 不同，此函式會在 Cloudflare 模式下發出 HTTP 請求取得短效憑證，
// 因此需要 context 支援取消，且必須在 goroutine 中呼叫（不可在 UI 執行緒）。
//
// TURN 模式對應：
//   - "cloudflare"：從 Cloudflare 公開端點取得免費 TURN 憑證
//   - "custom"：使用 AppConfig 中的自訂 TURN URL/帳號/密碼
//   - ""（空）：不使用 TURN，僅 STUN
func resolveICEConfig(ctx context.Context, cfg *AppConfig) (webrtc.ICEConfig, error) {
	ice := webrtc.ICEConfig{}
	if cfg.STUNServer != "" {
		ice.STUNServers = strings.Split(cfg.STUNServer, ",")
	}

	switch cfg.TURNMode {
	case TURNModeCloudflare:
		servers, err := fetchCloudflareTURN(ctx, nil)
		if err != nil {
			return ice, fmt.Errorf("Cloudflare TURN: %w", err)
		}
		ice.TURNServers = servers
		slog.Info("fetched Cloudflare TURN credentials", "servers", len(servers))

	case TURNModeCustom:
		if cfg.TURNServer != "" {
			ice.TURNServers = []webrtc.TURNServer{
				{URL: cfg.TURNServer, Username: cfg.TURNUser, Credential: cfg.TURNPass},
			}
		}
	}

	return ice, nil
}
