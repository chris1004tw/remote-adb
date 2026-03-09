package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"net"
	"net/http"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/agent"
	"github.com/chris1004tw/remote-adb/internal/daemon"
	"github.com/chris1004tw/remote-adb/internal/signal"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
)

// signalMode 是中央伺服器分頁的子模式。
type signalMode int

const (
	signalModeServer signalMode = iota
	signalModeAgent
	signalModeClient
)

// signalTab 是「中央伺服器」分頁的狀態。
type signalTab struct {
	window *app.Window
	mode   signalMode

	// 子模式按鈕
	serverModeBtn widget.Clickable
	agentModeBtn  widget.Clickable
	clientModeBtn widget.Clickable

	// --- 伺服器子模式 ---
	srvPortEditor  widget.Editor
	srvTokenEditor widget.Editor
	srvStartBtn    widget.Clickable
	srvMu          sync.Mutex
	srvRunning     bool
	srvStatus      string
	srvCancel      context.CancelFunc
	httpServer     *http.Server

	// --- Agent 子模式 ---
	agentURLEditor  widget.Editor
	agentTokenEditor widget.Editor
	agentHostEditor  widget.Editor
	agentADBEditor   widget.Editor
	agentStunEditor  widget.Editor
	agentStartBtn    widget.Clickable
	agentMu          sync.Mutex
	agentRunning     bool
	agentStatus      string
	agentDevices     []string
	agentCancel      context.CancelFunc

	// --- Client 子模式 ---
	clientURLEditor   widget.Editor
	clientTokenEditor widget.Editor
	clientStunEditor  widget.Editor
	clientPortEditor  widget.Editor
	clientConnectBtn  widget.Clickable
	clientMu          sync.Mutex
	clientRunning     bool
	clientStatus      string
	clientIPCAddr     string
	clientHosts       []protocol.HostInfo
	clientHostBtns    []widget.Clickable
	clientSelectedHost int
	clientDevBtns     []widget.Clickable
	clientBindings    []daemon.Binding
	clientUnbindBtns  []widget.Clickable
	clientCancel      context.CancelFunc
}

func newSignalTab(w *app.Window) *signalTab {
	t := &signalTab{
		window:             w,
		srvStatus:          "已停止",
		agentStatus:        "已停止",
		clientStatus:       "未連線",
		clientSelectedHost: -1,
	}
	// 伺服器子模式
	t.srvPortEditor.SingleLine = true
	t.srvPortEditor.SetText("8080")
	t.srvTokenEditor.SingleLine = true
	// Agent 子模式
	t.agentURLEditor.SingleLine = true
	t.agentURLEditor.SetText("ws://localhost:8080")
	t.agentTokenEditor.SingleLine = true
	t.agentHostEditor.SingleLine = true
	t.agentHostEditor.SetText("radb-gui")
	t.agentADBEditor.SingleLine = true
	t.agentADBEditor.SetText("5037")
	t.agentStunEditor.SingleLine = true
	t.agentStunEditor.SetText("stun:stun.l.google.com:19302")
	// Client 子模式
	t.clientURLEditor.SingleLine = true
	t.clientURLEditor.SetText("ws://localhost:8080")
	t.clientTokenEditor.SingleLine = true
	t.clientStunEditor.SingleLine = true
	t.clientStunEditor.SetText("stun:stun.l.google.com:19302")
	t.clientPortEditor.SingleLine = true
	t.clientPortEditor.SetText("15555")
	return t
}

