// tab_signal.go 實作「Relay 伺服器」分頁的 GUI 與邏輯。
//
// 本分頁提供三個子模式（透過頂部按鈕列切換）：
//
//  1. 伺服器子模式（signalModeServer）：在本機啟動 WebSocket Signaling Server，
//     供 Agent 和 Client 透過 PSK Token 認證連線。
//
//  2. 被控端子模式（signalModeAgent）：連線到 Signaling Server，將本機 ADB 設備
//     註冊為可用設備。主控端可透過伺服器中介建立 WebRTC P2P 通道。
//     包含 ADB 自動偵測/下載功能（EnsureADB）。
//
//  3. 主控端子模式（signalModeClient）：連線到 Signaling Server，瀏覽遠端主機的
//     設備列表，執行 Bind/Unbind 操作將遠端設備轉發到本機 port。
//     使用 Daemon IPC 協定（JSON over TCP）與內建 Daemon 溝通。
//
// 每個子模式使用獨立的 sync.Mutex 保護其狀態（UI 線程與背景 goroutine 並行存取）。
package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"log/slog"
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
	"github.com/chris1004tw/remote-adb/internal/buildinfo"
	"github.com/chris1004tw/remote-adb/internal/daemon"
	"github.com/chris1004tw/remote-adb/internal/signal"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
)

// signalMode 是「Relay 伺服器」分頁的子模式列舉。
// 三種模式各自獨立運作：伺服器/被控端/主控端，使用者一次只選一種。
type signalMode int

const (
	signalModeServer signalMode = iota // 啟動 Signaling Server
	signalModeAgent                    // 作為 Agent（被控端）連線到 Server
	signalModeClient                   // 作為 Client（主控端）連線到 Server
)

// signalTab 是「Relay 伺服器」分頁的完整狀態，包含三個子模式的所有 UI 元件與背景邏輯。
// 每個子模式使用獨立的 Mutex（srvMu/agentMu/clientMu）保護並行存取安全。
// STUN/TURN 設定來自全域 AppConfig（設定面板管理），不在各子模式中重複輸入。
type signalTab struct {
	window *app.Window
	config *AppConfig  // 全域設定（STUN/TURN 等共用設定）
	tc     *turnCache  // TURN 憑證快取（啟動時預先取得）
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
	agentURLEditor   widget.Editor
	agentTokenEditor widget.Editor
	agentHostEditor  widget.Editor
	agentADBEditor   widget.Editor
	agentStartBtn    widget.Clickable
	agentMu          sync.Mutex
	agentRunning     bool
	agentStatus      string
	agentDevices     []string
	agentCancel      context.CancelFunc

	// --- Client 子模式 ---
	clientURLEditor   widget.Editor
	clientTokenEditor widget.Editor
	clientPortEditor  widget.Editor      // ADB proxy 的起始 port
	clientConnectBtn  widget.Clickable
	clientMu          sync.Mutex
	clientRunning     bool
	clientStatus      string
	clientIPCAddr     string              // IPC listener 位址（隨機 port，避免和 CLI daemon 衝突）
	clientHosts       []protocol.HostInfo // 從 Daemon 查詢到的遠端主機列表
	clientHostBtns    []widget.Clickable  // 每個主機的展開/收起按鈕
	clientSelectedHost int               // 目前展開的主機索引（-1 表示全部收起）
	clientDevBtns     []widget.Clickable  // 每個設備的 Bind 按鈕
	clientBindings    []daemon.Binding    // 目前已綁定的設備（本機 port → 遠端設備）
	clientUnbindBtns  []widget.Clickable  // 每個綁定的 Unbind 按鈕
	clientCancel      context.CancelFunc
}

