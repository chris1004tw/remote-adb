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
package gui

import (
	"context"
	"fmt"
	"image/color"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/agent"
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

// --- 被控端子模式（開啟伺服器） ---
// 啟動 Direct TCP 服務和 mDNS 廣播，讓 LAN 上的主控端能發現並連線。
// 同時啟動 ADB 設備追蹤，定期輪詢設備列表。

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
			btn.Background = color.NRGBA{R: 244, G: 67, B: 54, A: 255}
		}
		return btn.Layout(gtx)
	}))
	// 狀態
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		c := colorPanelHint
		if running {
			c = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
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

		hostname, _ := os.Hostname()
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

// --- 主控端子模式（連線） ---
// 提供 mDNS 掃描 + 手動輸入 Agent 地址兩種連線方式。
// 連線後自動查詢 Agent 上的設備清單，為每個在線設備建立獨立 proxy。

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
				items := []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Body2(th, fmt.Sprintf(msg().Pair.RemoteDevFmt, len(entries))).Layout(gtx)
					}),
				}
				for _, e := range entries {
					text := fmt.Sprintf("    %s [device] → 127.0.0.1:%d", e.Serial, e.Port)
					items = append(items, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Body2(th, text)
							lbl.Color = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
							return lbl.Layout(gtx)
						})
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, items...)
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
		openCh := makeLANOpenChannel(addr, token)
		onReady, onRemoved := guiDeviceProxyCallbacks(t.window, "LAN device proxy")
		dpm := bridge.NewDeviceProxyManager(bridge.DeviceProxyConfig{
			PortStart: proxyPort,
			OpenCh:    openCh,
			ADBAddr:   fmt.Sprintf("127.0.0.1:%d", t.config.ADBPort),
			OnReady:   onReady,
			OnRemoved: onRemoved,
		})

		// 初始設備
		bridgeDevices := make([]bridge.DeviceInfo, 0, len(devices))
		for _, d := range devices {
			bridgeDevices = append(bridgeDevices, bridge.DeviceInfo{
				Serial: d.Serial, State: d.State,
			})
		}
		dpm.UpdateDevices(bridgeDevices)

		t.cliMu.Lock()
		t.connected = true
		t.cliCancel = cancel
		t.dpm = dpm
		t.cliStatus = fmt.Sprintf(msg().LAN.StatusConnectedDevFmt, len(devices))
		t.cliMu.Unlock()
		t.window.Invalidate()

		slog.Info("LAN per-device proxy started", "remote", addr, "devices", len(devices))

		// 3. 定期輪詢設備清單
		go t.pollRemoteDevices(ctx, addr, token)
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

// makeLANOpenChannel 建立 LAN 用的 bridge.OpenChannelFunc。
// 根據 label 前綴路由到不同的 directsrv action。
// 獨立函式（不依賴 lanTab），供 DeviceProxyManager 和 CLI 共用。
func makeLANOpenChannel(addr, token string) bridge.OpenChannelFunc {
	return func(label string) (io.ReadWriteCloser, error) {
		switch {
		case strings.HasPrefix(label, "adb-server/"):
			return directsrv.DialService(addr, token, "connect-server", "", "")

		case strings.HasPrefix(label, "adb-stream/"):
			parts := strings.SplitN(label, "/", 4)
			if len(parts) < 4 {
				return nil, fmt.Errorf("invalid stream label: %s", label)
			}
			conn, err := directsrv.DialService(addr, token, "connect-service", parts[2], parts[3])
			if err != nil {
				return nil, err
			}
			// setupStream 期待 ready signal（1 byte），connect-service 成功後連線已就緒
			return &bridge.PrefixedRWC{Ch: conn, Prefix: []byte{1}}, nil

		case strings.HasPrefix(label, "adb-fwd/"):
			parts := strings.SplitN(label, "/", 4)
			if len(parts) < 4 {
				return nil, fmt.Errorf("invalid fwd label: %s", label)
			}
			return directsrv.DialService(addr, token, "connect-service", parts[2], parts[3])

		default:
			return nil, fmt.Errorf("unknown channel: %s", label)
		}
	}
}

// pollRemoteDevices 定期查詢遠端設備清單並透過 DeviceProxyManager 更新 per-device proxy。
func (t *lanTab) pollRemoteDevices(ctx context.Context, addr, token string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			devices, err := t.queryDevices(addr, token)
			if err != nil {
				slog.Debug("LAN device polling failed", "error", err)
				continue
			}
			bridgeDevices := make([]bridge.DeviceInfo, 0, len(devices))
			for _, d := range devices {
				bridgeDevices = append(bridgeDevices, bridge.DeviceInfo{
					Serial: d.Serial, State: d.State,
				})
			}
			t.cliMu.Lock()
			dpm := t.dpm
			t.cliMu.Unlock()
			if dpm != nil {
				dpm.UpdateDevices(bridgeDevices)
			}
			t.window.Invalidate()
		}
	}
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

// cleanup 停止被控端服務並中斷所有主控端連線。視窗關閉時呼叫。
func (t *lanTab) cleanup() {
	t.stopServer()
	t.disconnect()
}