func (t *signalTab) layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	// 處理子模式切換
	for t.serverModeBtn.Clicked(gtx) {
		t.mode = signalModeServer
	}
	for t.agentModeBtn.Clicked(gtx) {
		t.mode = signalModeAgent
	}
	for t.clientModeBtn.Clicked(gtx) {
		t.mode = signalModeClient
	}

	var children []layout.FlexChild

	// 三個子模式按鈕列
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(th, &t.serverModeBtn, "伺服器")
					if t.mode == signalModeServer {
						btn.Background = colorTabActive
					} else {
						btn.Background = colorTabInactive
					}
					return btn.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(th, &t.agentModeBtn, "Agent")
					if t.mode == signalModeAgent {
						btn.Background = colorTabActive
					} else {
						btn.Background = colorTabInactive
					}
					return btn.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(th, &t.clientModeBtn, "Client")
					if t.mode == signalModeClient {
						btn.Background = colorTabActive
					} else {
						btn.Background = colorTabInactive
					}
					return btn.Layout(gtx)
				}),
			)
		})
	}))

	// 根據子模式渲染內容
	switch t.mode {
	case signalModeServer:
		children = append(children, t.layoutServer(gtx, th)...)
	case signalModeAgent:
		children = append(children, t.layoutAgent(gtx, th)...)
	case signalModeClient:
		children = append(children, t.layoutClient(gtx, th)...)
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// === 伺服器子模式 ===

func (t *signalTab) layoutServer(gtx layout.Context, th *material.Theme) []layout.FlexChild {
	t.srvMu.Lock()
	running := t.srvRunning
	status := t.srvStatus
	t.srvMu.Unlock()

	for t.srvStartBtn.Clicked(gtx) {
		if running {
			t.stopSignalServer()
		} else {
			t.startSignalServer()
		}
	}

	return []layout.FlexChild{
		// Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Port:", &t.srvPortEditor, "8080")
			})
		}),
		// Token
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Token:", &t.srvTokenEditor, "PSK 認證 Token")
			})
		}),
		// 啟動/停止
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "啟動伺服器"
			if running {
				label = "停止伺服器"
			}
			btn := material.Button(th, &t.srvStartBtn, label)
			if running {
				btn.Background = color.NRGBA{R: 244, G: 67, B: 54, A: 255}
			}
			return btn.Layout(gtx)
		}),
		// 狀態
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			c := color.NRGBA{R: 100, G: 100, B: 100, A: 255}
			if running {
				c = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
			}
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return statusText(gtx, th, "狀態: "+status, c)
			})
		}),
	}
}

func (t *signalTab) startSignalServer() {
	port := parsePort(t.srvPortEditor.Text(), 8080)
	token := t.srvTokenEditor.Text()
	if token == "" {
		t.srvMu.Lock()
		t.srvStatus = "請輸入 Token"
		t.srvMu.Unlock()
		t.window.Invalidate()
		return
	}

	hub := signal.NewHub()
	auth := signal.NewPSKAuth(token)
	srv := signal.NewServer(hub, auth)

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	t.httpServer = &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.srvMu.Lock()
	t.srvRunning = true
	t.srvStatus = fmt.Sprintf("運行中（port %d）", port)
	t.srvCancel = cancel
	t.srvMu.Unlock()
	t.window.Invalidate()

	go func() {
		if err := t.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			t.srvMu.Lock()
			t.srvStatus = fmt.Sprintf("錯誤: %v", err)
			t.srvRunning = false
			t.srvMu.Unlock()
			t.window.Invalidate()
		}
	}()

	// 監聽 cancel 來關閉 httpServer
	go func() {
		<-ctx.Done()
		t.httpServer.Shutdown(context.Background())
	}()
}

func (t *signalTab) stopSignalServer() {
	t.srvMu.Lock()
	if t.srvCancel != nil {
		t.srvCancel()
	}
	t.srvRunning = false
	t.srvStatus = "已停止"
	t.srvMu.Unlock()
	t.window.Invalidate()
}

// === Agent 子模式 ===

