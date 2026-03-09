package gui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"log/slog"
	"strings"
	"sync"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/proxy"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

// pairOffer 是 Client 端的 offer token。
type pairOffer struct {
	SDP       string `json:"sdp"`
	Serial    string `json:"serial"`
	SessionID string `json:"session_id"`
}

// pairAnswer 是 Agent 端的 answer token。
type pairAnswer struct {
	SDP string `json:"sdp"`
}

// pairTab 是 SDP 配對分頁的狀態。
type pairTab struct {
	window *app.Window

	// 角色選擇
	clientBtn widget.Clickable
	agentBtn  widget.Clickable
	isAgent   bool // false=Client, true=Agent

	// Client 模式
	serialEditor    widget.Editor
	stunEditor      widget.Editor
	localPortEditor widget.Editor
	genOfferBtn     widget.Clickable
	offerOutEditor  widget.Editor // 顯示生成的 offer（唯讀）
	answerInEditor  widget.Editor // 輸入 answer
	applyAnswerBtn  widget.Clickable

	// Agent 模式
	adbPortEditor     widget.Editor
	agentStunEditor   widget.Editor
	offerInEditor     widget.Editor // 輸入 offer
	processOfferBtn   widget.Clickable
	answerOutEditor   widget.Editor // 顯示生成的 answer（唯讀）

	// 共用狀態
	mu        sync.Mutex
	status    string
	connected bool
	cancel    context.CancelFunc
	pm        *webrtc.PeerManager
	curProxy  *proxy.Proxy
}

func newPairTab(w *app.Window) *pairTab {
	t := &pairTab{
		window: w,
		status: "未開始",
	}
	t.serialEditor.SingleLine = true
	t.stunEditor.SingleLine = true
	t.stunEditor.SetText("stun:stun.l.google.com:19302")
	t.localPortEditor.SingleLine = true
	t.localPortEditor.SetText("15555")
	t.answerInEditor.SingleLine = true

	t.adbPortEditor.SingleLine = true
	t.adbPortEditor.SetText("5037")
	t.agentStunEditor.SingleLine = true
	t.agentStunEditor.SetText("stun:stun.l.google.com:19302")
	t.offerInEditor.SingleLine = true

	t.offerOutEditor.ReadOnly = true
	t.answerOutEditor.ReadOnly = true

	return t
}

func (t *pairTab) layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	t.mu.Lock()
	isAgent := t.isAgent
	status := t.status
	connected := t.connected
	t.mu.Unlock()

	// 角色切換按鈕
	for t.clientBtn.Clicked(gtx) {
		t.mu.Lock()
		t.isAgent = false
		t.mu.Unlock()
	}
	for t.agentBtn.Clicked(gtx) {
		t.mu.Lock()
		t.isAgent = true
		t.mu.Unlock()
	}

	var children []layout.FlexChild

	// 角色選擇列
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(th, &t.clientBtn, "Client 模式")
					if !isAgent {
						btn.Background = colorTabActive
					} else {
						btn.Background = colorTabInactive
					}
					return btn.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(th, &t.agentBtn, "Agent 模式")
					if isAgent {
						btn.Background = colorTabActive
					} else {
						btn.Background = colorTabInactive
					}
					return btn.Layout(gtx)
				}),
			)
		})
	}))

	// 根據角色渲染不同內容
	if isAgent {
		children = append(children, t.layoutAgent(gtx, th)...)
	} else {
		children = append(children, t.layoutClient(gtx, th)...)
	}

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

