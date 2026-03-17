// tab_pair_client.go 實作「簡易連線」分頁的主控端（客戶端）模式 UI 與邏輯。

package gui

import (
	"context"
	"fmt"
	"image/color"
	"io"
	"log/slog"
	"strings"
	"time"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/bridge"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

const (
	// 快速模式：犧牲部分 candidate 換取速度，但仍等待 TURN 避免純 STUN 對稱 NAT 必敗
	quickTurnCacheWaitTimeout = 2 * time.Second
	quickOfferGatherTimeout   = 5 * time.Second

	// 一般模式即時產生（prewarm 不可用時）的 TURN 快取等待上限
	normalTurnCacheWaitTimeout = 15 * time.Second

	// 預產生邀請碼的 TURN 快取等待上限
	prewarmTurnCacheTimeout = 10 * time.Second
)

// layoutClientWidgets 繪製主控模式的 UI 元件。
// 根據連線狀態分兩種顯示：
//   - 未連線：STUN 設定、ADB Port、產生邀請碼按鈕、邀請碼/回應碼文字框
//   - 已連線：ADB Proxy 位址 + 延遲、遠端主機資訊、設備列表、結束連線按鈕
//
// 邀請碼產生後會自動複製到剪貼簿；回應碼貼入後自動偵測變更並觸發連線。
func (t *pairTab) layoutClientWidgets(gtx layout.Context, th *material.Theme) []layout.Widget {
	t.mu.Lock()
	connected := t.connected
	hostname := t.remoteHostname
	remoteAddr := t.remoteAddr
	relayed := t.relayed
	dpm := t.dpm
	t.mu.Unlock()

	// 已連線：只顯示連線資訊 + 設備清單 + 結束連線按鈕
	if connected {
		for t.disconnectBtn.Clicked(gtx) {
			t.disconnect()
		}
		// 重新添加遠端 ADB 設備到本機（不小心在 Scrcpy GUI 等工具按了 disconnect 時使用）
		// 除了逐一重新 connect 之外，也會先重啟一次本機 ADB server，
		// 清掉卡住或殘留的 transport（例如 Scrcpy GUI 中多出的幽靈項目），
		// 再把目前這批 per-device proxy 重新掛回。
		for t.reconnectADBBtn.Clicked(gtx) {
			if dpm != nil {
				dpmCtx := dpm.Ctx()
				entries := dpm.Entries()
				targets := make([]string, 0, len(entries))
				for _, e := range entries {
					targets = append(targets, fmt.Sprintf("127.0.0.1:%d", e.Port))
				}
				adbAddr := fmt.Sprintf("127.0.0.1:%d", t.config.ADBPort)
				go func() {
					if err := adb.RefreshServerAndReconnect(dpmCtx, adbAddr, targets); err != nil && dpmCtx.Err() == nil {
						slog.Warn("refresh remote ADB devices failed", "error", err, "targets", len(targets))
					}
				}()
			}
		}
		// 重新整理被控端設備清單
		for t.refreshDevicesBtn.Clicked(gtx) {
			t.mu.Lock()
			ch := t.controlCh
			t.mu.Unlock()
			if ch != nil {
				go bridge.SendCtrlRefresh(ch)
			}
		}

		var widgets []layout.Widget
		latency := t.latencyMs.Load()

		// TURN 中繼通知橫幅（橘色背景）
		if relayed {
			widgets = append(widgets, relayBanner(th))
		}

		// 延遲顯示
		if latency > 0 {
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, fmt.Sprintf(msg().Pair.LatencyFmt, latency))
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			})
		}

		// 遠端主機資訊
		if hostname != "" || remoteAddr != "" {
			infoText := msg().Pair.RemoteHost
			if hostname != "" {
				infoText += hostname
			}
			if remoteAddr != "" {
				if hostname != "" {
					infoText += " (" + remoteAddr + ")"
				} else {
					infoText += remoteAddr
				}
			}
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Body2(th, infoText).Layout(gtx)
				})
			})
		}

		// per-device 設備列表（每台設備顯示各自的 proxy port）
		var entries []bridge.DeviceEntry
		if dpm != nil {
			entries = dpm.Entries()
		}
		if len(entries) > 0 {
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layoutDeviceEntries(gtx, th, entries)
				})
			})
		}

		// 重新添加遠端 ADB 設備到本機按鈕（藍色，設備列表非空時顯示）
		if len(entries) > 0 {
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(th, &t.reconnectADBBtn, msg().Pair.ReconnectADB)
					btn.Background = color.NRGBA{R: 33, G: 150, B: 243, A: 255} // #2196F3 藍色
					return btn.Layout(gtx)
				})
			})
		}

		// 重新整理被控端設備按鈕（灰色，已連線時總是顯示）
		widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.refreshDevicesBtn, msg().Pair.RefreshDevices)
				btn.Background = colorTabInactive
				return btn.Layout(gtx)
			})
		})

		// 結束連線按鈕
		widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.disconnectBtn, msg().Pair.DisconnectBtn)
				btn.Background = colorBtnStop
				return btn.Layout(gtx)
			})
		})

		return widgets
	}

	// 未連線：顯示完整設定 UI
	for t.cliGenOfferBtn.Clicked(gtx) {
		t.clientGenerateOffer(false)
	}
	for t.cliGenOfferFastBtn.Clicked(gtx) {
		t.clientGenerateOffer(true)
	}

	// 自動偵測回應碼貼入（offer 已產生時）
	if t.cliOfferOutEditor.Text() != "" {
		currentAnswer := strings.TrimSpace(t.cliAnswerInEditor.Text())
		if currentAnswer != "" && currentAnswer != t.cliProcessedAnswer {
			t.cliProcessedAnswer = currentAnswer
			t.clientApplyAnswer()
		}
	}

	widgets := []layout.Widget{
		// 產生邀請碼按鈕
		func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.cliGenOfferBtn, msg().Pair.GenerateOffer)
				return btn.Layout(gtx)
			})
		},
		// 立即產生邀請碼（快速模式）
		func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.cliGenOfferFastBtn, msg().Pair.GenerateOfferFast)
				btn.Background = colorWarning
				return btn.Layout(gtx)
			})
		},
		// 邀請碼輸出（限高可捲動，已自動複製到剪貼簿）
		func(gtx layout.Context) layout.Dimensions {
			if t.cliOfferOutEditor.Text() == "" {
				return layout.Dimensions{}
			}
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return tokenBox(gtx, th, msg().Pair.OfferOutLabel, &t.cliOfferOutEditor, "", unit.Dp(100))
			})
		},
		// 回應碼輸入（貼入後自動連線）
		func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return tokenBox(gtx, th, msg().Pair.AnswerInLabel, &t.cliAnswerInEditor, msg().Pair.AnswerInHint, unit.Dp(80))
			})
		},
	}

	return widgets
}

