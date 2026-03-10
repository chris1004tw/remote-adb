// settings.go 實作設定面板的 GUI 與邏輯。
//
// 設定面板以 overlay 形式覆蓋在主視窗上方，由右下角齒輪按鈕觸發。
// 包含四個共用設定欄位（ADB Port、Proxy Port、Direct Port、STUN Server）
// 以及手動檢查更新按鈕。
//
// 設定值以 TOML 格式持久化（見 config.go），各分頁共用同一份設定。
//
// 相關文件：.claude/CLAUDE.md「專案概述 — GUI」
package gui

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/updater"
)

// stunPreset 定義 STUN 伺服器預設選項。
type stunPreset struct {
	label string // 顯示名稱（host:port）
	value string // 完整 STUN URL（stun:host:port）
}

// defaultStunPresets 是內建的公共 STUN 伺服器清單。
// 下拉選單最後一個選項「自訂」允許使用者輸入自訂位址。
var defaultStunPresets = []stunPreset{
	{"stun.l.google.com:19302", "stun:stun.l.google.com:19302"},
	{"stun1.l.google.com:19302", "stun:stun1.l.google.com:19302"},
	{"stun2.l.google.com:19302", "stun:stun2.l.google.com:19302"},
	{"stun.cloudflare.com:3478", "stun:stun.cloudflare.com:3478"},
	{"stun.nextcloud.com:443", "stun:stun.nextcloud.com:443"},
}

// settingsPanel 是設定面板的完整狀態。
// 設定以獨立子視窗方式呈現，由右下角齒輪按鈕觸發。
// 包含設定欄位的編輯框、儲存/關閉按鈕、以及檢查更新的相關狀態。
type settingsPanel struct {
	window      *app.Window // 主視窗（用於橫幅 Invalidate）
	settingsWin *app.Window // 設定子視窗（nil 表示未開啟）

	// UI 元件 — Port 編輯框
	adbPortEditor    widget.Editor
	proxyPortEditor  widget.Editor
	directPortEditor widget.Editor
	saveBtn          widget.Clickable
	closeBtn         widget.Clickable
	checkUpdateBtn   widget.Clickable
	doUpdateBtn      widget.Clickable

	// STUN 下拉選單
	stunDropExpanded bool               // 是否展開
	stunSelected     int                // 0~len(presets)-1=preset, len(presets)=自訂
	stunToggleBtn    widget.Clickable   // 下拉切換按鈕
	stunOptBtns      []widget.Clickable // 選項按鈕（presets + 自訂）
	stunEditor       widget.Editor      // 自訂 STUN 輸入框

	// 設定與路徑
	config     *AppConfig
	configPath string

	// 面板開關
	visible bool

	// 檢查更新狀態
	mu            sync.Mutex
	updateStatus  string // 更新狀態訊息
	updateChecked bool   // 是否已檢查過
	hasUpdate     bool   // 是否有可用更新
	checking      bool   // 正在檢查中
	updating      bool   // 正在更新中
	latestVersion string // 最新版本號

	// 更新通知橫幅（主畫面底部，非設定面板內）
	bannerDismissed bool             // 使用者已關閉橫幅
	bannerUpdateBtn widget.Clickable // 橫幅「立即更新」按鈕
	bannerDismissBtn widget.Clickable // 橫幅「稍後再說」按鈕

	// 捲動
	list widget.List
}

// newSettingsPanel 建立設定面板，從指定路徑載入設定。
// 若設定檔不存在，使用預設值。
func newSettingsPanel(w *app.Window) *settingsPanel {
	configPath := DefaultConfigPath()

	cfg, err := LoadConfig(configPath)
	if err != nil {
		slog.Warn("載入設定失敗，使用預設值", "error", err)
		cfg = DefaultConfig()
	}

	p := &settingsPanel{
		window:     w,
		config:     cfg,
		configPath: configPath,
	}

	// 初始化 Port 編輯框
	p.adbPortEditor.SingleLine = true
	p.proxyPortEditor.SingleLine = true
	p.directPortEditor.SingleLine = true

	// 初始化 STUN 下拉選單
	p.stunOptBtns = make([]widget.Clickable, len(defaultStunPresets)+1)
	p.stunEditor.SingleLine = true

	// 載入設定值到編輯框與下拉選單
	p.syncEditorsFromConfig()

	p.list.Axis = layout.Vertical
	return p
}

