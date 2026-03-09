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

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/agent"
	"github.com/chris1004tw/remote-adb/internal/directsrv"
	"github.com/chris1004tw/remote-adb/internal/proxy"
)

// lanTab 是「區網直連」分頁，包含「開啟伺服器」和「連線」兩個子模式。
type lanTab struct {
	window *app.Window

	// 子模式切換
	serverModeBtn  widget.Clickable
	connectModeBtn widget.Clickable
	isServerMode   bool

	// --- 開啟伺服器子模式（原 agentTab）---
	srvPortEditor    widget.Editor
	srvTokenEditor   widget.Editor
	srvADBPortEditor widget.Editor
	srvStartBtn      widget.Clickable

	srvMu      sync.Mutex
	srvRunning bool
	srvStatus  string
	srvDevices []string
	srvCancel  context.CancelFunc

	// --- 連線子模式（原 directTab）---
	scanBtn      widget.Clickable
	addrEditor   widget.Editor
	cliTokenEditor widget.Editor
	queryBtn     widget.Clickable
	portEditor   widget.Editor
	connectBtn   widget.Clickable
	serialEditor widget.Editor

	cliMu          sync.Mutex
	scanning       bool
	agents         []directsrv.DiscoveredAgent
	agentBtns      []widget.Clickable
	devices        []directsrv.DeviceInfo
	deviceBtns     []widget.Clickable
	selectedSerial string
	connected      bool
	cliStatus      string
	cliCancel      context.CancelFunc
	currentProxy   *proxy.Proxy
}

func newLANTab(w *app.Window) *lanTab {
	t := &lanTab{
		window:    w,
		srvStatus: "已停止",
		cliStatus: "未連線",
	}
	// 伺服器子模式預設值
	t.srvPortEditor.SingleLine = true
	t.srvPortEditor.SetText("7070")
	t.srvTokenEditor.SingleLine = true
	t.srvADBPortEditor.SingleLine = true
	t.srvADBPortEditor.SetText("5037")
	// 連線子模式預設值
	t.addrEditor.SingleLine = true
	t.cliTokenEditor.SingleLine = true
	t.portEditor.SingleLine = true
	t.portEditor.SetText("15555")
	t.serialEditor.SingleLine = true
	return t
}

func (t *lanTab) layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	// 處理子模式切換
	for t.serverModeBtn.Clicked(gtx) {
		t.isServerMode = true
	}
	for t.connectModeBtn.Clicked(gtx) {
		t.isServerMode = false
	}

	var children []layout.FlexChild

	// 子模式選擇列
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(th, &t.serverModeBtn, "開啟伺服器")
					if t.isServerMode {
						btn.Background = colorTabActive
					} else {
						btn.Background = colorTabInactive
					}
					return btn.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(th, &t.connectModeBtn, "連線")
					if !t.isServerMode {
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
	if t.isServerMode {
		children = append(children, t.layoutServer(gtx, th)...)
	} else {
		children = append(children, t.layoutConnect(gtx, th)...)
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// --- 開啟伺服器子模式 ---

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

	// Direct Port
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Direct Port:", &t.srvPortEditor, "7070")
		})
	}))
	// Token
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "Token:", &t.srvTokenEditor, "（可選）")
		})
	}))
	// ADB Port
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "ADB Port:", &t.srvADBPortEditor, "5037")
		})
	}))
	// 啟動/停止按鈕
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		label := "啟動伺服器"
		if running {
			label = "停止伺服器"
		}
		btn := material.Button(th, &t.srvStartBtn, label)
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

func (t *lanTab) startServer() {
	portText := t.srvPortEditor.Text()
	tokenText := t.srvTokenEditor.Text()
	adbPortText := t.srvADBPortEditor.Text()

	directPort := parsePort(portText, 7070)
	adbPort := parsePort(adbPortText, 5037)

	ctx, cancel := context.WithCancel(context.Background())

	t.srvMu.Lock()
	t.srvRunning = true
	t.srvStatus = "檢查 ADB..."
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
			t.srvStatus = fmt.Sprintf("ADB 錯誤: %v", err)
			t.srvRunning = false
			t.srvMu.Unlock()
			t.window.Invalidate()
			return
		}

		hostname := "radb-gui"

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
		})

		t.srvMu.Lock()
		t.srvStatus = fmt.Sprintf("運行中（port %d）", directPort)
		t.srvMu.Unlock()
		t.window.Invalidate()

		go func() {
			addr := fmt.Sprintf("0.0.0.0:%d", directPort)
			if err := dsrv.Serve(ctx, addr); err != nil && ctx.Err() == nil {
				t.srvMu.Lock()
				t.srvStatus = fmt.Sprintf("錯誤: %v", err)
				t.srvRunning = false
				t.srvMu.Unlock()
				t.window.Invalidate()
			}
		}()

		go a.RunDirectOnly(ctx)
		go t.pollDevices(ctx, a)
	}()
}

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
	t.srvStatus = "已停止"
	t.srvDevices = nil
	t.srvMu.Unlock()
	t.window.Invalidate()
}

// --- 連線子模式 ---

