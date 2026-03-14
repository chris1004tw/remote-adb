// tab_lan_connect.go — 區網直連分頁：主控端子模式（連線）。
//
// 提供 mDNS 掃描 + 手動輸入 Agent 地址兩種連線方式。
// 連線後自動查詢 Agent 上的設備清單，為每個在線設備建立獨立 proxy。
package gui

import (
	"context"
	"fmt"
	"image/color"
	"log/slog"
	"time"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/bridge"
	"github.com/chris1004tw/remote-adb/internal/directsrv"
)

// layoutConnect 繪製主控端子模式的 UI：掃描按鈕、Agent 列表、地址/Token/Port 輸入、連線按鈕。
func (t *lanTab) layoutConnect(gtx layout.Context, th *material.Theme) []layout.FlexChild {
	t.cliMu.Lock()
	scanning := t.scanning
	agents := append([]directsrv.DiscoveredAgent{}, t.agents...)
	connected := t.connected
	status := t.cliStatus
	dpm := t.dpm
	t.cliMu.Unlock()

	for len(t.agentBtns) < len(agents) {
		t.agentBtns = append(t.agentBtns, widget.Clickable{})
	}

	// 處理按鈕事件
	for t.scanBtn.Clicked(gtx) {
		if !scanning {
			t.scan()
		}
	}
	for i := range agents {
		for t.agentBtns[i].Clicked(gtx) {
			addr := fmt.Sprintf("%s:%d", agents[i].Addr, agents[i].Port)
			t.addrEditor.SetText(addr)
			if agents[i].Token != "" {
				t.cliTokenEditor.SetText(agents[i].Token)
			}
		}
	}
	for t.connectBtn.Clicked(gtx) {
		if connected {
			t.disconnect()
		} else {
			t.connect()
		}
	}

	var children []layout.FlexChild

	// 掃描按鈕
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			label := msg().LAN.ScanLAN
			if scanning {
				label = msg().LAN.Scanning
			}
			btn := material.Button(th, &t.scanBtn, label)
			return btn.Layout(gtx)
		})
	}))

	// Agent 列表
	if len(agents) > 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				items := []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Body2(th, fmt.Sprintf(msg().LAN.AgentsFoundFmt, len(agents))).Layout(gtx)
					}),
				}
				for i, a := range agents {
					idx := i
					text := fmt.Sprintf("  %s (%s:%d)", a.Name, a.Addr, a.Port)
					_ = idx
					items = append(items, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(8), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							btn := material.Button(th, &t.agentBtns[idx], text)
							btn.Background = color.NRGBA{R: 96, G: 96, B: 96, A: 255}
							return btn.Layout(gtx)
						})
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, items...)
			})
		}))
	}

	// Agent 地址
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, msg().LAN.AgentAddr, &t.addrEditor, "192.168.1.100:15555")
		})
	}))
	// Token
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, msg().Common.TokenLabel, &t.cliTokenEditor, msg().Common.TokenHintOptional)
		})
	}))
	// 連線/斷線按鈕
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		label := msg().Common.Connect
		if connected {
			label = msg().Common.DisconnectBtn
		}
		btn := material.Button(th, &t.connectBtn, label)
		if connected {
			btn.Background = color.NRGBA{R: 244, G: 67, B: 54, A: 255}
		}
		return btn.Layout(gtx)
	}))

	// 狀態
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		c := colorPanelHint
		if connected {
			c = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
		}
		return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return statusText(gtx, th, msg().Common.StatusPrefix+status, c)
		})
	}))

	// 已連線 per-device 設備列表
	var entries []bridge.DeviceEntry
	if connected && dpm != nil {
		entries = dpm.Entries()
	}
	if len(entries) > 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layoutDeviceEntries(gtx, th, entries)
			})
		}))
	}

	return children
}

// scan 在背景發起 mDNS 掃描（3 秒逾時），搜尋 LAN 上廣播的 radb Agent。
// 掃描結果更新到 agents 列表，點擊 Agent 按鈕會自動填入地址和 Token。
func (t *lanTab) scan() {
	t.cliMu.Lock()
	t.scanning = true
	t.cliMu.Unlock()
	t.window.Invalidate()

	go func() {
		agents, err := directsrv.DiscoverMDNS(3 * time.Second)
		t.cliMu.Lock()
		t.agents = agents
		if err != nil {
			t.cliStatus = fmt.Sprintf(msg().Common.ErrorFmt, err)
		}
		t.scanning = false
		t.agentBtns = make([]widget.Clickable, len(agents))
		t.cliMu.Unlock()
		t.window.Invalidate()
	}()
}