// syncEditorsFromConfig 將 config 的值同步到編輯框與 STUN 下拉選單。
func (p *settingsPanel) syncEditorsFromConfig() {
	p.adbPortEditor.SetText(strconv.Itoa(p.config.ADBPort))
	p.proxyPortEditor.SetText(strconv.Itoa(p.config.ProxyPort))
	p.directPortEditor.SetText(strconv.Itoa(p.config.DirectPort))

	// 比對 STUN 設定值是否為預設選項
	p.stunSelected = len(defaultStunPresets) // 預設選「自訂」
	for i, preset := range defaultStunPresets {
		if p.config.STUNServer == preset.value {
			p.stunSelected = i
			break
		}
	}
	// 若為自訂，填入目前設定值
	if p.stunSelected >= len(defaultStunPresets) {
		p.stunEditor.SetText(p.config.STUNServer)
	}
}

// openWindow 開啟獨立設定子視窗。
// 若已開啟則將既有視窗提到前景；否則建立新視窗並啟動事件迴圈 goroutine。
func (p *settingsPanel) openWindow() {
	p.mu.Lock()
	if p.settingsWin != nil {
		w := p.settingsWin
		p.mu.Unlock()
		w.Perform(system.ActionRaise)
		return
	}
	p.mu.Unlock()

	p.syncEditorsFromConfig()
	p.stunDropExpanded = false
	p.visible = true

	w := new(app.Window)
	w.Option(app.Title("設定"))
	w.Option(app.Size(unit.Dp(440), unit.Dp(200))) // 初始小尺寸，首幀自動成長至內容高度

	p.mu.Lock()
	p.settingsWin = w
	p.mu.Unlock()

	go p.settingsEventLoop(w)
}

// settingsEventLoop 是設定子視窗的事件迴圈。
// 處理畫面繪製與按鈕互動，視窗關閉時清除狀態。
// 視窗高度會根據內容自動調整（僅成長不縮小，避免閃爍）。
func (p *settingsPanel) settingsEventLoop(w *app.Window) {
	th := newThemeWithCJK()
	var ops op.Ops
	var lastH int // 上次設定的視窗高度（px），僅成長避免閃爍

	defer func() {
		p.mu.Lock()
		p.settingsWin = nil
		p.visible = false
		p.mu.Unlock()
		p.window.Invalidate() // 通知主視窗重繪（橫幅可能需更新）
	}()

	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)

			// 處理按鈕事件
			for p.saveBtn.Clicked(gtx) {
				p.save()
				w.Perform(system.ActionClose)
			}
			for p.closeBtn.Clicked(gtx) {
				w.Perform(system.ActionClose)
			}
			for p.checkUpdateBtn.Clicked(gtx) {
				p.startCheckUpdate()
			}
			for p.doUpdateBtn.Clicked(gtx) {
				p.startUpdate()
			}

			// 深色背景
			paint.FillShape(gtx.Ops, colorPanelBg,
				clip.Rect{Max: gtx.Constraints.Max}.Op())

			// 內容（含捲動，以防使用者手動縮小視窗）
			layout.UniformInset(unit.Dp(20)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return material.List(th, &p.list).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
					return p.layoutContent(gtx, th)
				})
			})

			// 自動調整視窗高度：讀取 List 回報的內容總高度，加上 padding
			if contentH := p.list.Position.Length; contentH > 0 {
				paddingPx := gtx.Dp(unit.Dp(40)) // 上下各 20dp
				totalPx := contentH + paddingPx
				if totalPx != lastH {
					lastH = totalPx
					h := unit.Dp(float32(totalPx) / gtx.Metric.PxPerDp)
					w.Option(app.Size(unit.Dp(440), h))
				}
			}

			e.Frame(&ops)
		}
	}
}