// newSignalTab 建立並初始化 signalTab，設定各輸入框的預設值。
// config 為全域設定（STUN/TURN 等共用設定），由設定面板管理。
func newSignalTab(w *app.Window, config *AppConfig, tc *turnCache) *signalTab {
	t := &signalTab{
		window:             w,
		config:             config,
		tc:                 tc,
		srvStatus:          msg().Common.Stopped,
		agentStatus:        msg().Common.Stopped,
		clientStatus:       msg().Common.Disconnected,
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
	if h := buildinfo.Hostname(); h != "" {
		t.agentHostEditor.SetText(h)
	} else {
		t.agentHostEditor.SetText("radb-gui")
	}
	t.agentADBEditor.SingleLine = true
	t.agentADBEditor.SetText("5037")
	// Client 子模式
	t.clientURLEditor.SingleLine = true
	t.clientURLEditor.SetText("ws://localhost:8080")
	t.clientTokenEditor.SingleLine = true
	t.clientPortEditor.SingleLine = true
	t.clientPortEditor.SetText("5555")
	return t
}

// layout 繪製分頁內容：頂部三個子模式切換按鈕 + 根據目前模式渲染對應的設定/狀態區域。
// 子模式切換是即時的，不會影響已啟動的服務。
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

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// 子模式按鈕列（全寬，與主分頁對齊）
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(th, &t.serverModeBtn, msg().Signal.Server)
						if t.mode == signalModeServer {
							btn.Background = colorModeActive
						} else {
							btn.Background = colorModeInactive
						}
						return btn.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(th, &t.clientModeBtn, msg().Common.Controller)
						if t.mode == signalModeClient {
							btn.Background = colorModeActive
						} else {
							btn.Background = colorModeInactive
						}
						return btn.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(th, &t.agentModeBtn, msg().Common.Agent)
						if t.mode == signalModeAgent {
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
				switch t.mode {
				case signalModeServer:
					children = append(children, t.layoutServer(gtx, th)...)
				case signalModeAgent:
					children = append(children, t.layoutAgent(gtx, th)...)
				case signalModeClient:
					children = append(children, t.layoutClient(gtx, th)...)
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
			})
		}),
	)
}

// === 伺服器子模式 ===
// 在本機啟動 HTTP + WebSocket Signaling Server，供 Agent/Client 連線。
// Token 用於 PSK 認證，所有透過此伺服器溝通的 Agent/Client 必須使用相同 Token。

// layoutServer 繪製伺服器子模式的 UI：Port 輸入、Token 輸入、啟動/停止按鈕、狀態。
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
				return labeledEditor(gtx, th, msg().Common.TokenLabel, &t.srvTokenEditor, msg().Common.TokenHintPSK)
			})
		}),
		// 啟動/停止
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := msg().Common.StartServer
			if running {
				label = msg().Common.StopServer
			}
			btn := material.Button(th, &t.srvStartBtn, label)
			if running {
				btn.Background = colorBtnStop
			}
			return btn.Layout(gtx)
		}),
		// 狀態
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			c := colorPanelHint
			if running {
				c = colorStatusOnline
			}
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return statusText(gtx, th, msg().Common.StatusPrefix+status, c)
			})
		}),
	}
}

// startSignalServer 啟動 Signaling Server。
// 建立 signal.Hub（管理連線）和 PSK 認證，在背景 goroutine 中啟動 HTTP 伺服器。
// 使用 context 控制生命週期，stopSignalServer() 會觸發 cancel 來優雅關閉。
func (t *signalTab) startSignalServer() {
	port := parsePort(t.srvPortEditor.Text(), 8080)
	token := t.srvTokenEditor.Text()
	if token == "" {
		t.srvMu.Lock()
		t.srvStatus = msg().Signal.StatusPleaseToken
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
	t.srvStatus = fmt.Sprintf(msg().Common.RunningFmt, port)
	t.srvCancel = cancel
	t.srvMu.Unlock()
	t.window.Invalidate()

	go func() {
		if err := t.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			t.srvMu.Lock()
			t.srvStatus = fmt.Sprintf(msg().Common.ErrorFmt, err)
			t.srvRunning = false
			t.srvMu.Unlock()
			t.window.Invalidate()
		}
	}()

	// 監聽 cancel 來關閉 httpServer（5 秒逾時保護，避免 Shutdown 永久阻塞）
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		t.httpServer.Shutdown(shutdownCtx)
	}()
}

func (t *signalTab) stopSignalServer() {
	t.srvMu.Lock()
	if t.srvCancel != nil {
		t.srvCancel()
	}
	t.srvRunning = false
	t.srvStatus = msg().Common.Stopped
	t.srvMu.Unlock()
	t.window.Invalidate()
}

// === Agent 子模式（被控端） ===
// 連線到 Signaling Server，將本機 ADB 上的 Android 設備註冊為可供遠端使用。
// 啟動流程：EnsureADB（自動偵測/下載 ADB）→ 建立 Agent → 連線到 Server → 輪詢設備。

