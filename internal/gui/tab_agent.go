package gui

import (
	"context"
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

	"github.com/chris1004tw/remote-adb/internal/agent"
	"github.com/chris1004tw/remote-adb/internal/directsrv"
)

// agentTab 是 Agent 分頁的狀態。
type agentTab struct {
	window *app.Window

	portEditor    widget.Editor
	tokenEditor   widget.Editor
	adbPortEditor widget.Editor
	startBtn      widget.Clickable

	mu      sync.Mutex
	running bool
	status  string
	devices []string
	cancel  context.CancelFunc
}

func newAgentTab(w *app.Window) *agentTab {
	t := &agentTab{
		window: w,
		status: "已停止",
	}
	t.portEditor.SingleLine = true
	t.portEditor.SetText("7070")
	t.tokenEditor.SingleLine = true
	t.adbPortEditor.SingleLine = true
	t.adbPortEditor.SetText("5037")
	return t
}

func (t *agentTab) layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	t.mu.Lock()
	running := t.running
	status := t.status
	devices := append([]string{}, t.devices...)
	t.mu.Unlock()

	// 處理按鈕點擊
	for t.startBtn.Clicked(gtx) {
		if running {
			t.stop()
		} else {
			t.start()
		}
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// Direct Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Direct Port:", &t.portEditor, "7070")
			})
		}),

		// Direct Token
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Token:", &t.tokenEditor, "（可選）")
			})
		}),

		// ADB Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "ADB Port:", &t.adbPortEditor, "5037")
			})
		}),

		// 啟動/停止按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "啟動 Agent"
			if running {
				label = "停止 Agent"
			}
			btn := material.Button(th, &t.startBtn, label)
			if running {
				btn.Background = color.NRGBA{R: 244, G: 67, B: 54, A: 255} // 紅色
			}
			return btn.Layout(gtx)
		}),

		// 狀態
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			c := color.NRGBA{R: 100, G: 100, B: 100, A: 255}
			if running {
				c = color.NRGBA{R: 76, G: 175, B: 80, A: 255} // 綠色
			}
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return statusText(gtx, th, "狀態: "+status, c)
			})
		}),

		// 設備列表
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if len(devices) == 0 {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				children := []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(th, fmt.Sprintf("設備 (%d):", len(devices)))
						return lbl.Layout(gtx)
					}),
				}
				for _, d := range devices {
					dev := d
					children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return material.Body2(th, dev).Layout(gtx)
						})
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
			})
		}),
	)
}

func (t *agentTab) start() {
	portText := t.portEditor.Text()
	tokenText := t.tokenEditor.Text()
	adbPortText := t.adbPortEditor.Text()

	var directPort int
	fmt.Sscanf(portText, "%d", &directPort)
	if directPort == 0 {
		directPort = 7070
	}

	var adbPort int
	fmt.Sscanf(adbPortText, "%d", &adbPort)
	if adbPort == 0 {
		adbPort = 5037
	}

	ctx, cancel := context.WithCancel(context.Background())

	t.mu.Lock()
	t.running = true
	t.status = "啟動中..."
	t.cancel = cancel
	t.mu.Unlock()
	t.window.Invalidate()

	hostname := "radb-gui"

	a := agent.New(agent.Config{
		ADBAddr: fmt.Sprintf("127.0.0.1:%d", adbPort),
	})

	dsrv := directsrv.New(directsrv.Config{
		DeviceTable: a.DeviceTable(),
		DialDevice: func(serial string, port int) (net.Conn, error) {
			return a.Dialer().DialDevice(serial, port)
		},
		Hostname: hostname,
		Token:    tokenText,
	})

	// 啟動 Direct Server
	go func() {
		addr := fmt.Sprintf("0.0.0.0:%d", directPort)
		t.mu.Lock()
		t.status = fmt.Sprintf("運行中（port %d）", directPort)
		t.mu.Unlock()
		t.window.Invalidate()

		if err := dsrv.Serve(ctx, addr); err != nil && ctx.Err() == nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("錯誤: %v", err)
			t.running = false
			t.mu.Unlock()
			t.window.Invalidate()
		}
	}()

	// 啟動 Agent（僅追蹤設備）
	go func() {
		a.RunDirectOnly(ctx)
	}()

	// 定期更新設備列表
	go t.pollDevices(ctx, a)
}

func (t *agentTab) pollDevices(ctx context.Context, a *agent.Agent) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		devs := a.DeviceTable().List()
		names := make([]string, len(devs))
		for i, d := range devs {
			names[i] = fmt.Sprintf("%s [%s]", d.Serial, d.State)
		}
		t.mu.Lock()
		t.devices = names
		t.mu.Unlock()
		t.window.Invalidate()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *agentTab) stop() {
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
	}
	t.running = false
	t.status = "已停止"
	t.devices = nil
	t.mu.Unlock()
	t.window.Invalidate()
}

func (t *agentTab) cleanup() {
	t.stop()
}