// connect 連線到 Agent，建立單一 ADB proxy。
//
// 新流程（複用簡易連線的智慧協定偵測）：
//  1. TCP 連線到 Agent → 發送 "list" 請求 → 取得設備清單
//  2. 在本機建立 ADB proxy listener
//  3. 每個進入的 TCP 連線偵測協定：
//     - hex prefix → connect-server 橋接到遠端 ADB server
//     - CNXN → deviceBridge 多工處理（每個 OPEN 透過 connect-service 連到遠端設備）
func (t *lanTab) connect() {
	addr := t.addrEditor.Text()
	token := t.cliTokenEditor.Text()

	if addr == "" {
		t.cliMu.Lock()
		t.cliStatus = msg().LAN.StatusPleaseAddr
		t.cliMu.Unlock()
		t.window.Invalidate()
		return
	}

	proxyPort := t.config.ProxyPort

	t.cliMu.Lock()
	t.cliStatus = msg().LAN.StatusQuerying
	t.cliMu.Unlock()
	t.window.Invalidate()

	go func() {
		// 1. 查詢設備清單
		devices, err := t.queryDevices(addr, token)
		if err != nil {
			t.cliMu.Lock()
			t.cliStatus = err.Error()
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}

		if len(devices) == 0 {
			t.cliMu.Lock()
			t.cliStatus = msg().LAN.StatusNoDevices
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}

		ctx, cancel := context.WithCancel(context.Background())

		// 2. 建立 per-device proxy 管理器
		openCh := directsrv.NewOpenChannelFunc(addr, token)
		onReady, onRemoved := guiDeviceProxyCallbacks(t.window, "LAN device proxy")
		dpm := bridge.NewDeviceProxyManager(bridge.DeviceProxyConfig{
			PortStart: proxyPort,
			OpenCh:    openCh,
			ADBAddr:   fmt.Sprintf("127.0.0.1:%d", t.config.ADBPort),
			OnReady:   onReady,
			OnRemoved: onRemoved,
		})

		// 初始設備
		dpm.UpdateDevices(directsrv.ToBridgeDevices(devices))

		t.cliMu.Lock()
		t.connected = true
		t.cliCancel = cancel
		t.dpm = dpm
		t.cliStatus = fmt.Sprintf(msg().LAN.StatusConnectedDevFmt, len(devices))
		t.cliMu.Unlock()
		t.window.Invalidate()

		slog.Info("LAN per-device proxy started", "remote", addr, "devices", len(devices))

		// 3. 定期輪詢設備清單
		go directsrv.PollDeviceLoop(ctx, 3*time.Second,
			func() []directsrv.DeviceInfo {
				devs, err := t.queryDevices(addr, token)
				if err != nil {
					slog.Debug("LAN device polling failed", "error", err)
					return nil
				}
				return devs
			},
			func(devs []bridge.DeviceInfo) {
				t.cliMu.Lock()
				dpm := t.dpm
				t.cliMu.Unlock()
				if dpm != nil {
					dpm.UpdateDevices(devs)
				}
				t.window.Invalidate()
			},
		)
	}()
}

// queryDevices 向 Agent 查詢設備清單，僅回傳在線設備（State=="device"）。
func (t *lanTab) queryDevices(addr, token string) ([]directsrv.DeviceInfo, error) {
	resp, err := directsrv.QueryDevices(addr, token)
	if err != nil {
		return nil, fmt.Errorf(msg().LAN.ErrQueryFmt, err)
	}

	// 篩選 device 狀態
	var online []directsrv.DeviceInfo
	for _, d := range resp.Devices {
		if d.State == "device" {
			online = append(online, d)
		}
	}
	return online, nil
}

// disconnect 中斷連線，關閉 DeviceProxyManager（內部觸發 OnRemoved 自動 adb disconnect）。
// dpm.Close() 在 goroutine 中非同步執行，避免多台設備時阻塞 UI 執行緒。
func (t *lanTab) disconnect() {
	t.cliMu.Lock()
	cancel := t.cliCancel
	dpm := t.dpm
	t.cliCancel = nil
	t.dpm = nil
	t.connected = false
	t.cliStatus = msg().LAN.StatusDisconnected
	t.cliMu.Unlock()

	go func() {
		if cancel != nil {
			cancel()
		}
		if dpm != nil {
			dpm.Close()
		}
	}()

	t.window.Invalidate()
}
