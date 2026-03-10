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
	"strconv"
	"sync"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/updater"
)

// settingsPanel 是設定面板的完整狀態。
// 包含設定欄位的編輯框、儲存/關閉按鈕、以及檢查更新的相關狀態。
type settingsPanel struct {
	window *app.Window

	// UI 元件
	adbPortEditor    widget.Editor
	proxyPortEditor  widget.Editor
	directPortEditor widget.Editor
	stunEditor       widget.Editor
	saveBtn          widget.Clickable
	closeBtn         widget.Clickable
	backdropBtn      widget.Clickable // 遮罩區域，點擊關閉面板
	checkUpdateBtn   widget.Clickable
	doUpdateBtn      widget.Clickable

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

	// 初始化編輯框
	p.adbPortEditor.SingleLine = true
	p.proxyPortEditor.SingleLine = true
	p.directPortEditor.SingleLine = true
	p.stunEditor.SingleLine = true

	// 載入設定值到編輯框
	p.syncEditorsFromConfig()

	p.list.Axis = layout.Vertical
	return p
}

// syncEditorsFromConfig 將 config 的值同步到編輯框。
func (p *settingsPanel) syncEditorsFromConfig() {
	p.adbPortEditor.SetText(strconv.Itoa(p.config.ADBPort))
	p.proxyPortEditor.SetText(strconv.Itoa(p.config.ProxyPort))
	p.directPortEditor.SetText(strconv.Itoa(p.config.DirectPort))
	p.stunEditor.SetText(p.config.STUNServer)
}

// open 顯示設定面板。
func (p *settingsPanel) open() {
	p.syncEditorsFromConfig()
	p.visible = true
}

// close 關閉設定面板。
func (p *settingsPanel) close() {
	p.visible = false
}

// save 將編輯框的值寫入 config 並持久化到檔案。
// 回傳是否儲存成功。
func (p *settingsPanel) save() bool {
	p.config.ADBPort = parsePort(p.adbPortEditor.Text(), 5037)
	p.config.ProxyPort = parsePort(p.proxyPortEditor.Text(), 5555)
	p.config.DirectPort = parsePort(p.directPortEditor.Text(), 15555)
	p.config.STUNServer = p.stunEditor.Text()

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

// layout 繪製設定面板 overlay。
// 先繪製半透明遮罩，再繪製居中的白色面板。
func (p *settingsPanel) layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	if !p.visible {
		return layout.Dimensions{}
	}

	// 處理按鈕事件
	for p.backdropBtn.Clicked(gtx) {
		p.close()
		return layout.Dimensions{Size: gtx.Constraints.Max}
	}
	for p.closeBtn.Clicked(gtx) {
		p.close()
		return layout.Dimensions{Size: gtx.Constraints.Max}
	}
	for p.saveBtn.Clicked(gtx) {
		p.save()
		p.close()
		return layout.Dimensions{Size: gtx.Constraints.Max}
	}
	for p.checkUpdateBtn.Clicked(gtx) {
		p.startCheckUpdate()
	}
	for p.doUpdateBtn.Clicked(gtx) {
		p.startUpdate()
	}

	// Stacked 子元素需設 Min=Max 才能讓定位正確
	gtx.Constraints.Min = gtx.Constraints.Max

	// 使用 Stack：底層=可點擊的半透明遮罩，上層=居中面板
	return layout.Stack{Alignment: layout.Center}.Layout(gtx,
		// 遮罩層：全螢幕可點擊，點擊關閉面板
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			return p.backdropBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				size := gtx.Constraints.Max
				paint.FillShape(gtx.Ops,
					color.NRGBA{A: 120},
					clip.Rect{Max: size}.Op(),
				)
				return layout.Dimensions{Size: size}
			})
		}),
		// 面板層：居中白色面板
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				panelWidth := gtx.Dp(unit.Dp(420))
				panelMaxH := gtx.Dp(unit.Dp(440))
				if panelWidth > gtx.Constraints.Max.X {
					panelWidth = gtx.Constraints.Max.X - gtx.Dp(unit.Dp(32))
				}
				if panelMaxH > gtx.Constraints.Max.Y {
					panelMaxH = gtx.Constraints.Max.Y - gtx.Dp(unit.Dp(32))
				}

				gtx.Constraints = layout.Exact(image.Pt(panelWidth, panelMaxH))

				// 白色背景
				rect := clip.Rect{Max: image.Pt(panelWidth, panelMaxH)}
				paint.FillShape(gtx.Ops, color.NRGBA{R: 255, G: 255, B: 255, A: 255}, rect.Op())

				return layout.UniformInset(unit.Dp(20)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.List(th, &p.list).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
						return p.layoutContent(gtx, th)
					})
				})
			})
		}),
	)
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
			lbl.Color = color.NRGBA{R: 100, G: 100, B: 100, A: 255}
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

		// STUN Server
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "STUN Server", &p.stunEditor, "stun:stun.l.google.com:19302")
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
			paint.FillShape(gtx.Ops, colorDivider, clip.Rect{Max: size}.Op())
			return layout.Dimensions{Size: size}
		}),

		spacing,

		// --- 版本資訊與更新 ---
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, "關於")
			lbl.Color = color.NRGBA{R: 100, G: 100, B: 100, A: 255}
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
			c := color.NRGBA{R: 100, G: 100, B: 100, A: 255}
			if hasUpdate {
				c = color.NRGBA{R: 230, G: 126, B: 34, A: 255} // 橘色提示
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
	p.window.Invalidate()

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
		p.window.Invalidate()
	}()
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
	p.window.Invalidate()

	go func() {
		u := updater.NewUpdater()
		result, err := u.Update(context.Background())

		p.mu.Lock()
		p.updating = false
		if err != nil {
			p.updateStatus = fmt.Sprintf("更新失敗：%v", err)
		} else if result.HasUpdate {
			p.hasUpdate = false
			p.updateStatus = fmt.Sprintf("已更新至 %s，請重新啟動程式", result.LatestVersion)
		} else {
			p.updateStatus = "已是最新版本"
		}
		p.mu.Unlock()
		p.window.Invalidate()
	}()
}