// layoutAgent 繪製被控端子模式的 UI：Server URL、Token、主機名稱、ADB Port、STUN 設定。
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
				return labeledEditor(gtx, th, msg().Common.TokenLabel, &t.agentTokenEditor, msg().Common.TokenHintPSK)
			})
		}),
		// Host ID
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, msg().Signal.HostnameLabel, &t.agentHostEditor, msg().Signal.HostnameHint)
			})
		}),
		// ADB Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "ADB Port:", &t.agentADBEditor, "5037")
			})
		}),
		// 啟動/停止
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := msg().Signal.StartAgent
			if running {
				label = msg().Signal.StopAgent
			}
			btn := material.Button(th, &t.agentStartBtn, label)
			if running {
				btn.Background = colorBtnStop
			}
			return btn.Layout(gtx)
		}),
		// 狀態
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			c := colorPanelHint
			if running {
				c = colorStatusOnline
			}
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return statusText(gtx, th, msg().Common.StatusPrefix+status, c)
			})
		}),
	}

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

// startSignalAgent 啟動被控端 Agent。
// 啟動流程：驗證輸入 → EnsureADB → 建立 Agent → Agent.Run（阻塞）。
// 並行啟動 pollAgentDevices 每 2 秒輪詢設備列表更新 UI。
func (t *signalTab) startSignalAgent() {
	url := t.agentURLEditor.Text()
	token := t.agentTokenEditor.Text()
	hostID := t.agentHostEditor.Text()
	adbPort := parsePort(t.agentADBEditor.Text(), 5037)

	if url == "" || token == "" {
		t.agentMu.Lock()
		t.agentStatus = msg().Signal.StatusPleaseURLToken
		t.agentMu.Unlock()
		t.window.Invalidate()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	t.agentMu.Lock()
	t.agentRunning = true
	t.agentStatus = msg().Common.CheckingADB
	t.agentCancel = cancel
	t.agentMu.Unlock()
	t.window.Invalidate()

	go func() {
		iceConfig, turnWarn := resolveICEWithTURN(t.config, t.tc, 2*time.Second)
		if turnWarn != "" {
			slog.Warn("Cloudflare TURN unavailable for agent", "warning", turnWarn)
		}

		adbAddr := fmt.Sprintf("127.0.0.1:%d", adbPort)
		if err := adb.EnsureADB(ctx, adbAddr, func(status string) {
			t.agentMu.Lock()
			t.agentStatus = status
			t.agentMu.Unlock()
			t.window.Invalidate()
		}); err != nil {
			t.agentMu.Lock()
			t.agentStatus = fmt.Sprintf(msg().Common.ADBErrorFmt, err)
			t.agentRunning = false
			t.agentMu.Unlock()
			t.window.Invalidate()
			return
		}

		t.agentMu.Lock()
		t.agentStatus = msg().Common.Connecting
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
			t.agentStatus = fmt.Sprintf(msg().Common.ErrorFmt, err)
			t.agentRunning = false
			t.agentMu.Unlock()
			t.window.Invalidate()
			return
		}
		t.agentMu.Lock()
		if ctx.Err() == nil {
			t.agentStatus = msg().Signal.StatusDisconnected
			t.agentRunning = false
		}
		t.agentMu.Unlock()
		t.window.Invalidate()
	}()
}