func (t *pairTab) layoutClient(gtx layout.Context, th *material.Theme) []layout.FlexChild {
	// Client 模式按鈕事件
	for t.genOfferBtn.Clicked(gtx) {
		t.generateOffer()
	}
	for t.applyAnswerBtn.Clicked(gtx) {
		t.applyAnswer()
	}

	return []layout.FlexChild{
		// Serial
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Serial:", &t.serialEditor, "設備序號")
			})
		}),
		// STUN
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "STUN:", &t.stunEditor, "stun:stun.l.google.com:19302")
			})
		}),
		// Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "本機 Port:", &t.localPortEditor, "15555")
			})
		}),
		// 生成 Offer 按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.genOfferBtn, "生成 Offer")
				return btn.Layout(gtx)
			})
		}),
		// Offer 輸出
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if t.offerOutEditor.Text() == "" {
				return layout.Dimensions{}
			}
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Offer:", &t.offerOutEditor, "")
			})
		}),
		// Answer 輸入
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Answer:", &t.answerInEditor, "貼入 answer token")
			})
		}),
		// 套用 Answer 按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			btn := material.Button(th, &t.applyAnswerBtn, "套用 Answer")
			return btn.Layout(gtx)
		}),
	}
}

func (t *pairTab) layoutAgent(gtx layout.Context, th *material.Theme) []layout.FlexChild {
	// Agent 模式按鈕事件
	for t.processOfferBtn.Clicked(gtx) {
		t.processOffer()
	}

	return []layout.FlexChild{
		// ADB Port
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "ADB Port:", &t.adbPortEditor, "5037")
			})
		}),
		// STUN
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "STUN:", &t.agentStunEditor, "stun:stun.l.google.com:19302")
			})
		}),
		// Offer 輸入
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Offer:", &t.offerInEditor, "貼入 offer token")
			})
		}),
		// 處理 Offer 按鈕
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.processOfferBtn, "處理 Offer")
				return btn.Layout(gtx)
			})
		}),
		// Answer 輸出
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if t.answerOutEditor.Text() == "" {
				return layout.Dimensions{}
			}
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return labeledEditor(gtx, th, "Answer:", &t.answerOutEditor, "")
			})
		}),
	}
}

// --- Client 模式邏輯 ---

