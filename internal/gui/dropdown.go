// dropdown.go 實作通用下拉選單 UI 元件。
//
// dropdownState 封裝展開/收合狀態、toggle 按鈕與選項按鈕，
// 三個下拉選單（STUN / TURN / Language）共用此結構消除重複渲染邏輯。
//
// 相關文件：.claude/CLAUDE.md「設定面板」
package gui

import (
	"image"
	"image/color"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// dropdownState 封裝下拉選單的 UI 狀態（展開/收合、toggle 按鈕、選項按鈕）。
// 三個下拉選單（STUN / TURN / Language）共用此結構，消除重複的渲染邏輯。
type dropdownState struct {
	expanded  bool               // 是否展開
	toggleBtn widget.Clickable   // 下拉切換按鈕
	optBtns   []widget.Clickable // 各選項按鈕（依需要自動擴展）
}

// dropdownOption 描述下拉選單中的單一選項。
type dropdownOption struct {
	Label    string // 顯示文字
	Selected bool   // 是否為目前選取項
}

// layoutDropdown 繪製通用下拉選單 UI。
//
// 參數：
//   - label: 左側標籤文字（如 "STUN Server"）
//   - currentLabel: toggle 按鈕上顯示的目前選取文字
//   - options: 選項清單（含 Label 和 Selected 狀態）
//   - onSelect: 選項被點擊時的回呼，參數為選項索引
//   - extra: 下拉選單收合時附加在底部的額外 FlexChild（如自訂輸入框），
//     展開時不渲染額外元素，可傳 nil
//
// 回傳完整下拉選單（含展開選項清單或額外元素）的 Dimensions。
func (ds *dropdownState) layoutDropdown(
	gtx layout.Context, th *material.Theme,
	label, currentLabel string,
	options []dropdownOption,
	onSelect func(int),
	extra ...layout.FlexChild,
) layout.Dimensions {
	// 自動擴展 optBtns slice
	if len(ds.optBtns) < len(options) {
		ds.optBtns = append(ds.optBtns, make([]widget.Clickable, len(options)-len(ds.optBtns))...)
	}

	// 處理切換按鈕點擊
	for ds.toggleBtn.Clicked(gtx) {
		ds.expanded = !ds.expanded
	}

	// 處理選項點擊
	for i := range options {
		for ds.optBtns[i].Clicked(gtx) {
			ds.expanded = false
			onSelect(i)
		}
	}

	arrow := " ▼"
	if ds.expanded {
		arrow = " ▲"
	}

	// 組裝垂直 Flex 子元素
	children := []layout.FlexChild{
		// 第一列：標籤 + 下拉切換按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th, label)
						lbl.TextSize = unit.Sp(14)
						return lbl.Layout(gtx)
					})
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return ds.toggleBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
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
	if ds.expanded {
		for i, opt := range options {
			idx := i
			optLabel := opt.Label
			isSelected := opt.Selected

			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ds.optBtns[idx].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
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
	} else if len(extra) > 0 {
		// 收合時才附加額外元素（如自訂輸入框）
		children = append(children, extra...)
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}