func (t *signalTab) layoutAgent(gtx layout.Context, th *material.Theme) []layout.FlexChild {
	t.agentMu.Lock()
	running := t.agentRunning
	status := t.agentStatus
	devices := append([]string{}, t.agentDevices...)
	t.agentMu.Unlock()

	for t.agentStartBtn.Clicked(gtx) {
		if running {
			t.stopSignalAgent()
		} else {
			t.startSignalAgent()
		}
	}

	children := []layout.FlexChild{
		// Server URL
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Server URL:", &t.agentURLEditor, "ws://localhost:8080")
			})
		}),
		// Token
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Token:", &t.agentTokenEditor, "PSK 認證 Token")
			})
		}),
		// Host ID
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "主機名稱:", &t.agentHostEditor, "radb-gui")
			})
		}),
		// ADB Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "ADB Port:", &t.agentADBEditor, "5037")
			})
		}),
		// STUN
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "STUN:", &t.agentStunEditor, "stun:stun.l.google.com:19302")
			})
		}),
		// 啟動/停止
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "啟動 Agent"
			if running {
				label = "停止 Agent"
			}
			btn := material.Button(th, &t.agentStartBtn, label)
			if running {
				btn.Background = color.NRGBA{R: 244, G: 67, B: 54, A: 255}
			}
			return btn.Layout(gtx)
		}),
		// 狀態
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			c := color.NRGBA{R: 100, G: 100, B: 100, A: 255}
			if running {
				c = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
			}
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return statusText(gtx, th, "狀態: "+status, c)
			})
		}),
	}

	// 設備列表
	if len(devices) > 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				items := []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Body2(th, fmt.Sprintf("設備 (%d):", len(devices))).Layout(gtx)
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