func (t *lanTab) layoutConnect(gtx layout.Context, th *material.Theme) []layout.FlexChild {
	t.cliMu.Lock()
	scanning := t.scanning
	agents := append([]directsrv.DiscoveredAgent{}, t.agents...)
	devices := append([]directsrv.DeviceInfo{}, t.devices...)
	selectedSerial := t.selectedSerial
	connected := t.connected
	status := t.cliStatus
	t.cliMu.Unlock()

	for len(t.agentBtns) < len(agents) {
		t.agentBtns = append(t.agentBtns, widget.Clickable{})
	}
	for len(t.deviceBtns) < len(devices) {
		t.deviceBtns = append(t.deviceBtns, widget.Clickable{})
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
		}
	}
	for t.queryBtn.Clicked(gtx) {
		t.queryDevices()
	}
	for i := range devices {
		for t.deviceBtns[i].Clicked(gtx) {
			t.cliMu.Lock()
			t.selectedSerial = devices[i].Serial
			t.cliMu.Unlock()
			t.serialEditor.SetText(devices[i].Serial)
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
			return labeledEditor(gtx, th, "Token:", &t.cliTokenEditor, "（可選）")
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

	// 設備序號 + Port
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "設備序號:", &t.serialEditor, "如 emulator-5554")
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

	return children
}

func (t *lanTab) scan() {
	t.cliMu.Lock()
	t.scanning = true
	t.cliMu.Unlock()
	t.window.Invalidate()

	go func() {
		agents, _ := directsrv.DiscoverMDNS(3 * time.Second)
		t.cliMu.Lock()
		t.agents = agents
		t.scanning = false
		t.agentBtns = make([]widget.Clickable, len(agents))
		t.cliMu.Unlock()
		t.window.Invalidate()
	}()
}

func (t *lanTab) queryDevices() {
	addr := t.addrEditor.Text()
	token := t.cliTokenEditor.Text()
	if addr == "" {
		t.cliMu.Lock()
		t.cliStatus = "請輸入 Agent 地址"
		t.cliMu.Unlock()
		t.window.Invalidate()
		return
	}

	go func() {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.cliMu.Lock()
			t.cliStatus = fmt.Sprintf("連線失敗: %v", err)
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}
		defer conn.Close()

		conn.SetDeadline(time.Now().Add(10 * time.Second))

		if err := json.NewEncoder(conn).Encode(directsrv.Request{Action: "list", Token: token}); err != nil {
			t.cliMu.Lock()
			t.cliStatus = fmt.Sprintf("發送失敗: %v", err)
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}

		var resp directsrv.Response
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			t.cliMu.Lock()
			t.cliStatus = fmt.Sprintf("讀取失敗: %v", err)
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}

		if !resp.OK {
			t.cliMu.Lock()
			t.cliStatus = fmt.Sprintf("查詢失敗: %s", resp.Error)
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}

		t.cliMu.Lock()
		t.devices = resp.Devices
		t.deviceBtns = make([]widget.Clickable, len(resp.Devices))
		t.cliStatus = fmt.Sprintf("查詢成功，%d 個設備", len(resp.Devices))
		t.cliMu.Unlock()
		t.window.Invalidate()
	}()
}

func (t *lanTab) connect() {
	addr := t.addrEditor.Text()
	token := t.cliTokenEditor.Text()
	serial := t.serialEditor.Text()
	portText := t.portEditor.Text()

	if addr == "" || serial == "" {
		t.cliMu.Lock()
		t.cliStatus = "請填入 Agent 地址和設備序號"
		t.cliMu.Unlock()
		t.window.Invalidate()
		return
	}

	localPort := parsePort(portText, 15555)

	t.cliMu.Lock()
	t.cliStatus = "連線中..."
	t.cliMu.Unlock()
	t.window.Invalidate()

	go func() {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.cliMu.Lock()
			t.cliStatus = fmt.Sprintf("連線失敗: %v", err)
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}

		if err := json.NewEncoder(conn).Encode(directsrv.Request{
			Action: "connect",
			Serial: serial,
			Token:  token,
		}); err != nil {
			conn.Close()
			t.cliMu.Lock()
			t.cliStatus = fmt.Sprintf("發送失敗: %v", err)
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}

		conn.SetDeadline(time.Now().Add(10 * time.Second))
		var resp directsrv.Response
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			conn.Close()
			t.cliMu.Lock()
			t.cliStatus = fmt.Sprintf("讀取失敗: %v", err)
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}
		conn.SetDeadline(time.Time{})

		if !resp.OK {
			conn.Close()
			t.cliMu.Lock()
			t.cliStatus = fmt.Sprintf("設備連線失敗: %s", resp.Error)
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}

		p, err := proxy.New(localPort, conn)
		if err != nil {
			conn.Close()
			t.cliMu.Lock()
			t.cliStatus = fmt.Sprintf("建立代理失敗: %v", err)
			t.cliMu.Unlock()
			t.window.Invalidate()
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		p.Start(ctx)

		t.cliMu.Lock()
		t.connected = true
		t.cliCancel = cancel
		t.currentProxy = p
		t.cliStatus = fmt.Sprintf("已連線 127.0.0.1:%d → %s", p.Port(), serial)
		t.cliMu.Unlock()
		t.window.Invalidate()
	}()
}

func (t *lanTab) disconnect() {
	t.cliMu.Lock()
	if t.cliCancel != nil {
		t.cliCancel()
	}
	if t.currentProxy != nil {
		t.currentProxy.Stop()
		t.currentProxy = nil
	}
	t.connected = false
	t.cliStatus = "已中斷"
	t.cliMu.Unlock()
	t.window.Invalidate()
}

func (t *lanTab) cleanup() {
	t.stopServer()
	t.disconnect()
}
