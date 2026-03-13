// tab_pair_server.go 實作「簡易連線」分頁的被控端（伺服器）模式 UI 與邏輯。

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

// layoutServerWidgets 繪製被控模式的 UI 元件。
// 邀請碼貼入後自動偵測變更並觸發 serverProcessOffer（產生回應碼）。
// 已連線後顯示延遲、設備列表、結束連線按鈕。
func (t *pairTab) layoutServerWidgets(gtx layout.Context, th *material.Theme) []layout.Widget {
	t.mu.Lock()
	devices := append([]bridge.DeviceInfo{}, t.srvDevices...)
	connected := t.connected
	relayed := t.relayed
	t.mu.Unlock()

	// 已連線：只顯示延遲 + 設備清單 + 結束連線按鈕
	if connected {
		for t.disconnectBtn.Clicked(gtx) {
			t.disconnect()
		}

		var widgets []layout.Widget
		latency := t.latencyMs.Load()

		// TURN 中繼通知橫幅（橘色背景）
		if relayed {
			widgets = append(widgets, relayBanner(th))
		}
		if latency > 0 {
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, fmt.Sprintf(msg().Pair.LatencyFmt, latency))
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			})
		}

		if len(devices) > 0 {
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Body2(th, fmt.Sprintf(msg().Common.DevicesFmt, len(devices))).Layout(gtx)
				})
			})
			for _, d := range devices {
				text := fmt.Sprintf("  %s [%s]", d.Serial, d.State)
				widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return material.Body2(th, text).Layout(gtx)
					})
				})
			}
		}

		// 結束連線按鈕
		widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.disconnectBtn, msg().Pair.DisconnectBtn)
				btn.Background = color.NRGBA{R: 244, G: 67, B: 54, A: 255}
				return btn.Layout(gtx)
			})
		})
		return widgets
	}

	// 未連線：完整設定 UI + 自動偵測邀請碼
	currentOffer := strings.TrimSpace(t.srvOfferInEditor.Text())
	if currentOffer != "" && currentOffer != t.srvProcessedOffer {
		t.srvProcessedOffer = currentOffer
		t.serverProcessOffer()
	}

	for t.clearBtn.Clicked(gtx) {
		t.cleanup() // 清理待連線的 PeerManager（如有）
		t.srvOfferInEditor.SetText("")
		t.srvAnswerOutEditor.SetText("")
		t.srvProcessedOffer = ""
		t.mu.Lock()
		t.status = msg().Pair.StatusNotStarted
		t.mu.Unlock()
	}

	var widgets []layout.Widget

	// 邀請碼輸入
	widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return tokenBox(gtx, th, msg().Pair.OfferInLabel, &t.srvOfferInEditor, msg().Pair.OfferInHint, unit.Dp(80))
		})
	})
	// 回應碼輸出
	widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
		if t.srvAnswerOutEditor.Text() == "" {
			return layout.Dimensions{}
		}
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return tokenBox(gtx, th, msg().Pair.AnswerOutLabel, &t.srvAnswerOutEditor, "", unit.Dp(100))
		})
	})

	// 清除按鈕（有內容時才顯示）
	hasContent := t.srvOfferInEditor.Text() != "" || t.srvAnswerOutEditor.Text() != ""
	if hasContent {
		widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.clearBtn, msg().Pair.ClearBtn)
				btn.Background = colorTabInactive
				return btn.Layout(gtx)
			})
		})
	}

	// 設備列表
	if len(devices) > 0 {
		widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return material.Body2(th, fmt.Sprintf(msg().Common.DevicesFmt, len(devices))).Layout(gtx)
			})
		})
		for _, d := range devices {
			text := fmt.Sprintf("  %s [%s]", d.Serial, d.State)
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Body2(th, text).Layout(gtx)
				})
			})
		}
	}

	return widgets
}

// === 伺服器（被控）模式邏輯 ===

