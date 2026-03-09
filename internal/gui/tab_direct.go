package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"net"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/directsrv"
	"github.com/chris1004tw/remote-adb/internal/proxy"
)

// directTab 是 Direct Connect 分頁的狀態。
type directTab struct {
	window *app.Window

	// UI 元件
	scanBtn       widget.Clickable
	addrEditor    widget.Editor
	tokenEditor   widget.Editor
	queryBtn      widget.Clickable
	portEditor    widget.Editor
	connectBtn    widget.Clickable
	serialEditor  widget.Editor

	// 掃描結果
	mu              sync.Mutex
	scanning        bool
	agents          []directsrv.DiscoveredAgent
	agentBtns       []widget.Clickable
	devices         []directsrv.DeviceInfo
	deviceBtns      []widget.Clickable
	selectedSerial  string
	connected       bool
	status          string
	cancel          context.CancelFunc
	currentProxy    *proxy.Proxy
}

func newDirectTab(w *app.Window) *directTab {
	t := &directTab{
		window: w,
		status: "未連線",
	}
	t.addrEditor.SingleLine = true
	t.tokenEditor.SingleLine = true
	t.portEditor.SingleLine = true
	t.portEditor.SetText("15555")
	t.serialEditor.SingleLine = true
	return t
}

func (t *directTab) layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	t.mu.Lock()
	scanning := t.scanning
	agents := append([]directsrv.DiscoveredAgent{}, t.agents...)
	devices := append([]directsrv.DeviceInfo{}, t.devices...)
	selectedSerial := t.selectedSerial
	connected := t.connected
	status := t.status
	t.mu.Unlock()

	// 確保按鈕 slice 長度足夠
	for len(t.agentBtns) < len(agents) {
		t.agentBtns = append(t.agentBtns, widget.Clickable{})
	}
	for len(t.deviceBtns) < len(devices) {
		t.deviceBtns = append(t.deviceBtns, widget.Clickable{})
	}

	// 處理掃描按鈕
	for t.scanBtn.Clicked(gtx) {
		if !scanning {
			t.scan()
		}
	}

	// 處理 Agent 點選
	for i := range agents {
		for t.agentBtns[i].Clicked(gtx) {
			addr := fmt.Sprintf("%s:%d", agents[i].Addr, agents[i].Port)
			t.addrEditor.SetText(addr)
		}
	}

	// 處理查詢按鈕
	for t.queryBtn.Clicked(gtx) {
		t.queryDevices()
	}

	// 處理設備點選
	for i := range devices {
		for t.deviceBtns[i].Clicked(gtx) {
			t.mu.Lock()
			t.selectedSerial = devices[i].Serial
			t.mu.Unlock()
			t.serialEditor.SetText(devices[i].Serial)
		}
	}

	// 處理連線/斷線按鈕
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
			label := "掃描 LAN"
			if scanning {
				label = "掃描中..."
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
						return material.Body2(th, fmt.Sprintf("發現 %d 個 Agent:", len(agents))).Layout(gtx)
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
			return labeledEditor(gtx, th, "Agent 地址:", &t.addrEditor, "192.168.1.100:7070")
		})
	}))

	// Token
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Token:", &t.tokenEditor, "（可選）")
		})
	}))

	// 查詢設備按鈕
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			btn := material.Button(th, &t.queryBtn, "查詢設備")
			return btn.Layout(gtx)
		})
	}))

	// 設備列表
	if len(devices) > 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				items := []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Body2(th, fmt.Sprintf("設備 (%d):", len(devices))).Layout(gtx)
					}),
				}
				for i, d := range devices {
					idx := i
					text := fmt.Sprintf("  %s [%s]", d.Serial, d.State)
					if d.Serial == selectedSerial {
						text = "▶ " + text
					}
					items = append(items, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(8), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							btn := material.Button(th, &t.deviceBtns[idx], text)
							if d.Serial == selectedSerial {
								btn.Background = color.NRGBA{R: 33, G: 150, B: 243, A: 255}
							} else {
								btn.Background = color.NRGBA{R: 96, G: 96, B: 96, A: 255}
							}
							return btn.Layout(gtx)
						})
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, items...)
			})
		}))
	}

	// Serial + Port
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Serial:", &t.serialEditor, "設備序號")
		})
	}))
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "本機 Port:", &t.portEditor, "15555")
		})
	}))

	// 連線/斷線按鈕
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		label := "連線"
		if connected {
			label = "中斷連線"
		}
		btn := material.Button(th, &t.connectBtn, label)
		if connected {
			btn.Background = color.NRGBA{R: 244, G: 67, B: 54, A: 255}
		}
		return btn.Layout(gtx)
	}))

	// 狀態
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		c := color.NRGBA{R: 100, G: 100, B: 100, A: 255}
		if connected {
			c = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
		}
		return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return statusText(gtx, th, "狀態: "+status, c)
		})
	}))

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (t *directTab) scan() {
	t.mu.Lock()
	t.scanning = true
	t.mu.Unlock()
	t.window.Invalidate()

	go func() {
		agents, _ := directsrv.DiscoverMDNS(3 * time.Second)
		t.mu.Lock()
		t.agents = agents
		t.scanning = false
		t.agentBtns = make([]widget.Clickable, len(agents))
		t.mu.Unlock()
		t.window.Invalidate()
	}()
}