func (t *signalTab) startSignalAgent() {
	url := t.agentURLEditor.Text()
	token := t.agentTokenEditor.Text()
	hostID := t.agentHostEditor.Text()
	adbPort := parsePort(t.agentADBEditor.Text(), 5037)
	iceConfig := parseICEConfig(t.agentStunEditor.Text())

	if url == "" || token == "" {
		t.agentMu.Lock()
		t.agentStatus = "請輸入 Server URL 和 Token"
		t.agentMu.Unlock()
		t.window.Invalidate()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	t.agentMu.Lock()
	t.agentRunning = true
	t.agentStatus = "檢查 ADB..."
	t.agentCancel = cancel
	t.agentMu.Unlock()
	t.window.Invalidate()

	go func() {
		adbAddr := fmt.Sprintf("127.0.0.1:%d", adbPort)
		if err := adb.EnsureADB(ctx, adbAddr, func(status string) {
			t.agentMu.Lock()
			t.agentStatus = status
			t.agentMu.Unlock()
			t.window.Invalidate()
		}); err != nil {
			t.agentMu.Lock()
			t.agentStatus = fmt.Sprintf("ADB 錯誤: %v", err)
			t.agentRunning = false
			t.agentMu.Unlock()
			t.window.Invalidate()
			return
		}

		t.agentMu.Lock()
		t.agentStatus = "連線中..."
		t.agentMu.Unlock()
		t.window.Invalidate()

		a := agent.New(agent.Config{
			ServerURL: url,
			Token:     token,
			HostID:    hostID,
			ADBAddr:   adbAddr,
			ICEConfig: iceConfig,
		})

		go t.pollAgentDevices(ctx, a)

		if err := a.Run(ctx); err != nil && ctx.Err() == nil {
			t.agentMu.Lock()
			t.agentStatus = fmt.Sprintf("錯誤: %v", err)
			t.agentRunning = false
			t.agentMu.Unlock()
			t.window.Invalidate()
			return
		}
		t.agentMu.Lock()
		if ctx.Err() == nil {
			t.agentStatus = "已斷線"
			t.agentRunning = false
		}
		t.agentMu.Unlock()
		t.window.Invalidate()
	}()
}

func (t *signalTab) pollAgentDevices(ctx context.Context, a *agent.Agent) {
	// 等待 Agent 連線完成
	time.Sleep(2 * time.Second)

	t.agentMu.Lock()
	if t.agentRunning {
		t.agentStatus = "運行中"
	}
	t.agentMu.Unlock()
	t.window.Invalidate()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		devs := a.DeviceTable().List()
		names := make([]string, len(devs))
		for i, d := range devs {
			names[i] = fmt.Sprintf("%s [%s]", d.Serial, d.State)
		}
		t.agentMu.Lock()
		t.agentDevices = names
		t.agentMu.Unlock()
		t.window.Invalidate()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *signalTab) stopSignalAgent() {
	t.agentMu.Lock()
	if t.agentCancel != nil {
		t.agentCancel()
	}
	t.agentRunning = false
	t.agentStatus = "已停止"
	t.agentDevices = nil
	t.agentMu.Unlock()
	t.window.Invalidate()
}

// === Client 子模式 ===

func (t *signalTab) layoutClient(gtx layout.Context, th *material.Theme) []layout.FlexChild {
	t.clientMu.Lock()
	running := t.clientRunning
	status := t.clientStatus
	hosts := append([]protocol.HostInfo{}, t.clientHosts...)
	selectedHost := t.clientSelectedHost
	bindings := append([]daemon.Binding{}, t.clientBindings...)
	t.clientMu.Unlock()

	// 確保按鈕 slice 長度
	totalDevBtns := 0
	for _, h := range hosts {
		totalDevBtns += len(h.Devices)
	}
	for len(t.clientHostBtns) < len(hosts) {
		t.clientHostBtns = append(t.clientHostBtns, widget.Clickable{})
	}
	for len(t.clientDevBtns) < totalDevBtns {
		t.clientDevBtns = append(t.clientDevBtns, widget.Clickable{})
	}
	for len(t.clientUnbindBtns) < len(bindings) {
		t.clientUnbindBtns = append(t.clientUnbindBtns, widget.Clickable{})
	}

	// 處理連線按鈕
	for t.clientConnectBtn.Clicked(gtx) {
		if running {
			t.stopClient()
		} else {
			t.startClient()
		}
	}

	// 處理主機點選
	for i := range hosts {
		for t.clientHostBtns[i].Clicked(gtx) {
			t.clientMu.Lock()
			if t.clientSelectedHost == i {
				t.clientSelectedHost = -1 // 再點一次收起
			} else {
				t.clientSelectedHost = i
			}
			t.clientMu.Unlock()
		}
	}

	// 處理設備 Bind 點選
	devIdx := 0
	for hi, h := range hosts {
		for di := range h.Devices {
			if devIdx < len(t.clientDevBtns) {
				for t.clientDevBtns[devIdx].Clicked(gtx) {
					t.bindDevice(hosts[hi].HostID, h.Devices[di].Serial)
				}
			}
			devIdx++
		}
	}

	// 處理 Unbind 點選
	for i := range bindings {
		if i < len(t.clientUnbindBtns) {
			for t.clientUnbindBtns[i].Clicked(gtx) {
				t.unbindDevice(bindings[i].LocalPort)
			}
		}
	}

	var children []layout.FlexChild

	// Server URL
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Server URL:", &t.clientURLEditor, "ws://localhost:8080")
		})
	}))
	// Token
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Token:", &t.clientTokenEditor, "PSK 認證 Token")
		})
	}))
	// STUN
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "STUN:", &t.clientStunEditor, "stun:stun.l.google.com:19302")
		})
	}))
	// Port 起始
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Port 起始:", &t.clientPortEditor, "15555")
		})
	}))
	// 連線/中斷按鈕
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		label := "連線到伺服器"
		if running {
			label = "中斷連線"
		}
		btn := material.Button(th, &t.clientConnectBtn, label)
		if running {
			btn.Background = color.NRGBA{R: 244, G: 67, B: 54, A: 255}
		}
		return btn.Layout(gtx)
	}))

	// 狀態
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		c := color.NRGBA{R: 100, G: 100, B: 100, A: 255}
		if running {
			c = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
		}
		return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return statusText(gtx, th, "狀態: "+status, c)
		})
	}))

	// 主機列表
	if len(hosts) > 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				items := []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Body2(th, fmt.Sprintf("主機 (%d):", len(hosts))).Layout(gtx)
					}),
				}

				devBtnIdx := 0
				for hi, h := range hosts {
					hostIdx := hi
					hostText := fmt.Sprintf("  %s (%d 設備)", h.Hostname, len(h.Devices))
					if hostIdx == selectedHost {
						hostText = "▼ " + hostText
					} else {
						hostText = "▶ " + hostText
					}

					items = append(items, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(4), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							btn := material.Button(th, &t.clientHostBtns[hostIdx], hostText)
							if hostIdx == selectedHost {
								btn.Background = colorTabActive
							} else {
								btn.Background = color.NRGBA{R: 96, G: 96, B: 96, A: 255}
							}
							return btn.Layout(gtx)
						})
					}))

					// 展開設備列表
					if hostIdx == selectedHost {
						for di, d := range h.Devices {
							dIdx := devBtnIdx + di
							devText := fmt.Sprintf("    %s [%s]", d.Serial, d.State)
							lockInfo := ""
							if d.Lock == "locked" {
								lockInfo = " (已鎖定)"
							}
							items = append(items, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									label := devText + lockInfo
									if d.Lock != "locked" && d.State == "device" {
										label = devText + " [Bind]"
									}
									if dIdx < len(t.clientDevBtns) {
										btn := material.Button(th, &t.clientDevBtns[dIdx], label)
										btn.Background = color.NRGBA{R: 70, G: 70, B: 70, A: 255}
										return btn.Layout(gtx)
									}
									return material.Body2(th, label).Layout(gtx)
								})
							}))
						}
					}
					devBtnIdx += len(h.Devices)
				}

				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, items...)
			})
		}))
	}

	// 已綁定列表
	if len(bindings) > 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				items := []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Body2(th, fmt.Sprintf("已綁定 (%d):", len(bindings))).Layout(gtx)
					}),
				}
				for i, b := range bindings {
					idx := i
					bindText := fmt.Sprintf("  127.0.0.1:%d → %s [%s] [解綁]", b.LocalPort, b.Serial, b.Status)
					items = append(items, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(4), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							if idx < len(t.clientUnbindBtns) {
								btn := material.Button(th, &t.clientUnbindBtns[idx], bindText)
								btn.Background = color.NRGBA{R: 96, G: 96, B: 96, A: 255}
								return btn.Layout(gtx)
							}
							return material.Body2(th, bindText).Layout(gtx)
						})
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, items...)
			})
		}))
	}

	return children
}