// serverProcessOffer 處理對方的邀請碼，建立 Answer 並啟動被控端服務。
// 流程：EnsureADB → 解碼邀請碼 → 建立 PeerConnection → 設定 OnChannel 回調
// （監聽 control/adb-server/adb-stream/adb-fwd DataChannel）→ HandleOffer →
// 產生回應碼 → 自動複製到剪貼簿。
// 注意：不在此時設定 connected=true，而是等 control DataChannel 真正開啟後才切換。
func (t *pairTab) serverProcessOffer() {
	offerToken := t.srvOfferInEditor.Text()
	adbPort := t.config.ADBPort

	if offerToken == "" {
		t.mu.Lock()
		t.status = msg().Pair.StatusPleaseOffer
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	// 防並行：auto-detect 與手動點擊可能同時觸發，避免兩個 goroutine 競爭覆寫 t.pm
	t.mu.Lock()
	if t.processingOffer || t.connected {
		t.mu.Unlock()
		return
	}
	t.processingOffer = true
	t.status = msg().Common.CheckingADB
	t.mu.Unlock()
	t.window.Invalidate()

	go func() {
		defer func() {
			t.mu.Lock()
			t.processingOffer = false
			t.mu.Unlock()
		}()

		// 建立可取消 context，存入 t.cancel，cleanup() 可取消
		ctx, cancel := context.WithCancel(context.Background())
		t.mu.Lock()
		t.cancel = cancel
		t.mu.Unlock()

		startedAt := time.Now()
		adbAddr := fmt.Sprintf("127.0.0.1:%d", adbPort)

		// 確保 ADB 可用（改用可取消 ctx）
		stepStarted := time.Now()
		if err := adb.EnsureADB(ctx, adbAddr, func(status string) {
			t.mu.Lock()
			t.status = status
			t.mu.Unlock()
			t.window.Invalidate()
		}); err != nil {
			slog.Warn("pair answer failed", "step", "ensure_adb", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			cancel()
			t.mu.Lock()
			t.cancel = nil
			t.status = fmt.Sprintf(msg().Common.ADBErrorFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}
		slog.Debug("pair answer step", "step", "ensure_adb", "elapsed_ms", time.Since(stepStarted).Milliseconds())

		t.mu.Lock()
		t.status = msg().Pair.StatusDecodingOffer
		t.mu.Unlock()
		t.window.Invalidate()

		// 解碼 Offer
		stepStarted = time.Now()
		offer, err := bridge.DecodeToken(offerToken)
		slog.Debug("pair answer step", "step", "decode_offer", "elapsed_ms", time.Since(stepStarted).Milliseconds())
		if err != nil {
			slog.Warn("pair answer failed", "step", "decode_offer", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			cancel()
			t.mu.Lock()
			t.cancel = nil
			t.status = fmt.Sprintf(msg().Pair.ErrInvalidOfferFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		if t.config.TURNMode == TURNModeCloudflare {
			t.mu.Lock()
			t.status = msg().Pair.StatusPreparingTURN
			t.mu.Unlock()
			t.window.Invalidate()
		}
		stepStarted = time.Now()
		iceConfig, turnWarn := resolveICEWithTURN(t.config, t.tc, 10*time.Second)
		slog.Debug("pair answer step", "step", "turn_cache", "elapsed_ms", time.Since(stepStarted).Milliseconds(), "warning", turnWarn != "")
		if turnWarn != "" {
			slog.Warn("Cloudflare TURN unavailable for answer generation", "warning", turnWarn)
			t.mu.Lock()
			t.turnWarning = turnWarn
			t.mu.Unlock()
			t.window.Invalidate()
		}

		t.mu.Lock()
		t.status = msg().Pair.StatusCreatingPC
		t.mu.Unlock()
		t.window.Invalidate()

		stepStarted = time.Now()
		pm, err := webrtc.NewPeerManager(iceConfig)
		slog.Debug("pair answer step", "step", "new_peer_manager", "elapsed_ms", time.Since(stepStarted).Milliseconds())
		if err != nil {
			slog.Warn("pair answer failed", "step", "new_peer_manager", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			cancel()
			t.mu.Lock()
			t.cancel = nil
			t.status = fmt.Sprintf(msg().Pair.ErrCreatePCFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		// 監聽客戶端建立的 DataChannel
		srvHandler := &bridge.ServerHandler{ADBAddr: adbAddr}
		pm.OnChannel(func(label string, rwc io.ReadWriteCloser) {
			slog.Debug("received DataChannel", "label", label)
			if label == "control" {
				// DataChannel 開啟 = P2P 真正連上，此時才切換到已連線 UI
				t.mu.Lock()
				t.connected = true
				t.status = msg().Pair.StatusP2PWaiting
				t.mu.Unlock()
				t.window.Invalidate()
				// 客戶端的 control channel → 啟動設備推送
				go t.devicePushLoop(ctx, rwc, adbAddr)
				return
			}
			// 委託 ServerHandler 處理 adb-server/adb-stream/adb-fwd DataChannel
			if srvHandler.HandleChannel(ctx, label, rwc) {
				return
			}
		})

		pm.OnDisconnect(func() {
			// 套用「鎖內擷取引用、鎖外關閉」模式，與 client OnDisconnect 對稱。
			// server-mode 沒有 dpm/controlCh（control channel 由 OnChannel callback 管理），
			// 只需清理 cancel + nil 化 pm 引用。
			t.mu.Lock()
			cancel := t.cancel
			t.cancel = nil
			t.pm = nil
			t.status = msg().Pair.StatusP2PDisconnected
			t.connected = false
			t.relayed = false
			t.srvDevices = nil
			t.mu.Unlock()

			if cancel != nil {
				cancel()
			}
			// 注意：不在 OnDisconnect callback 內呼叫 pm.Close()，
			// 因為此 callback 從 pion 的 OnConnectionStateChange 觸發，
			// 在 callback 內關閉 PeerConnection 可能造成重入問題。
			// pm 引用已 nil 化，確保 cleanup() 不會重複關閉。
			t.window.Invalidate()
		})

		pm.OnConnected(func(relayed bool) {
			t.mu.Lock()
			t.relayed = relayed
			t.mu.Unlock()
			if relayed {
				slog.Info("P2P connection is relayed through TURN server")
			}
			t.window.Invalidate()
		})

		// 處理 Offer 並生成 Answer
		t.mu.Lock()
		t.status = msg().Pair.StatusCreatingAnswer
		t.mu.Unlock()
		t.window.Invalidate()

		stepStarted = time.Now()
		answerSDP, err := pm.HandleOffer(bridge.CompactToSDP(offer))
		slog.Debug("pair answer step", "step", "handle_offer", "elapsed_ms", time.Since(stepStarted).Milliseconds())
		if err != nil {
			slog.Warn("pair answer failed", "step", "handle_offer", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			pm.Close()
			cancel()
			t.mu.Lock()
			t.cancel = nil
			t.status = fmt.Sprintf(msg().Pair.ErrHandleOfferFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		t.mu.Lock()
		t.status = msg().Pair.StatusEncodingAnswer
		t.mu.Unlock()
		t.window.Invalidate()

		stepStarted = time.Now()
		answerToken, err := bridge.EncodeToken(bridge.SDPToCompact(answerSDP))
		slog.Debug("pair answer step", "step", "encode_answer", "elapsed_ms", time.Since(stepStarted).Milliseconds())
		if err != nil {
			slog.Warn("pair answer failed", "step", "encode_answer", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			pm.Close()
			cancel()
			t.mu.Lock()
			t.cancel = nil
			t.status = fmt.Sprintf(msg().Pair.ErrEncodeAnswerFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		t.mu.Lock()
		t.pm = pm
		t.cancel = cancel
		// 注意：不在此設 connected = true，等 control DataChannel 開啟才切換
		t.pendingClipboard = answerToken
		t.pendingSrvAnswerSet = &answerToken
		t.status = msg().Pair.StatusAnswerReady
		t.mu.Unlock()
		t.window.Invalidate()
		slog.Info("answer code generated", "elapsed_ms", time.Since(startedAt).Milliseconds(), "token_len", len(answerToken))

		// 啟動 RTT 延遲輪詢
		go t.rttPollLoop(ctx, pm)
	}()
}

// devicePushLoop 追蹤 ADB 設備並透過 control channel 推送清單給客戶端。
// 委託 bridge.DevicePushLoop 處理 ADB tracker 和 JSON 推送，
// 透過 callback 更新被控端 UI 的設備列表。
func (t *pairTab) devicePushLoop(ctx context.Context, controlCh io.ReadWriteCloser, adbAddr string) {
	bridge.DevicePushLoop(ctx, controlCh, adbAddr, func(devices []bridge.DeviceInfo) {
		t.mu.Lock()
		t.srvDevices = devices
		online := 0
		for _, d := range devices {
			if d.State == "device" {
				online++
			}
		}
		if online > 0 {
			t.status = fmt.Sprintf(msg().Pair.StatusP2PDevicesFmt, online)
		} else {
			t.status = msg().Pair.StatusP2PWaiting
		}
		t.mu.Unlock()
		t.window.Invalidate()
	})
}
