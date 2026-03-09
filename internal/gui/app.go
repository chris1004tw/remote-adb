//go:build windows || darwin

// Package gui 實作 radb 的 Gio GUI 介面。
package gui

import (
	"image"
	"image/color"
	"log"
	"os"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// Run 啟動 GUI 主視窗。阻塞直到視窗關閉。
func Run() {
	go func() {
		w := new(app.Window)
		w.Option(app.Title("radb — 遠端 ADB 工具"))
		w.Option(app.Size(unit.Dp(580), unit.Dp(500)))
		if err := eventLoop(w); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()
	app.Main()
}

func eventLoop(w *app.Window) error {
	theme := material.NewTheme()
	var ops op.Ops

	// 建立三個分頁
	at := newAgentTab(w)
	dt := newDirectTab(w)
	pt := newPairTab(w)

	tabs := &tabBar{
		items: []tabItem{
			{title: "Agent", layoutFn: at.layout},
			{title: "Direct Connect", layoutFn: dt.layout},
			{title: "SDP 配對", layoutFn: pt.layout},
		},
	}

	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			at.cleanup()
			dt.cleanup()
			pt.cleanup()
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