func (t *signalTab) startClient() {
	url := t.clientURLEditor.Text()
	token := t.clientTokenEditor.Text()
	stunURLs := t.clientStunEditor.Text()
	portStart := parsePort(t.clientPortEditor.Text(), 15555)

	if url == "" || token == "" {
		t.clientMu.Lock()
		t.clientStatus = "請輸入 Server URL 和 Token"
		t.clientMu.Unlock()
		t.window.Invalidate()
		return
	}

	iceConfig := parseICEConfig(stunURLs)

	cfg := daemon.Config{
		ServerURL: url,
		Token:     token,
		PortStart: portStart,
		PortEnd:   portStart + 100,
		ICEConfig: iceConfig,
	}

	d := daemon.NewDaemon(cfg)

	// 使用隨機 port 建立 IPC listener（避免和 CLI daemon 衝突）
	ipcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.clientMu.Lock()
		t.clientStatus = fmt.Sprintf("建立 IPC 失敗: %v", err)
		t.clientMu.Unlock()
		t.window.Invalidate()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	t.clientMu.Lock()
	t.clientRunning = true
	t.clientStatus = "連線中..."
	t.clientIPCAddr = ipcLn.Addr().String()
	t.clientCancel = cancel
	t.clientMu.Unlock()
	t.window.Invalidate()

	go func() {
		if err := d.Start(ctx, ipcLn); err != nil && ctx.Err() == nil {
			t.clientMu.Lock()
			t.clientStatus = fmt.Sprintf("Daemon 錯誤: %v", err)
			t.clientRunning = false
			t.clientMu.Unlock()
			t.window.Invalidate()
		}
	}()

	go t.pollClientState(ctx)
}