// pollAgentDevices 定期輪詢 Agent 的 DeviceTable，更新 UI 上的設備清單。
// 先等待 2 秒讓 Agent 完成 WebSocket 握手，之後每 2 秒查詢一次直到 ctx 取消。
func (t *signalTab) pollAgentDevices(ctx context.Context, a *agent.Agent) {
	// 等待 Agent 連線完成
	time.Sleep(2 * time.Second)

	t.agentMu.Lock()
	if t.agentRunning {
		t.agentStatus = msg().Signal.StatusRunning
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
	t.agentStatus = msg().Common.Stopped
	t.agentDevices = nil
	t.agentMu.Unlock()
	t.window.Invalidate()
}

// === Client 子模式（主控端） ===
// 連線到 Signaling Server 後啟動內建 Daemon，透過 IPC 查詢主機列表、
// 執行 Bind（將遠端設備轉發到本機 port）/ Unbind 操作。
// pollClientState 負責定期透過 IPC 更新 UI 上的主機和綁定資訊。

// layoutClient 繪製主控端子模式的 UI。
// 包含：Server URL、Token、STUN、ADB Port 輸入 + 連線按鈕 + 主機/設備/綁定列表。
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
			return labeledEditor(gtx, th, msg().Common.TokenLabel, &t.clientTokenEditor, msg().Common.TokenHintPSK)
		})
	}))
	// Port 起始
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return labeledEditor(gtx, th, "ADB Port:", &t.clientPortEditor, "5555")
		})
	}))
	// 連線/中斷按鈕
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		label := msg().Signal.ConnectServer
		if running {
			label = msg().Common.DisconnectBtn
		}
		btn := material.Button(th, &t.clientConnectBtn, label)
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

	// 主機列表
	if len(hosts) > 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.layoutHostList(gtx, th, hosts, selectedHost)
		}))
	}

	// 已綁定列表
	if len(bindings) > 0 {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.layoutBindingList(gtx, th, bindings)
		}))
	}

	return children
}

// layoutHostList 繪製主機及設備列表。
// 每台主機顯示為可展開按鈕，點擊後展開該主機的設備列表（含 Bind/鎖定狀態）。
func (t *signalTab) layoutHostList(gtx layout.Context, th *material.Theme, hosts []protocol.HostInfo, selectedHost int) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		items := []layout.FlexChild{
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return material.Body2(th, fmt.Sprintf(msg().Signal.HostsFmt, len(hosts))).Layout(gtx)
			}),
		}

		devBtnIdx := 0
		for hi, h := range hosts {
			hostIdx := hi
			hostText := fmt.Sprintf("  %s ("+msg().Signal.HostDevFmt+")", h.Hostname, len(h.Devices))
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
						lockInfo = " " + msg().Signal.Locked
					}
					items = append(items, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							label := devText + lockInfo
							if d.Lock != "locked" && d.State == "device" {
								label = devText + " " + msg().Signal.BindLabel
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
}

// layoutBindingList 繪製已綁定設備列表。
// 每筆 binding 顯示本機 port → 遠端 serial + 狀態 + Unbind 按鈕。
func (t *signalTab) layoutBindingList(gtx layout.Context, th *material.Theme, bindings []daemon.Binding) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		items := []layout.FlexChild{
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return material.Body2(th, fmt.Sprintf(msg().Signal.BindingsFmt, len(bindings))).Layout(gtx)
			}),
		}
		for i, b := range bindings {
			idx := i
			bindText := fmt.Sprintf("  127.0.0.1:%d → %s [%s] %s", b.LocalPort, b.Serial, b.Status, msg().Signal.UnbindLabel)
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
}

// startClient 啟動主控端 Daemon。
// 建立 Daemon 物件 + 隨機 port 的 IPC listener（避免和 CLI daemon 衝突），
// 在背景啟動 Daemon 並開始 pollClientState 輪詢。
func (t *signalTab) startClient() {
	url := t.clientURLEditor.Text()
	token := t.clientTokenEditor.Text()
	portStart := parsePort(t.clientPortEditor.Text(), 5555)

	if url == "" || token == "" {
		t.clientMu.Lock()
		t.clientStatus = msg().Signal.StatusPleaseURLToken
		t.clientMu.Unlock()
		t.window.Invalidate()
		return
	}

	// 使用隨機 port 建立 IPC listener（避免和 CLI daemon 衝突）
	ipcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.clientMu.Lock()
		t.clientStatus = fmt.Sprintf(msg().Signal.ErrIPCFmt, err)
		t.clientMu.Unlock()
		t.window.Invalidate()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	t.clientMu.Lock()
	t.clientRunning = true
	t.clientStatus = msg().Common.Connecting
	t.clientIPCAddr = ipcLn.Addr().String()
	t.clientCancel = cancel
	t.clientMu.Unlock()
	t.window.Invalidate()

	go func() {
		iceConfig, turnWarn := resolveICEWithTURN(t.config, t.tc, 2*time.Second)
		if turnWarn != "" {
			slog.Warn("Cloudflare TURN unavailable for daemon", "warning", turnWarn)
		}

		cfg := daemon.Config{
			ServerURL: url,
			Token:     token,
			PortStart: portStart,
			PortEnd:   portStart + 100,
			ICEConfig: iceConfig,
		}

		d := daemon.NewDaemon(cfg)

		if err := d.Start(ctx, ipcLn); err != nil && ctx.Err() == nil {
			ipcLn.Close() // d.Start 失敗時 ServeIPC 未接管 listener，須手動關閉
			cancel()      // 停止 pollClientState goroutine
			t.clientMu.Lock()
			t.clientStatus = fmt.Sprintf(msg().Signal.ErrDaemonFmt, err)
			t.clientRunning = false
			t.clientMu.Unlock()
			t.window.Invalidate()
		}
	}()

	go t.pollClientState(ctx)
}