// maybeStartOfferPrewarm 在背景預先建立一組完整 gathering 的邀請碼。
// 僅在主控模式且目前無活動連線/等待中的 offer 時啟動。
func (t *pairTab) maybeStartOfferPrewarm() {
	t.mu.Lock()
	if t.isServer || t.connected || t.pm != nil || t.prewarmInFlight || t.prewarmPM != nil {
		t.mu.Unlock()
		return
	}
	done := make(chan struct{})
	t.prewarmInFlight = true
	t.prewarmDone = done
	t.mu.Unlock()

	go func(done chan struct{}) {
		defer close(done)
		startedAt := time.Now()
		defer func() {
			t.mu.Lock()
			if t.prewarmDone == done {
				t.prewarmInFlight = false
			}
			t.mu.Unlock()
		}()

		iceConfig, turnWarn := resolveICEWithTURN(t.config, t.tc, prewarmTurnCacheTimeout)
		if turnWarn != "" {
			slog.Warn("Cloudflare TURN unavailable for prewarm offer generation", "warning", turnWarn)
			t.mu.Lock()
			t.turnWarning = turnWarn
			t.mu.Unlock()
			t.window.Invalidate()
		}

		pm, err := webrtc.NewPeerManager(iceConfig)
		if err != nil {
			slog.Warn("prewarm offer failed", "step", "new_peer_manager", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			return
		}

		controlCh, err := pm.OpenChannel("control")
		if err != nil {
			slog.Warn("prewarm offer failed", "step", "open_control_channel", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			pm.Close()
			return
		}

		offerSDP, err := pm.CreateOffer()
		if err != nil {
			slog.Warn("prewarm offer failed", "step", "create_offer", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			pm.Close()
			controlCh.Close()
			return
		}

		compact := bridge.SDPToCompact(offerSDP)
		offerToken, err := bridge.EncodeToken(compact)
		if err != nil {
			slog.Warn("prewarm offer failed", "step", "encode_offer", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			pm.Close()
			controlCh.Close()
			return
		}

		var oldPM *webrtc.PeerManager
		var oldControl io.ReadWriteCloser

		t.mu.Lock()
		// 若 prewarm 期間狀態改變（切到被控端、已連線、或 prewarm 已被重置），丟棄本次結果
		if t.prewarmDone != done || t.isServer || t.connected || t.pm != nil {
			t.mu.Unlock()
			pm.Close()
			controlCh.Close()
			return
		}
		oldPM = t.prewarmPM
		oldControl = t.prewarmControl
		t.prewarmPM = pm
		t.prewarmControl = controlCh
		t.prewarmOffer = offerToken
		t.prewarmInFlight = false
		t.prewarmDone = nil
		t.mu.Unlock()

		if oldPM != nil {
			oldPM.Close()
		}
		if oldControl != nil {
			oldControl.Close()
		}
		host, srflx, relay := compact.CandidateStats()
		slog.Info("invite code prewarmed", "elapsed_ms", time.Since(startedAt).Milliseconds(), "token_len", len(offerToken),
			"candidates_host", host, "candidates_srflx", srflx, "candidates_relay", relay)
	}(done)
}

// usePrewarmedOffer 將背景預產生的 offer 套用到目前會話。
func (t *pairTab) usePrewarmedOffer() bool {
	t.mu.Lock()
	if t.prewarmPM == nil || t.prewarmControl == nil || t.prewarmOffer == "" {
		t.mu.Unlock()
		return false
	}

	pm := t.prewarmPM
	controlCh := t.prewarmControl
	offerToken := t.prewarmOffer

	t.prewarmPM = nil
	t.prewarmControl = nil
	t.prewarmOffer = ""
	t.prewarmDone = nil
	t.prewarmInFlight = false

	t.pm = pm
	t.controlCh = controlCh
	t.pendingClipboard = offerToken
	t.status = msg().Pair.StatusOfferReady
	t.mu.Unlock()

	t.cliOfferOutEditor.SetText(offerToken)
	t.window.Invalidate()
	slog.Info("invite code served from prewarm cache", "token_len", len(offerToken))
	return true
}

// clearReadyPrewarm 清除已完成但尚未使用的 prewarm 資源。
func (t *pairTab) clearReadyPrewarm() {
	t.mu.Lock()
	pm := t.prewarmPM
	controlCh := t.prewarmControl
	t.prewarmPM = nil
	t.prewarmControl = nil
	t.prewarmOffer = ""
	t.mu.Unlock()

	if pm != nil {
		pm.Close()
	}
	if controlCh != nil {
		controlCh.Close()
	}
}

// === 客戶端（主控）模式邏輯 ===

// clientGenerateOffer 產生 WebRTC Offer 並編碼為邀請碼 token。
// 流程：建立 PeerConnection → 建立 control DataChannel → 建立 Offer →
// SDP 壓縮編碼 → 顯示在 UI 並自動複製到剪貼簿。
func (t *pairTab) clientGenerateOffer(quick bool) {
	// 防重入：快速雙擊按鈕時，避免第二個 goroutine 覆寫 t.pm 導致舊 PeerConnection 洩漏
	t.mu.Lock()
	if t.generatingOffer || t.connected {
		t.mu.Unlock()
		return
	}
	t.generatingOffer = true
	t.mu.Unlock()

	if !quick {
		if t.usePrewarmedOffer() {
			t.mu.Lock()
			t.generatingOffer = false
			t.mu.Unlock()
			return
		}
		// 若 prewarm 尚未就緒，確保背景任務已啟動
		t.maybeStartOfferPrewarm()
	} else {
		// 快速模式不重用背景完整 gathering 結果，避免同時維持兩組待連線 offer。
		t.clearReadyPrewarm()
	}

	t.mu.Lock()
	t.status = msg().Pair.StatusGenerating
	t.mu.Unlock()
	t.window.Invalidate()

	go func() {
		defer func() {
			t.mu.Lock()
			t.generatingOffer = false
			t.mu.Unlock()
		}()
		startedAt := time.Now()
		mode := "normal"
		if quick {
			mode = "quick"
		}
		if t.config.TURNMode == TURNModeCloudflare {
			t.mu.Lock()
			t.status = msg().Pair.StatusPreparingTURN
			t.mu.Unlock()
			t.window.Invalidate()
		}
		turnStart := time.Now()
		waitTimeout := normalTurnCacheWaitTimeout
		if quick {
			waitTimeout = quickTurnCacheWaitTimeout
		}
		iceConfig, turnWarn := resolveICEWithTURN(t.config, t.tc, waitTimeout)
		slog.Debug("pair offer step", "mode", mode, "step", "turn_cache", "elapsed_ms", time.Since(turnStart).Milliseconds(), "warning", turnWarn != "", "wait_timeout_ms", waitTimeout.Milliseconds())
		if turnWarn != "" {
			slog.Warn("Cloudflare TURN unavailable for offer generation", "warning", turnWarn)
			t.mu.Lock()
			t.turnWarning = turnWarn
			t.mu.Unlock()
			t.window.Invalidate()
		}

		t.mu.Lock()
		t.status = msg().Pair.StatusCreatingPC
		t.mu.Unlock()
		t.window.Invalidate()

		stepStarted := time.Now()
		pm, err := webrtc.NewPeerManager(iceConfig)
		slog.Debug("pair offer step", "step", "new_peer_manager", "elapsed_ms", time.Since(stepStarted).Milliseconds())
		if err != nil {
			slog.Warn("pair offer failed", "step", "new_peer_manager", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			t.mu.Lock()
			t.status = fmt.Sprintf(msg().Pair.ErrCreatePCFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		// 建立 control DataChannel
		stepStarted = time.Now()
		controlCh, err := pm.OpenChannel("control")
		slog.Debug("pair offer step", "step", "open_control_channel", "elapsed_ms", time.Since(stepStarted).Milliseconds())
		if err != nil {
			slog.Warn("pair offer failed", "step", "open_control_channel", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			pm.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf(msg().Pair.ErrCreateCtrlChFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		t.mu.Lock()
		t.status = msg().Pair.StatusCreatingOffer
		t.mu.Unlock()
		t.window.Invalidate()

		stepStarted = time.Now()
		var offerSDP string
		if quick {
			offerSDP, err = pm.CreateOfferWithGatherTimeout(quickOfferGatherTimeout)
		} else {
			offerSDP, err = pm.CreateOffer()
		}
		slog.Debug("pair offer step", "mode", mode, "step", "create_offer", "elapsed_ms", time.Since(stepStarted).Milliseconds())
		if err != nil {
			slog.Warn("pair offer failed", "step", "create_offer", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			pm.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf(msg().Pair.ErrCreateOfferFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		t.mu.Lock()
		t.status = msg().Pair.StatusEncodingOffer
		t.mu.Unlock()
		t.window.Invalidate()

		stepStarted = time.Now()
		compact := bridge.SDPToCompact(offerSDP)
		offerToken, err := bridge.EncodeToken(compact)
		slog.Debug("pair offer step", "step", "encode_offer", "elapsed_ms", time.Since(stepStarted).Milliseconds())
		if err != nil {
			slog.Warn("pair offer failed", "step", "encode_offer", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			pm.Close()
			controlCh.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf(msg().Pair.ErrEncodeOfferFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		t.mu.Lock()
		t.pm = pm
		t.controlCh = controlCh
		t.pendingClipboard = offerToken
		t.pendingCliOfferSet = &offerToken
		t.status = msg().Pair.StatusOfferReady
		t.mu.Unlock()
		t.window.Invalidate()
		host, srflx, relay := compact.CandidateStats()
		slog.Info("invite code generated", "mode", mode, "elapsed_ms", time.Since(startedAt).Milliseconds(), "token_len", len(offerToken),
			"candidates_host", host, "candidates_srflx", srflx, "candidates_relay", relay)
	}()
}

// clientApplyAnswer 處理對方回傳的回應碼，完成 WebRTC 握手並啟動服務。
// 流程：解碼回應碼 → HandleAnswer → 建立 ADB server proxy listener →
// 啟動 adbServerProxy（每個 TCP 連線建立 DataChannel）→
// 啟動 RTT 輪詢 → 啟動 controlReadLoop（接收設備清單）。
func (t *pairTab) clientApplyAnswer() {
	answerToken := t.cliAnswerInEditor.Text()
	proxyPort := t.config.ProxyPort

	t.mu.Lock()
	pm := t.pm
	controlCh := t.controlCh
	t.mu.Unlock()

	if pm == nil {
		t.mu.Lock()
		t.status = msg().Pair.StatusPleaseGenerate
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	if answerToken == "" {
		t.mu.Lock()
		t.status = msg().Pair.StatusPleaseAnswer
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	go func() {
		answer, err := bridge.DecodeToken(answerToken)
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf(msg().Pair.ErrInvalidAnswerFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		// 提前建立 context 以便 cleanup 可取消後續操作（M28 修復）
		ctx, cancel := context.WithCancel(context.Background())
		t.mu.Lock()
		t.cancel = cancel
		t.mu.Unlock()

		// 先註冊回呼再啟動 ICE，避免 LAN 環境下 ICE 在毫秒內完成導致回呼未觸發
		pm.OnDisconnect(func() {
			// 連線斷開時必須釋放本機 per-device proxy，
			// 否則 adb devices 仍會看到 127.0.0.1:5555/5556，但實際 DataChannel 已失效，
			// 後續 adb shell/scrcpy 會持續回傳 "error: closed"。
			t.mu.Lock()
			cancel := t.cancel
			dpm := t.dpm
			control := t.controlCh
			t.cancel = nil
			t.dpm = nil
			t.controlCh = nil
			t.pm = nil
			t.status = msg().Pair.StatusP2PDisconnected
			t.connected = false
			t.relayed = false
			t.mu.Unlock()

			if cancel != nil {
				cancel()
			}
			if dpm != nil {
				dpm.Close()
			}
			if control != nil {
				control.Close()
			}
			t.window.Invalidate()
		})

		// 建立 per-device proxy 管理器（每台設備獨立 port）——
		// 在 HandleAnswer 之前建立，確保 OnConnected 觸發時 dpm 已就緒
		onReady, onRemoved := guiDeviceProxyCallbacks(t.window, "device proxy")
		dpm := bridge.NewDeviceProxyManager(bridge.DeviceProxyConfig{
			PortStart: proxyPort,
			OpenCh:    pm.OpenChannel,
			ADBAddr:   fmt.Sprintf("127.0.0.1:%d", t.config.ADBPort),
			OnReady:   onReady,
			OnRemoved: onRemoved,
		})

		t.mu.Lock()
		t.dpm = dpm
		t.mu.Unlock()

		// OnConnected：ICE 連線真正建立後才標記 connected = true
		pm.OnConnected(func(relayed bool) {
			t.mu.Lock()
			t.connected = true
			if relayed {
				t.status = msg().Pair.StatusRelayConnected
			} else {
				t.status = msg().Pair.StatusP2PConnected
			}
			t.mu.Unlock()
			t.onConnectedHandler(relayed)
		})

		// HandleAnswer 設定 remote SDP，觸發 ICE 連接嘗試
		if err := pm.HandleAnswer(bridge.CompactToSDP(answer)); err != nil {
			cancel()
			dpm.Close()
			pm.Close() // 清理 ICE agent、DTLS transport、UDP socket
			t.mu.Lock()
			t.pm = nil
			t.dpm = nil
			t.controlCh = nil
			t.cancel = nil
			t.status = fmt.Sprintf(msg().Pair.ErrHandleAnswerFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		// HandleAnswer 成功 → ICE 連接嘗試中，尚未真正連上
		t.mu.Lock()
		t.status = msg().Pair.StatusP2PConnecting
		t.mu.Unlock()
		t.window.Invalidate()

		// 啟動 RTT 延遲輪詢
		go t.rttPollLoop(ctx, pm)

		// 啟動 control channel 讀取迴圈（驅動 DeviceProxyManager 的設備增減）
		t.controlReadLoop(ctx, controlCh)
	}()
}

// controlReadLoop 持續讀取 control channel 的 JSON 訊息，更新客戶端 UI。
// 委託 bridge.ControlReadLoop 解析 JSON，透過 callback 驅動 DeviceProxyManager 的設備增減。
//
// 訊息類型處理：
//   - "hello"：記錄遠端主機名稱（顯示在 UI 上）
//   - "devices"：委託 DeviceProxyManager.UpdateDevices 管理 per-device proxy
func (t *pairTab) controlReadLoop(ctx context.Context, controlCh io.ReadWriteCloser) {
	err := bridge.ControlReadLoop(ctx, controlCh, func(cm bridge.CtrlMessage) {
		switch cm.Type {
		case "hello":
			t.mu.Lock()
			t.remoteHostname = cm.Hostname
			t.mu.Unlock()
			t.window.Invalidate()

		case "devices":
			t.mu.Lock()
			dpm := t.dpm
			t.mu.Unlock()

			if dpm != nil {
				dpm.UpdateDevices(cm.Devices)
			}

			// 更新狀態文字
			count := 0
			for _, d := range cm.Devices {
				if d.State == "device" {
					count++
				}
			}
			t.mu.Lock()
			if count == 0 {
				if t.relayed {
					t.status = msg().Pair.StatusRelayWaiting
				} else {
					t.status = msg().Pair.StatusP2PWaiting
				}
			} else {
				if t.relayed {
					t.status = fmt.Sprintf(msg().Pair.StatusRelayDevicesFmt, count)
				} else {
					t.status = fmt.Sprintf(msg().Pair.StatusP2PDevicesFmt, count)
				}
			}
			t.mu.Unlock()
			t.window.Invalidate()
		}
	})
	if err != nil {
		// 套用「鎖內擷取引用、鎖外關閉」模式，與 client OnDisconnect 冪等：
		// 若 OnDisconnect 先觸發已 nil 化所有引用，此處取出全是 nil → no-op；反之亦然。
		t.mu.Lock()
		cancel := t.cancel
		dpm := t.dpm
		pm := t.pm
		control := t.controlCh
		t.cancel = nil
		t.dpm = nil
		t.pm = nil
		t.controlCh = nil
		t.status = msg().Pair.StatusControlClosed
		t.connected = false
		t.relayed = false
		t.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if dpm != nil {
			dpm.Close()
		}
		if pm != nil {
			pm.Close()
		}
		if control != nil {
			control.Close()
		}
		t.window.Invalidate()
		t.maybeStartOfferPrewarm()
	}
}
