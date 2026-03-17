// settings_update.go 實作設定面板的自動更新流程。
//
// 包含檢查更新、執行更新、更新通知橫幅（主畫面底部）、
// 以及更新完成後的自動重啟邏輯。
// 所有方法皆為 settingsPanel 的方法。
//
// 相關文件：.claude/CLAUDE.md「自動更新」
package gui

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/updater"
)

// startCheckUpdate 在背景 goroutine 中檢查更新。
func (p *settingsPanel) startCheckUpdate() {
	p.mu.Lock()
	if p.checking || p.updating {
		p.mu.Unlock()
		return
	}
	p.checking = true
	p.updateStatus = msg().Settings.StatusChecking
	p.hasUpdate = false
	p.mu.Unlock()
	p.invalidateAll()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		u := updater.NewUpdater()
		result, err := u.Check(ctx)

		p.mu.Lock()
		p.checking = false
		if err != nil {
			slog.Warn("check update failed", "error", err)
			p.updateStatus = fmt.Sprintf(msg().Settings.StatusCheckFailFmt, err)
		} else {
			p.latestVersion = result.LatestVersion
			if result.HasUpdate {
				p.hasUpdate = true
				p.updateStatus = fmt.Sprintf(msg().Settings.StatusUpdateAvailFmt, result.CurrentVersion, result.LatestVersion)
			} else {
				p.updateStatus = msg().Settings.StatusUpToDate
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
		slog.Error("failed to get executable path", "error", err)
		p.mu.Lock()
		p.updateStatus = fmt.Sprintf(msg().Settings.StatusRestartFailFmt, err)
		p.mu.Unlock()
		p.invalidateAll()
		return
	}
	cmd := exec.Command(exePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		slog.Error("failed to start new process", "error", err)
		p.mu.Lock()
		p.updateStatus = fmt.Sprintf(msg().Settings.StatusRestartFailFmt, err)
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
	return (p.hasUpdate || p.pendingRestart) && !p.bannerDismissed
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
	pendingRestart := p.pendingRestart
	p.mu.Unlock()

	// 更新已下載，等待連線結束：每幀檢查，全部斷開後自動重啟
	if pendingRestart && p.isAnyConnected != nil && !p.isAnyConnected() {
		p.mu.Lock()
		p.pendingRestart = false
		p.hasUpdate = false
		p.updateStatus = fmt.Sprintf(msg().Settings.StatusUpdatedFmt, latestVer)
		p.mu.Unlock()
		p.invalidateAll()
		go func() {
			time.Sleep(500 * time.Millisecond)
			p.restartSelf()
		}()
	}

	// 不顯示橫幅的條件：無更新且無待重啟、已關閉
	if (!hasUpdate && !pendingRestart) || dismissed {
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
				paint.FillShape(gtx.Ops, colorWarning,
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
							bannerMsg := fmt.Sprintf(msg().Settings.BannerNewVerFmt, latestVer)
							lbl := material.Body2(th, bannerMsg)
							lbl.Color = color.NRGBA{R: 255, G: 200, B: 100, A: 255}
							return lbl.Layout(gtx)
						}),
						// 右側：按鈕
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Spacing: layout.SpaceStart}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									btn := material.Button(th, &p.bannerDismissBtn, msg().Settings.BannerDismiss)
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
									btn := material.Button(th, &p.bannerUpdateBtn, msg().Settings.UpdateNow)
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
	p.updateStatus = msg().Settings.StatusDownloading
	p.mu.Unlock()
	p.invalidateAll()

	go func() {
		// 下載更新檔案可能較大，給予 5 分鐘逾時
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		u := updater.NewUpdater()
		result, err := u.Update(ctx)

		p.mu.Lock()
		p.updating = false
		if err != nil {
			p.updateStatus = fmt.Sprintf(msg().Settings.StatusUpdateFailFmt, err)
		} else if result.HasUpdate {
			// 有活動連線時不強制重啟，設定 pendingRestart 旗標，
			// layoutBanner 每幀檢查連線狀態，全部斷開後自動重啟
			if p.isAnyConnected != nil && p.isAnyConnected() {
				p.pendingRestart = true
				p.updateStatus = msg().Settings.StatusUpdatePendingRestart
				slog.Info("update downloaded but active connections exist, deferring restart")
				p.mu.Unlock()
				p.invalidateAll()
				return
			}

			p.hasUpdate = false
			p.updateStatus = fmt.Sprintf(msg().Settings.StatusUpdatedFmt, result.LatestVersion)
			p.mu.Unlock()
			p.invalidateAll()
			// 短暫延遲讓使用者看到狀態訊息，再啟動新進程並退出
			time.Sleep(500 * time.Millisecond)
			p.restartSelf()
			return
		} else {
			p.updateStatus = msg().Settings.StatusUpToDate
		}
		p.mu.Unlock()
		p.invalidateAll()
	}()
}