// save 將編輯框的值寫入 config 並持久化到檔案。
// 回傳是否儲存成功。
func (p *settingsPanel) save() bool {
	p.config.ADBPort = parsePort(p.adbPortEditor.Text(), 5037)
	p.config.ProxyPort = parsePort(p.proxyPortEditor.Text(), 5555)
	p.config.DirectPort = parsePort(p.directPortEditor.Text(), 15555)

	// 根據下拉選單選取結果決定 STUN 值
	if p.stunSelected < len(defaultStunPresets) {
		p.config.STUNServer = defaultStunPresets[p.stunSelected].value
	} else {
		p.config.STUNServer = p.stunEditor.Text()
	}

	if p.configPath == "" {
		slog.Warn("設定檔路徑為空，無法儲存")
		return false
	}

	if err := SaveConfig(p.config, p.configPath); err != nil {
		slog.Error("儲存設定失敗", "error", err)
		return false
	}

	slog.Info("設定已儲存", "path", p.configPath)
	return true
}

// layoutContent 繪製設定面板的內部內容。
func (p *settingsPanel) layoutContent(gtx layout.Context, th *material.Theme) layout.Dimensions {
	p.mu.Lock()
	updateStatus := p.updateStatus
	hasUpdate := p.hasUpdate
	checking := p.checking
	updating := p.updating
	latestVersion := p.latestVersion
	p.mu.Unlock()

	spacing := layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// 標題
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			title := material.H6(th, "設定")
			title.TextSize = unit.Sp(18)
			return title.Layout(gtx)
		}),

		spacing,

		// --- 連線設定區塊 ---
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, "連線設定")
			lbl.Color = colorPanelHint
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),

		// ADB Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "ADB Port", &p.adbPortEditor, "5037")
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

		// Proxy Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Proxy Port", &p.proxyPortEditor, "5555")
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

		// Direct Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Direct Port", &p.directPortEditor, "15555")
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

		// STUN Server（下拉選單）
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return p.layoutStunDropdown(gtx, th)
		}),

		spacing,

		// 儲存按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			btn := material.Button(th, &p.saveBtn, "儲存設定")
			btn.Background = colorTabActive
			return btn.Layout(gtx)
		}),

		spacing,

		// --- 分隔線 ---
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			size := image.Pt(gtx.Constraints.Max.X, gtx.Dp(unit.Dp(1)))
			paint.FillShape(gtx.Ops, colorPanelDivider, clip.Rect{Max: size}.Op())
			return layout.Dimensions{Size: size}
		}),

		spacing,

		// --- 版本資訊與更新 ---
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, "關於")
			lbl.Color = colorPanelHint
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),

		// 目前版本
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			ver := fmt.Sprintf("目前版本：%s", buildinfo.Version)
			return material.Body1(th, ver).Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),

		// 最新版本（檢查後才顯示）
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if latestVersion == "" {
				return layout.Dimensions{}
			}
			ver := fmt.Sprintf("最新版本：%s", latestVersion)
			return material.Body1(th, ver).Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),

		// 更新狀態訊息
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if updateStatus == "" {
				return layout.Dimensions{}
			}
			c := colorPanelHint
			if hasUpdate {
				c = color.NRGBA{R: 255, G: 152, B: 0, A: 255} // 橘色提示（暗色面板上更亮）
			}
			return statusText(gtx, th, updateStatus, c)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

		// 檢查更新 / 執行更新按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if hasUpdate && !updating {
				// 有更新可用：顯示「立即更新」按鈕
				btn := material.Button(th, &p.doUpdateBtn, "立即更新")
				btn.Background = color.NRGBA{R: 230, G: 126, B: 34, A: 255}
				return btn.Layout(gtx)
			}
			// 一般狀態：顯示「檢查更新」按鈕
			label := "檢查更新"
			if checking {
				label = "檢查中..."
			}
			if updating {
				label = "更新中..."
			}
			btn := material.Button(th, &p.checkUpdateBtn, label)
			btn.Background = colorModeActive
			return btn.Layout(gtx)
		}),

		spacing,

		// 關閉按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			btn := material.Button(th, &p.closeBtn, "關閉")
			btn.Background = colorTabInactive
			return btn.Layout(gtx)
		}),
	)
}