func (t *directTab) queryDevices() {
	addr := t.addrEditor.Text()
	token := t.tokenEditor.Text()
	if addr == "" {
		t.mu.Lock()
		t.status = "請輸入 Agent 地址"
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	go func() {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("連線失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}
		defer conn.Close()

		conn.SetDeadline(time.Now().Add(10 * time.Second))

		if err := json.NewEncoder(conn).Encode(directsrv.Request{Action: "list", Token: token}); err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("發送失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		var resp directsrv.Response
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("讀取失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		if !resp.OK {
			t.mu.Lock()
			t.status = fmt.Sprintf("查詢失敗: %s", resp.Error)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		t.mu.Lock()
		t.devices = resp.Devices
		t.deviceBtns = make([]widget.Clickable, len(resp.Devices))
		t.status = fmt.Sprintf("查詢成功，%d 個設備", len(resp.Devices))
		t.mu.Unlock()
		t.window.Invalidate()
	}()
}

func (t *directTab) connect() {
	addr := t.addrEditor.Text()
	token := t.tokenEditor.Text()
	serial := t.serialEditor.Text()
	portText := t.portEditor.Text()

	if addr == "" || serial == "" {
		t.mu.Lock()
		t.status = "請填入 Agent 地址和 Serial"
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	var localPort int
	fmt.Sscanf(portText, "%d", &localPort)
	if localPort == 0 {
		localPort = 15555
	}

	t.mu.Lock()
	t.status = "連線中..."
	t.mu.Unlock()
	t.window.Invalidate()

	go func() {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("連線失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		if err := json.NewEncoder(conn).Encode(directsrv.Request{
			Action: "connect",
			Serial: serial,
			Token:  token,
		}); err != nil {
			conn.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf("發送失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		conn.SetDeadline(time.Now().Add(10 * time.Second))
		var resp directsrv.Response
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			conn.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf("讀取失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}
		conn.SetDeadline(time.Time{})

		if !resp.OK {
			conn.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf("設備連線失敗: %s", resp.Error)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		// 建立 TCP 代理
		p, err := proxy.New(localPort, conn)
		if err != nil {
			conn.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf("建立代理失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		p.Start(ctx)

		t.mu.Lock()
		t.connected = true
		t.cancel = cancel
		t.currentProxy = p
		t.status = fmt.Sprintf("已連線 127.0.0.1:%d → %s", p.Port(), serial)
		t.mu.Unlock()
		t.window.Invalidate()
	}()
}

func (t *directTab) disconnect() {
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
	}
	if t.currentProxy != nil {
		t.currentProxy.Stop()
		t.currentProxy = nil
	}
	t.connected = false
	t.status = "已中斷"
	t.mu.Unlock()
	t.window.Invalidate()
}

func (t *directTab) cleanup() {
	t.disconnect()
}