func (t *pairTab) generateOffer() {
	serial := t.serialEditor.Text()
	stunURLs := t.stunEditor.Text()

	if serial == "" {
		t.mu.Lock()
		t.status = "請輸入 Serial"
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	t.mu.Lock()
	t.status = "生成 Offer 中..."
	t.mu.Unlock()
	t.window.Invalidate()

	go func() {
		iceConfig := webrtc.ICEConfig{}
		if stunURLs != "" {
			iceConfig.STUNServers = strings.Split(stunURLs, ",")
		}

		pm, err := webrtc.NewPeerManager(iceConfig)
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("建立 PeerConnection 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		sessionID := fmt.Sprintf("pair-gui-%d", strings.Count(serial, ""))
		label := fmt.Sprintf("adb/%s/%s", serial, sessionID)

		if _, err := pm.OpenChannel(label); err != nil {
			pm.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf("建立 DataChannel 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		offerSDP, err := pm.CreateOffer()
		if err != nil {
			pm.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf("建立 Offer 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		offerJSON, _ := json.Marshal(pairOffer{SDP: offerSDP, Serial: serial, SessionID: sessionID})
		offerToken := base64.StdEncoding.EncodeToString(offerJSON)

		t.mu.Lock()
		t.pm = pm
		t.status = "Offer 已生成，等待 Answer"
		t.mu.Unlock()
		t.offerOutEditor.SetText(offerToken)
		t.window.Invalidate()
	}()
}

func (t *pairTab) applyAnswer() {
	answerToken := t.answerInEditor.Text()
	portText := t.localPortEditor.Text()
	serial := t.serialEditor.Text()

	t.mu.Lock()
	pm := t.pm
	t.mu.Unlock()

	if pm == nil {
		t.mu.Lock()
		t.status = "請先生成 Offer"
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	if answerToken == "" {
		t.mu.Lock()
		t.status = "請輸入 Answer token"
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	var localPort int
	fmt.Sscanf(portText, "%d", &localPort)
	if localPort == 0 {
		localPort = 15555
	}

	go func() {
		answerJSON, err := base64.StdEncoding.DecodeString(strings.TrimSpace(answerToken))
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("無效 Answer: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		var answer pairAnswer
		if err := json.Unmarshal(answerJSON, &answer); err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("解析 Answer 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		if err := pm.HandleAnswer(answer.SDP); err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("處理 Answer 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		// OpenChannel 已在 generateOffer 中呼叫，需要用另一種方式取得 channel
		// 重新 OpenChannel 會建立新的 DataChannel
		label := fmt.Sprintf("adb/%s/pair-gui-0", serial)
		channel, err := pm.OpenChannel(label)
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("DataChannel 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		p, err := proxy.New(localPort, channel)
		if err != nil {
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
		t.curProxy = p
		t.status = fmt.Sprintf("已連線 127.0.0.1:%d → %s", p.Port(), serial)
		t.mu.Unlock()
		t.window.Invalidate()
	}()
}

// --- Agent 模式邏輯 ---

func (t *pairTab) processOffer() {
	offerToken := t.offerInEditor.Text()
	adbPortText := t.adbPortEditor.Text()
	stunURLs := t.agentStunEditor.Text()

	if offerToken == "" {
		t.mu.Lock()
		t.status = "請輸入 Offer token"
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	var adbPort int
	fmt.Sscanf(adbPortText, "%d", &adbPort)
	if adbPort == 0 {
		adbPort = 5037
	}

	t.mu.Lock()
	t.status = "處理 Offer 中..."
	t.mu.Unlock()
	t.window.Invalidate()

	go func() {
		offerJSON, err := base64.StdEncoding.DecodeString(strings.TrimSpace(offerToken))
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("無效 Offer: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		var offer pairOffer
		if err := json.Unmarshal(offerJSON, &offer); err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("解析 Offer 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		iceConfig := webrtc.ICEConfig{}
		if stunURLs != "" {
			iceConfig.STUNServers = strings.Split(stunURLs, ",")
		}

		pm, err := webrtc.NewPeerManager(iceConfig)
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("建立 PeerConnection 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		dialer := adb.NewDialer(fmt.Sprintf("127.0.0.1:%d", adbPort))

		// 設定 DataChannel 處理
		pm.OnChannel(func(label string, rwc io.ReadWriteCloser) {
			go func() {
				defer rwc.Close()
				parts := strings.SplitN(label, "/", 3)
				if len(parts) < 2 || parts[0] != "adb" {
					slog.Warn("無效的 DataChannel label", "label", label)
					return
				}
				serial := parts[1]

				slog.Info("開始 ADB 轉發", "serial", serial)
				adbConn, err := dialer.DialDevice(serial, 5555)
				if err != nil {
					slog.Error("連線 ADB 設備失敗", "serial", serial, "error", err)
					return
				}
				defer adbConn.Close()

				errc := make(chan error, 2)
				go func() { _, err := io.Copy(adbConn, rwc); errc <- err }()
				go func() { _, err := io.Copy(rwc, adbConn); errc <- err }()

				select {
				case err := <-errc:
					if err != nil {
						slog.Debug("ADB 轉發結束", "error", err)
					}
				case <-ctx.Done():
				}
			}()
		})

		answerSDP, err := pm.HandleOffer(offer.SDP)
		if err != nil {
			pm.Close()
			cancel()
			t.mu.Lock()
			t.status = fmt.Sprintf("處理 Offer 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		answerJSON, _ := json.Marshal(pairAnswer{SDP: answerSDP})
		answerTokenStr := base64.StdEncoding.EncodeToString(answerJSON)

		t.mu.Lock()
		t.pm = pm
		t.cancel = cancel
		t.connected = true
		t.status = "Answer 已生成，等待對方連線"
		t.mu.Unlock()
		t.answerOutEditor.SetText(answerTokenStr)
		t.window.Invalidate()
	}()
}

func (t *pairTab) cleanup() {
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
	}
	if t.curProxy != nil {
		t.curProxy.Stop()
	}
	if t.pm != nil {
		t.pm.Close()
	}
	t.connected = false
	t.mu.Unlock()
}