// layoutStunDropdown 繪製 STUN 伺服器下拉選單。
// 包含預設公共 STUN 伺服器清單，最後一個選項「自訂」允許使用者輸入自訂位址。
func (p *settingsPanel) layoutStunDropdown(gtx layout.Context, th *material.Theme) layout.Dimensions {
	totalOpts := len(defaultStunPresets) + 1

	// 處理切換按鈕點擊
	for p.stunToggleBtn.Clicked(gtx) {
		p.stunDropExpanded = !p.stunDropExpanded
	}

	// 處理選項點擊
	for i := 0; i < totalOpts; i++ {
		for p.stunOptBtns[i].Clicked(gtx) {
			p.stunSelected = i
			p.stunDropExpanded = false
		}
	}

	// 目前選取的顯示文字
	var currentLabel string
	if p.stunSelected < len(defaultStunPresets) {
		currentLabel = defaultStunPresets[p.stunSelected].label
	} else {
		currentLabel = "自訂"
	}

	arrow := " ▼"
	if p.stunDropExpanded {
		arrow = " ▲"
	}

	// 組裝垂直 Flex 子元素
	children := []layout.FlexChild{
		// 第一列：標籤 + 下拉切換按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th, "STUN Server")
						lbl.TextSize = unit.Sp(14)
						return lbl.Layout(gtx)
					})
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return p.stunToggleBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Background{}.Layout(gtx,
							func(gtx layout.Context) layout.Dimensions {
								sz := gtx.Constraints.Min
								paint.FillShape(gtx.Ops, colorEditorBg, clip.Rect{Max: sz}.Op())
								lineH := gtx.Dp(unit.Dp(2))
								paint.FillShape(gtx.Ops, colorTabActive,
									clip.Rect{Min: image.Pt(0, sz.Y-lineH), Max: sz}.Op())
								return layout.Dimensions{Size: sz}
							},
							func(gtx layout.Context) layout.Dimensions {
								return layout.UniformInset(unit.Dp(6)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									lbl := material.Body1(th, currentLabel+arrow)
									lbl.TextSize = unit.Sp(14)
									lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
									return lbl.Layout(gtx)
								})
							},
						)
					})
				}),
			)
		}),
	}

	// 展開時：選項清單
	if p.stunDropExpanded {
		for i := 0; i < totalOpts; i++ {
			idx := i
			var optLabel string
			if idx < len(defaultStunPresets) {
				optLabel = defaultStunPresets[idx].label
			} else {
				optLabel = "自訂..."
			}
			isSelected := idx == p.stunSelected

			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return p.stunOptBtns[idx].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					bg := color.NRGBA{R: 58, G: 58, B: 58, A: 255}
					if isSelected {
						bg = color.NRGBA{R: 33, G: 80, B: 120, A: 255}
					}
					return layout.Background{}.Layout(gtx,
						func(gtx layout.Context) layout.Dimensions {
							sz := gtx.Constraints.Min
							paint.FillShape(gtx.Ops, bg, clip.Rect{Max: sz}.Op())
							// 底部分隔線
							lineY := sz.Y - gtx.Dp(unit.Dp(1))
							paint.FillShape(gtx.Ops, colorPanelDivider,
								clip.Rect{Min: image.Pt(0, lineY), Max: sz}.Op())
							return layout.Dimensions{Size: sz}
						},
						func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{
								Top: unit.Dp(8), Bottom: unit.Dp(8),
								Left: unit.Dp(12), Right: unit.Dp(12),
							}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								lbl := material.Body1(th, optLabel)
								lbl.TextSize = unit.Sp(13)
								return lbl.Layout(gtx)
							})
						},
					)
				})
			}))
		}
	}

	// 自訂選項被選中且下拉收起時：顯示自訂輸入框
	if p.stunSelected >= len(defaultStunPresets) && !p.stunDropExpanded {
		children = append(children,
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Background{}.Layout(gtx,
					func(gtx layout.Context) layout.Dimensions {
						sz := gtx.Constraints.Min
						paint.FillShape(gtx.Ops, colorEditorBg, clip.Rect{Max: sz}.Op())
						lineH := gtx.Dp(unit.Dp(2))
						paint.FillShape(gtx.Ops, colorTabActive,
							clip.Rect{Min: image.Pt(0, sz.Y-lineH), Max: sz}.Op())
						return layout.Dimensions{Size: sz}
					},
					func(gtx layout.Context) layout.Dimensions {
						return layout.UniformInset(unit.Dp(6)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							ed := material.Editor(th, &p.stunEditor, "stun:your.server.com:3478")
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

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// invalidateAll 通知主視窗和設定子視窗重繪。
// 用於背景 goroutine 更新狀態後觸發 UI 刷新。
func (p *settingsPanel) invalidateAll() {
	p.window.Invalidate()
	p.mu.Lock()
	w := p.settingsWin
	p.mu.Unlock()
	if w != nil {
		w.Invalidate()
	}
}

// startCheckUpdate 在背景 goroutine 中檢查更新。
func (p *settingsPanel) startCheckUpdate() {
	p.mu.Lock()
	if p.checking || p.updating {
		p.mu.Unlock()
		return
	}
	p.checking = true
	p.updateStatus = "正在檢查更新..."
	p.hasUpdate = false
	p.mu.Unlock()
	p.invalidateAll()

	go func() {
		u := updater.NewUpdater()
		result, err := u.Check(context.Background())

		p.mu.Lock()
		p.checking = false
		if err != nil {
			p.updateStatus = fmt.Sprintf("檢查失敗：%v", err)
		} else {
			p.latestVersion = result.LatestVersion
			if result.HasUpdate {
				p.hasUpdate = true
				p.updateStatus = fmt.Sprintf("有新版本可用：%s → %s", result.CurrentVersion, result.LatestVersion)
			} else {
				p.updateStatus = "已是最新版本"
			}
		}
		p.mu.Unlock()
		p.invalidateAll()
	}()
}

// restartSelf 啟動新的自身進程後退出當前進程，實現更新後自動重啟。
// 使用 os/exec.Command 啟動新進程（與當前進程相同路徑），
// 然後以 os.Exit(0) 結束自身，讓新版本接手運行。
func (p *settingsPanel) restartSelf() {
	exePath, err := os.Executable()
	if err != nil {
		slog.Error("無法取得執行檔路徑", "error", err)
		p.mu.Lock()
		p.updateStatus = fmt.Sprintf("重啟失敗：%v", err)
		p.mu.Unlock()
		p.invalidateAll()
		return
	}
	cmd := exec.Command(exePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		slog.Error("啟動新進程失敗", "error", err)
		p.mu.Lock()
		p.updateStatus = fmt.Sprintf("重啟失敗：%v", err)
		p.mu.Unlock()
		p.invalidateAll()
		return
	}
	os.Exit(0)
}

// bannerVisible 回傳更新通知橫幅是否正在顯示。
// 用於讓齒輪按鈕在橫幅可見時上移，避免遮擋。
func (p *settingsPanel) bannerVisible() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hasUpdate && !p.bannerDismissed
}

// layoutBanner 繪製主畫面底部的更新通知橫幅。
// 僅在有可用更新且使用者尚未關閉橫幅時顯示。
// 橫幅包含版本資訊、「立即更新」和「稍後再說」兩個按鈕。
func (p *settingsPanel) layoutBanner(gtx layout.Context, th *material.Theme) layout.Dimensions {
	p.mu.Lock()
	hasUpdate := p.hasUpdate
	updating := p.updating
	dismissed := p.bannerDismissed
	latestVer := p.latestVersion
	updateStatus := p.updateStatus
	p.mu.Unlock()

	// 不顯示橫幅的條件：無更新、已關閉
	if !hasUpdate || dismissed {
		return layout.Dimensions{}
	}

	// 處理按鈕事件
	for p.bannerUpdateBtn.Clicked(gtx) {
		p.startUpdate()
	}
	for p.bannerDismissBtn.Clicked(gtx) {
		p.mu.Lock()
		p.bannerDismissed = true
		p.mu.Unlock()
	}

	// 定位到底部
	gtx.Constraints.Min = gtx.Constraints.Max
	return layout.S.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		// 橘色底色橫幅
		bannerBg := color.NRGBA{R: 50, G: 50, B: 50, A: 245}
		return layout.Background{}.Layout(gtx,
			func(gtx layout.Context) layout.Dimensions {
				sz := gtx.Constraints.Min
				paint.FillShape(gtx.Ops, bannerBg, clip.Rect{Max: sz}.Op())
				// 頂部橘色邊線
				lineH := gtx.Dp(unit.Dp(2))
				paint.FillShape(gtx.Ops, color.NRGBA{R: 255, G: 152, B: 0, A: 255},
					clip.Rect{Max: image.Pt(sz.X, lineH)}.Op())
				return layout.Dimensions{Size: sz}
			},
			func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{
					Top: unit.Dp(10), Bottom: unit.Dp(10),
					Left: unit.Dp(16), Right: unit.Dp(16),
				}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					if updating {
						// 更新進行中：顯示狀態訊息
						lbl := material.Body2(th, updateStatus)
						lbl.Color = colorPanelHint
						return lbl.Layout(gtx)
					}
					return layout.Flex{Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
						// 左側：版本資訊
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							msg := fmt.Sprintf("新版本 %s 可用", latestVer)
							lbl := material.Body2(th, msg)
							lbl.Color = color.NRGBA{R: 255, G: 200, B: 100, A: 255}
							return lbl.Layout(gtx)
						}),
						// 右側：按鈕
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Spacing: layout.SpaceStart}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									btn := material.Button(th, &p.bannerDismissBtn, "稍後再說")
									btn.Background = colorTabInactive
									btn.TextSize = unit.Sp(12)
									btn.Inset = layout.Inset{
										Top: unit.Dp(4), Bottom: unit.Dp(4),
										Left: unit.Dp(10), Right: unit.Dp(10),
									}
									return btn.Layout(gtx)
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									btn := material.Button(th, &p.bannerUpdateBtn, "立即更新")
									btn.Background = color.NRGBA{R: 230, G: 126, B: 34, A: 255}
									btn.TextSize = unit.Sp(12)
									btn.Inset = layout.Inset{
										Top: unit.Dp(4), Bottom: unit.Dp(4),
										Left: unit.Dp(10), Right: unit.Dp(10),
									}
									return btn.Layout(gtx)
								}),
							)
						}),
					)
				})
			},
		)
	})
}

// startUpdate 在背景 goroutine 中執行更新。
func (p *settingsPanel) startUpdate() {
	p.mu.Lock()
	if p.updating || p.checking {
		p.mu.Unlock()
		return
	}
	p.updating = true
	p.updateStatus = "正在下載更新..."
	p.mu.Unlock()
	p.invalidateAll()

	go func() {
		u := updater.NewUpdater()
		result, err := u.Update(context.Background())

		p.mu.Lock()
		p.updating = false
		if err != nil {
			p.updateStatus = fmt.Sprintf("更新失敗：%v", err)
		} else if result.HasUpdate {
			p.hasUpdate = false
			p.updateStatus = fmt.Sprintf("已更新至 %s，正在重新啟動...", result.LatestVersion)
			p.mu.Unlock()
			p.invalidateAll()
			// 短暫延遲讓使用者看到狀態訊息，再啟動新進程並退出
			time.Sleep(500 * time.Millisecond)
			p.restartSelf()
			return
		} else {
			p.updateStatus = "已是最新版本"
		}
		p.mu.Unlock()
		p.invalidateAll()
	}()
}