// pollClientState 定期透過 IPC 查詢 Daemon 狀態並更新 UI。
// 每 3 秒查詢一次，包含兩個 IPC 請求：
//   - "hosts"：取得遠端主機及其設備列表（包含 HostID、Hostname、設備序號/狀態/鎖定資訊）
//   - "list"：取得目前已綁定的 port → 設備映射列表
//
// 查詢結果會更新 clientHosts、clientBindings 及對應的 UI 按鈕 slice。
func (t *signalTab) pollClientState(ctx context.Context) {
	// 等待 Daemon 連線完成
	time.Sleep(2 * time.Second)

	t.clientMu.Lock()
	if t.clientRunning {
		t.clientStatus = msg().Signal.StatusConnected
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

// sendIPC 向內建 Daemon 發送 IPC 命令並等待回應。
// 連線逾時 5 秒，命令收發委派給 daemon.SendCommand（共用邏輯，讀寫逾時 30 秒）。
func (t *signalTab) sendIPC(cmd daemon.IPCCommand) daemon.IPCResponse {
	t.clientMu.Lock()
	addr := t.clientIPCAddr
	t.clientMu.Unlock()

	if addr == "" {
		return daemon.ErrorResponse(msg().Signal.ErrIPCNotReady)
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return daemon.ErrorResponse(fmt.Sprintf("IPC connect failed: %v", err))
	}
	defer conn.Close()

	resp, err := daemon.SendCommand(conn, cmd)
	if err != nil {
		return daemon.ErrorResponse(err.Error())
	}
	return resp
}

// bindDevice 透過 IPC 發送 bind 命令，將遠端設備綁定到本機 port。
// 成功後 Daemon 會建立 WebRTC DataChannel + TCP proxy。
func (t *signalTab) bindDevice(hostID, serial string) {
	go func() {
		payload, _ := json.Marshal(daemon.BindRequest{HostID: hostID, Serial: serial})
		resp := t.sendIPC(daemon.IPCCommand{Action: "bind", Payload: payload})

		t.clientMu.Lock()
		if resp.Success {
			var result daemon.BindResult
			if err := json.Unmarshal(resp.Data, &result); err != nil {
				t.clientStatus = fmt.Sprintf(msg().Signal.StatusBindDecodeFailFmt, err)
			} else {
				t.clientStatus = fmt.Sprintf(msg().Signal.StatusBindOKFmt, result.LocalPort, result.Serial)
			}
		} else {
			t.clientStatus = fmt.Sprintf(msg().Signal.StatusBindFailFmt, resp.Error)
		}
		t.clientMu.Unlock()
		t.window.Invalidate()
	}()
}

// unbindDevice 透過 IPC 發送 unbind 命令，解除本機 port 的設備綁定。
func (t *signalTab) unbindDevice(localPort int) {
	go func() {
		payload, _ := json.Marshal(daemon.UnbindRequest{LocalPort: localPort})
		resp := t.sendIPC(daemon.IPCCommand{Action: "unbind", Payload: payload})

		t.clientMu.Lock()
		if resp.Success {
			t.clientStatus = fmt.Sprintf(msg().Signal.StatusUnbindOKFmt, localPort)
		} else {
			t.clientStatus = fmt.Sprintf(msg().Signal.StatusUnbindFailFmt, resp.Error)
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
	t.clientStatus = msg().Common.Disconnected
	t.clientHosts = nil
	t.clientBindings = nil
	t.clientSelectedHost = -1
	t.clientMu.Unlock()
	t.window.Invalidate()
}

// cleanup 停止所有子模式的服務，釋放資源。視窗關閉時呼叫。
func (t *signalTab) cleanup() {
	t.stopSignalServer()
	t.stopSignalAgent()
	t.stopClient()
}
