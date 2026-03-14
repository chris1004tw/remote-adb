// tab_lan.go 實作「區網直連」分頁的 GUI 與邏輯。
//
// 本分頁提供兩個子模式：
//
//  1. 被控端子模式（isServerMode=true）：在本機啟動 Direct TCP 服務 + mDNS 廣播，
//     讓同一 LAN 的主控端可以自動發現並連線。不需要 Signaling Server。
//
//  2. 主控端子模式（isServerMode=false）：透過 mDNS 掃描或手動輸入 Agent 地址，
//     連線後自動查詢設備清單，為每個在線設備建立獨立的 TCP proxy。
//
// 與 Relay 伺服器模式的差異：區網直連使用原始 TCP 連線（不經過 WebRTC），
// 延遲更低但僅限同一區域網路。
//
// 檔案結構：
//   - tab_lan.go（本檔）：lanTab struct、newLANTab、layout、cleanup
//   - tab_lan_server.go：被控端子模式（layoutServer、startServer、pollDevices、stopServer）
//   - tab_lan_connect.go：主控端子模式（layoutConnect、scan、connect、queryDevices、disconnect）
package gui

import (
	"context"
	"sync"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/bridge"
	"github.com/chris1004tw/remote-adb/internal/directsrv"
)

// lanTab 是「區網直連」分頁的完整狀態。
// isServerMode 控制目前顯示的子模式（被控端/主控端）。
// 被控端使用 srvMu 保護狀態，主控端使用 cliMu 保護狀態。
type lanTab struct {
	window *app.Window
	config *AppConfig // 共用設定（Port 等），來自設定面板

	// 子模式切換
	serverModeBtn  widget.Clickable
	connectModeBtn widget.Clickable
	isServerMode   bool

	// --- 開啟伺服器子模式（原 agentTab）---
	srvTokenEditor widget.Editor
	srvStartBtn    widget.Clickable

	srvMu      sync.Mutex
	srvRunning bool
	srvStatus  string
	srvDevices []string
	srvCancel  context.CancelFunc

	// --- 連線子模式 ---
	scanBtn        widget.Clickable
	addrEditor     widget.Editor
	cliTokenEditor widget.Editor
	connectBtn     widget.Clickable

	cliMu     sync.Mutex
	scanning  bool
	agents    []directsrv.DiscoveredAgent
	agentBtns []widget.Clickable
	connected bool
	cliStatus string
	cliCancel context.CancelFunc

	// per-device proxy 管理器（每台設備獨立 port）
	dpm *bridge.DeviceProxyManager
}

// newLANTab 建立並初始化 lanTab，設定各輸入框的預設值。
// 預設顯示主控端子模式（isServerMode=false）。
func newLANTab(w *app.Window, cfg *AppConfig) *lanTab {
	t := &lanTab{
		window:    w,
		config:    cfg,
		srvStatus: msg().Common.Stopped,
		cliStatus: msg().Common.Disconnected,
	}
	// 伺服器子模式預設值
	t.srvTokenEditor.SingleLine = true
	// 連線子模式預設值
	t.addrEditor.SingleLine = true
	t.cliTokenEditor.SingleLine = true
	return t
}

// layout 繪製分頁內容：頂部兩個子模式切換按鈕（主控端/被控端）+ 對應的設定/狀態區域。
func (t *lanTab) layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	// 處理子模式切換
	for t.serverModeBtn.Clicked(gtx) {
		t.isServerMode = true
	}
	for t.connectModeBtn.Clicked(gtx) {
		t.isServerMode = false
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// 子模式按鈕列（全寬，與主分頁對齊）
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(th, &t.connectModeBtn, msg().Common.Controller)
						if !t.isServerMode {
							btn.Background = colorModeActive
						} else {
							btn.Background = colorModeInactive
						}
						return btn.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(th, &t.serverModeBtn, msg().Common.Agent)
						if t.isServerMode {
							btn.Background = colorModeActive
						} else {
							btn.Background = colorModeInactive
						}
						return btn.Layout(gtx)
					}),
				)
			})
		}),
		// 內容區域（加水平 padding，與子模式按鈕列分離）
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				var children []layout.FlexChild
				if t.isServerMode {
					children = append(children, t.layoutServer(gtx, th)...)
				} else {
					children = append(children, t.layoutConnect(gtx, th)...)
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
			})
		}),
	)
}

// cleanup 停止被控端服務並中斷所有主控端連線。視窗關閉時呼叫。
func (t *lanTab) cleanup() {
	t.stopServer()
	t.disconnect()
}