func (t *signalTab) pollClientState(ctx context.Context) {
	// 等待 Daemon 連線完成
	time.Sleep(2 * time.Second)

	t.clientMu.Lock()
	if t.clientRunning {
		t.clientStatus = "已連線"
	}
	t.clientMu.Unlock()
	t.window.Invalidate()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		// 查詢 hosts
		hostsResp := t.sendIPC(daemon.IPCCommand{Action: "hosts"})
		if hostsResp.Success {
			var hosts []protocol.HostInfo
			if err := json.Unmarshal(hostsResp.Data, &hosts); err == nil {
				t.clientMu.Lock()
				t.clientHosts = hosts
				t.clientHostBtns = make([]widget.Clickable, len(hosts))
				// 重計設備按鈕數量
				total := 0
				for _, h := range hosts {
					total += len(h.Devices)
				}
				t.clientDevBtns = make([]widget.Clickable, total)
				t.clientMu.Unlock()
			}
		}

		// 查詢 bindings
		listResp := t.sendIPC(daemon.IPCCommand{Action: "list"})
		if listResp.Success {
			var bindings []daemon.Binding
			if err := json.Unmarshal(listResp.Data, &bindings); err == nil {
				t.clientMu.Lock()
				t.clientBindings = bindings
				t.clientUnbindBtns = make([]widget.Clickable, len(bindings))
				t.clientMu.Unlock()
			}
		}

		t.window.Invalidate()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *signalTab) sendIPC(cmd daemon.IPCCommand) daemon.IPCResponse {
	t.clientMu.Lock()
	addr := t.clientIPCAddr
	t.clientMu.Unlock()

	if addr == "" {
		return daemon.ErrorResponse("IPC 未就緒")
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return daemon.ErrorResponse(fmt.Sprintf("IPC 連線失敗: %v", err))
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := json.NewEncoder(conn).Encode(cmd); err != nil {
		return daemon.ErrorResponse(fmt.Sprintf("IPC 發送失敗: %v", err))
	}

	var resp daemon.IPCResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return daemon.ErrorResponse(fmt.Sprintf("IPC 讀取失敗: %v", err))
	}
	return resp
}

func (t *signalTab) bindDevice(hostID, serial string) {
	go func() {
		payload, _ := json.Marshal(daemon.BindRequest{HostID: hostID, Serial: serial})
		resp := t.sendIPC(daemon.IPCCommand{Action: "bind", Payload: payload})

		t.clientMu.Lock()
		if resp.Success {
			var result daemon.BindResult
			json.Unmarshal(resp.Data, &result)
			t.clientStatus = fmt.Sprintf("綁定成功 127.0.0.1:%d → %s", result.LocalPort, result.Serial)
		} else {
			t.clientStatus = fmt.Sprintf("綁定失敗: %s", resp.Error)
		}
		t.clientMu.Unlock()
		t.window.Invalidate()
	}()
}

func (t *signalTab) unbindDevice(localPort int) {
	go func() {
		payload, _ := json.Marshal(daemon.UnbindRequest{LocalPort: localPort})
		resp := t.sendIPC(daemon.IPCCommand{Action: "unbind", Payload: payload})

		t.clientMu.Lock()
		if resp.Success {
			t.clientStatus = fmt.Sprintf("已解綁 port %d", localPort)
		} else {
			t.clientStatus = fmt.Sprintf("解綁失敗: %s", resp.Error)
		}
		t.clientMu.Unlock()
		t.window.Invalidate()
	}()
}

func (t *signalTab) stopClient() {
	t.clientMu.Lock()
	if t.clientCancel != nil {
		t.clientCancel()
	}
	t.clientRunning = false
	t.clientStatus = "未連線"
	t.clientHosts = nil
	t.clientBindings = nil
	t.clientSelectedHost = -1
	t.clientMu.Unlock()
	t.window.Invalidate()
}

func (t *signalTab) cleanup() {
	t.stopSignalServer()
	t.stopSignalAgent()
	t.stopClient()
}
