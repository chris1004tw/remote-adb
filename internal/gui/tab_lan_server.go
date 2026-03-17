// tab_lan_server.go — 區網直連分頁：被控端子模式（開啟伺服器）。
//
// 啟動 Direct TCP 服務和 mDNS 廣播，讓 LAN 上的主控端能發現並連線。
// 同時啟動 ADB 設備追蹤，定期輪詢設備列表。
package gui

import (
	"context"
	"fmt"
	"net"
	"time"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/agent"
	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/directsrv"
)

// layoutServer 繪製被控端子模式的 UI：Direct Port、Token、ADB Port + 啟動/停止按鈕。
func (t *lanTab) layoutServer(gtx layout.Context, th *material.Theme) []layout.FlexChild {
	t.srvMu.Lock()
	running := t.srvRunning
	status := t.srvStatus
	devices := append([]string{}, t.srvDevices...)
	t.srvMu.Unlock()

	for t.srvStartBtn.Clicked(gtx) {
		if running {
			t.stopServer()
		} else {
			t.startServer()
		}
	}

	var children []layout.FlexChild

	// Token
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, msg().Common.TokenLabel, &t.srvTokenEditor, msg().Common.TokenHintOptional)
		})
	}))
	// 啟動/停止按鈕
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		label := msg().Common.StartServer
		if running {
			label = msg().Common.StopServer
		}
		btn := material.Button(th, &t.srvStartBtn, label)
		if running {
			btn.Background = colorBtnStop
		}
		return btn.Layout(gtx)
	}))
	// 狀態
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		c := colorPanelHint
		if running {
			c = colorStatusOnline
		}
		return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return statusText(gtx, th, msg().Common.StatusPrefix+status, c)
		})
	}))
	// 設備列表
	if len(devices) > 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				items := []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Body2(th, fmt.Sprintf(msg().Common.DevicesFmt, len(devices))).Layout(gtx)
					}),
				}
				for _, d := range devices {
					dev := d
					items = append(items, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return material.Body2(th, dev).Layout(gtx)
						})
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, items...)
			})
		}))
	}

	return children
}

// startServer 啟動被控端服務。
// 啟動流程：
//  1. EnsureADB：偵測/下載 ADB 並確認 ADB server 可連線
//  2. 建立 Agent（僅用於 DeviceTable 和 Dialer，不連 Signal Server）
//  3. 建立 directsrv.Server（TCP 直連服務 + mDNS 廣播）
//  4. 啟動 Agent.RunDirectOnly（僅追蹤設備，不進行 WebRTC 配對）
//  5. 啟動 pollDevices 定期更新 UI 設備列表
func (t *lanTab) startServer() {
	tokenText := t.srvTokenEditor.Text()

	// 沒設 token 時自動生成臨時 token
	if tokenText == "" {
		tokenText = generateToken()
		t.srvTokenEditor.SetText(tokenText)
	}

	directPort := t.config.DirectPort
	adbPort := t.config.ADBPort

	ctx, cancel := context.WithCancel(context.Background())

	t.srvMu.Lock()
	t.srvRunning = true
	t.srvStatus = msg().Common.CheckingADB
	t.srvCancel = cancel
	t.srvMu.Unlock()
	t.window.Invalidate()

	go func() {
		adbAddr := fmt.Sprintf("127.0.0.1:%d", adbPort)
		if err := adb.EnsureADB(ctx, adbAddr, func(status string) {
			t.srvMu.Lock()
			t.srvStatus = status
			t.srvMu.Unlock()
			t.window.Invalidate()
		}); err != nil {
			t.srvMu.Lock()
			t.srvStatus = fmt.Sprintf(msg().Common.ADBErrorFmt, err)
			t.srvRunning = false
			t.srvMu.Unlock()
			t.window.Invalidate()
			return
		}

		hostname := buildinfo.Hostname()
		if hostname == "" {
			hostname = "radb-gui"
		}

		a := agent.New(agent.Config{
			ADBAddr: adbAddr,
		})

		dsrv := directsrv.New(directsrv.Config{
			DeviceTable: a.DeviceTable(),
			DialDevice: func(serial string, port int) (net.Conn, error) {
				return a.Dialer().DialDevice(serial, port)
			},
			Hostname: hostname,
			Token:    tokenText,
			ADBAddr:  adbAddr,
		})

		t.srvMu.Lock()
		t.srvStatus = fmt.Sprintf(msg().Common.RunningFmt, directPort)
		t.srvMu.Unlock()
		t.window.Invalidate()

		go func() {
			addr := fmt.Sprintf("0.0.0.0:%d", directPort)
			if err := dsrv.Serve(ctx, addr); err != nil && ctx.Err() == nil {
				t.srvMu.Lock()
				t.srvStatus = fmt.Sprintf(msg().Common.ErrorFmt, err)
				t.srvRunning = false
				t.srvMu.Unlock()
				t.window.Invalidate()
			}
		}()

		go a.RunDirectOnly(ctx)
		go t.pollDevices(ctx, a)
	}()
}

// pollDevices 每 2 秒輪詢 Agent 的 DeviceTable，更新 UI 上的設備列表。
func (t *lanTab) pollDevices(ctx context.Context, a *agent.Agent) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		devs := a.DeviceTable().List()
		names := make([]string, len(devs))
		for i, d := range devs {
			names[i] = fmt.Sprintf("%s [%s]", d.Serial, d.State)
		}
		t.srvMu.Lock()
		t.srvDevices = names
		t.srvMu.Unlock()
		t.window.Invalidate()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *lanTab) stopServer() {
	t.srvMu.Lock()
	if t.srvCancel != nil {
		t.srvCancel()
	}
	t.srvRunning = false
	t.srvStatus = msg().Common.Stopped
	t.srvDevices = nil
	t.srvMu.Unlock()
	t.window.Invalidate()
}
