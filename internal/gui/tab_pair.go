// tab_pair.go 實作「簡易連線」分頁的 GUI 與邏輯。
//
// 本分頁透過手動交換 SDP token（邀請碼/回應碼）建立 WebRTC P2P 連線，
// 不需要中央伺服器。適合跨 NAT 的開發場景。
//
// # SDP 緊湊格式（compactSDP）
//
// 完整的 WebRTC SDP 包含大量樣板行（v=, o=, s=, m= 等固定內容），
// 實際需要交換的只有 ice-ufrag、ice-pwd、fingerprint、setup role 和 candidates。
// compactSDP 只保留這些必要欄位，配合二進位序列化 + deflate 壓縮 + base64 編碼，
// 將 token 長度壓縮到約 100-200 字元，方便使用者手動複製貼上。
//
// # ADB Transport 多工
//
// 客戶端（主控端）建立 ADB server proxy，接受 `adb connect` 的 device transport 連線。
// 每個 adb 服務（shell, push, pull 等）會建立一條獨立的 DataChannel
// （label=adb-stream/{id}/{serial}/{service}），
// 由 deviceBridge（見 adb_transport.go）管理 OPEN/OKAY/WRTE/CLSE 的訊息流控。
//
// # control channel
//
// P2P 連線建立後，雙方透過 label="control" 的 DataChannel 交換 JSON 訊息：
//   - hello：被控端發送主機名稱
//   - devices：被控端定期推送設備清單（serial、state、features）
package gui

import (
	"context"
	"fmt"
	"image/color"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/io/clipboard"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/bridge"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

const (
	quickTurnCacheWaitTimeout = 300 * time.Millisecond
	quickOfferGatherTimeout   = 1500 * time.Millisecond
)

// pairTab 是「簡易連線」分頁的完整狀態。
// 提供兩個角色：主控模式（客戶端）和被控模式（伺服器）。
//
// 主控模式流程：產生邀請碼 → 傳給對方 → 對方貼回回應碼 → 自動建立 P2P 連線 →
// 啟動 ADB server proxy → 自動 adb connect。
//
// 被控模式流程：貼入邀請碼 → 自動產生回應碼 → 傳給對方 → 等待 P2P 連線 →
// 透過 control channel 推送設備清單。
type pairTab struct {
	window *app.Window
	config *AppConfig // 共用設定（Port、STUN 等），來自設定面板

	// 角色選擇
	clientBtn widget.Clickable
	serverBtn widget.Clickable
	isServer  bool // false=客戶端, true=伺服器

	// --- 客戶端模式 ---
	cliGenOfferBtn     widget.Clickable
	cliGenOfferFastBtn widget.Clickable
	cliOfferOutEditor  widget.Editor // 顯示邀請碼（唯讀）
	cliAnswerInEditor  widget.Editor // 貼入回應碼
	cliApplyBtn        widget.Clickable

	// --- 被控模式 ---
	srvOfferInEditor   widget.Editor // 貼入邀請碼
	srvAnswerOutEditor widget.Editor // 顯示回應碼（唯讀）
	srvProcessedOffer  string        // 上次已處理的 offer，用於自動偵測變更
	cliProcessedAnswer string        // 上次已處理的 answer，用於自動偵測變更

	// --- 共用狀態 ---
	mu        sync.Mutex
	status    string
	connected bool
	cancel    context.CancelFunc
	pm        *webrtc.PeerManager
	controlCh io.ReadWriteCloser // 客戶端持有的 control channel

	// 剪貼簿：產生 token 後自動複製
	pendingClipboard string

	// 背景預產生邀請碼（完整 ICE gathering）
	prewarmInFlight bool
	prewarmDone     chan struct{}
	prewarmPM       *webrtc.PeerManager
	prewarmControl  io.ReadWriteCloser
	prewarmOffer    string

	// 伺服器模式：設備清單
	srvDevices []bridge.DeviceInfo

	// 客戶端模式：per-device proxy 管理器（每台設備獨立 port）
	dpm *bridge.DeviceProxyManager

	// 遠端資訊（客戶端模式，mutex 保護）
	remoteHostname string // 遠端主機名稱（via control channel）
	remoteAddr     string // 遠端 IP:port（via WebRTC stats）

	// 實時延遲（毫秒），atomic 存取
	latencyMs atomic.Int64

	// 是否透過 TURN 中繼連線（mutex 保護）
	relayed bool

	// TURN 憑證快取（啟動時預先取得）
	tc *turnCache

	// TURN 不可用警告（mutex 保護）
	turnWarning string

	// 結束連線 / 清除按鈕
	disconnectBtn widget.Clickable
	clearBtn      widget.Clickable

	// 捲動清單
	list widget.List
}

// newPairTab 建立並初始化 pairTab，設定各輸入框的預設值。
// 預設顯示主控模式（isServer=false）。
func newPairTab(w *app.Window, cfg *AppConfig, tc *turnCache) *pairTab {
	t := &pairTab{
		window: w,
		config: cfg,
		tc:     tc,
		status: msg().Pair.StatusNotStarted,
	}
	// 客戶端模式
	t.cliOfferOutEditor.ReadOnly = true

	// 伺服器模式
	t.srvAnswerOutEditor.ReadOnly = true

	t.list.Axis = layout.Vertical
	t.maybeStartOfferPrewarm()
	return t
}

// layout 繪製分頁內容。使用 widget.List 實現可捲動版面（token 和設備列表可能超出視窗）。
// 已連線時自動切換到精簡 UI（只顯示連線資訊 + 設備清單 + 結束連線按鈕）。
func (t *pairTab) layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	t.mu.Lock()
	isServer := t.isServer
	status := t.status
	connected := t.connected
	turnWarning := t.turnWarning
	t.mu.Unlock()

	// 自動複製到剪貼簿
	t.mu.Lock()
	clip := t.pendingClipboard
	t.pendingClipboard = ""
	t.mu.Unlock()
	if clip != "" {
		gtx.Execute(clipboard.WriteCmd{
			Type: "application/text",
			Data: io.NopCloser(strings.NewReader(clip)),
		})
	}

	// 角色切換
	for t.clientBtn.Clicked(gtx) {
		t.isServer = false
		t.maybeStartOfferPrewarm()
	}
	for t.serverBtn.Clicked(gtx) {
		t.isServer = true
		t.clearReadyPrewarm()
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// 角色選擇列（全寬，固定在頂部不捲動，與主分頁對齊）
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(th, &t.clientBtn, msg().Common.Controller)
						if !isServer {
							btn.Background = colorModeActive
						} else {
							btn.Background = colorModeInactive
						}
						return btn.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(th, &t.serverBtn, msg().Common.Agent)
						if isServer {
							btn.Background = colorModeActive
						} else {
							btn.Background = colorModeInactive
						}
						return btn.Layout(gtx)
					}),
				)
			})
		}),
		// 可捲動的內容區域（加水平 padding，與子模式按鈕列分離）
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				var widgets []layout.Widget

				// 子模式內容
				if isServer {
					for _, child := range t.layoutServerWidgets(gtx, th) {
						widgets = append(widgets, child)
					}
				} else {
					for _, child := range t.layoutClientWidgets(gtx, th) {
						widgets = append(widgets, child)
					}
				}

				// 狀態
				widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
					c := colorPanelHint
					if connected {
						c = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
					}
					return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return statusText(gtx, th, msg().Common.StatusPrefix+status, c)
					})
				})

				// TURN 不可用警告（橘色文字）
				if turnWarning != "" {
					widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return statusText(gtx, th, turnWarning, color.NRGBA{R: 255, G: 152, B: 0, A: 255})
						})
					})
				}

				// 可捲動的清單
				return material.List(th, &t.list).Layout(gtx, len(widgets), func(gtx layout.Context, i int) layout.Dimensions {
					return widgets[i](gtx)
				})
			})
		}),
	)
}

// === 主控模式 UI ===

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

		var widgets []layout.Widget
		latency := t.latencyMs.Load()

		// TURN 中繼通知橫幅（橘色背景）
		if relayed {
			widgets = append(widgets, relayBanner(th))
		}

		// 延遲顯示
		if latency > 0 {
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, fmt.Sprintf("RTT: %d ms", latency))
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
					return material.Body2(th, fmt.Sprintf(msg().Pair.RemoteDevFmt, len(entries))).Layout(gtx)
				})
			})
			for _, e := range entries {
				text := fmt.Sprintf("    %s [device] → 127.0.0.1:%d", e.Serial, e.Port)
				widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(th, text)
						lbl.Color = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
						return lbl.Layout(gtx)
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
				btn.Background = color.NRGBA{R: 255, G: 152, B: 0, A: 255}
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

// === 被控模式 UI ===

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

		iceConfig := parseICEConfig(t.config)
		if t.config.TURNMode == TURNModeCloudflare {
			servers, warning := t.tc.getServers(0)
			if warning != "" {
				slog.Warn("Cloudflare TURN unavailable for prewarm offer generation", "warning", warning)
				t.mu.Lock()
				t.turnWarning = warning
				t.mu.Unlock()
				t.window.Invalidate()
			} else {
				iceConfig.TURNServers = servers
			}
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

		offerToken, err := bridge.EncodeToken(bridge.SDPToCompact(offerSDP))
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
		slog.Info("invite code prewarmed", "elapsed_ms", time.Since(startedAt).Milliseconds(), "token_len", len(offerToken))
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
	if !quick {
		if t.usePrewarmedOffer() {
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
		startedAt := time.Now()
		mode := "normal"
		if quick {
			mode = "quick"
		}
		iceConfig := parseICEConfig(t.config)
		if t.config.TURNMode == TURNModeCloudflare {
			t.mu.Lock()
			t.status = msg().Pair.StatusPreparingTURN
			t.mu.Unlock()
			t.window.Invalidate()

			stepStarted := time.Now()
			waitTimeout := time.Duration(0)
			if quick {
				waitTimeout = quickTurnCacheWaitTimeout
			}
			servers, warning := t.tc.getServers(waitTimeout)
			slog.Debug("pair offer step", "mode", mode, "step", "turn_cache", "elapsed_ms", time.Since(stepStarted).Milliseconds(), "servers", len(servers), "warning", warning != "", "wait_timeout_ms", waitTimeout.Milliseconds())
			if warning != "" {
				slog.Warn("Cloudflare TURN unavailable for offer generation", "warning", warning)
				t.mu.Lock()
				t.turnWarning = warning
				t.mu.Unlock()
				t.window.Invalidate()
			} else {
				iceConfig.TURNServers = servers
			}
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
		offerToken, err := bridge.EncodeToken(bridge.SDPToCompact(offerSDP))
		slog.Debug("pair offer step", "step", "encode_offer", "elapsed_ms", time.Since(stepStarted).Milliseconds())
		if err != nil {
			slog.Warn("pair offer failed", "step", "encode_offer", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
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
		t.status = msg().Pair.StatusOfferReady
		t.mu.Unlock()
		t.cliOfferOutEditor.SetText(offerToken)
		t.window.Invalidate()
		slog.Info("invite code generated", "mode", mode, "elapsed_ms", time.Since(startedAt).Milliseconds(), "token_len", len(offerToken))
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

		pm.OnConnected(func(relayed bool) {
			t.mu.Lock()
			t.relayed = relayed
			t.mu.Unlock()
			if relayed {
				slog.Info("P2P connection is relayed through TURN server")
			}
			t.window.Invalidate()
		})

		if err := pm.HandleAnswer(bridge.CompactToSDP(answer)); err != nil {
			pm.Close() // 清理 ICE agent、DTLS transport、UDP socket
			t.mu.Lock()
			t.pm = nil
			t.controlCh = nil
			t.status = fmt.Sprintf(msg().Pair.ErrHandleAnswerFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		ctx, cancel := context.WithCancel(context.Background())

		// 建立 per-device proxy 管理器（每台設備獨立 port）
		dpm := bridge.NewDeviceProxyManager(bridge.DeviceProxyConfig{
			PortStart: proxyPort,
			OpenCh:    pm.OpenChannel,
			ADBAddr:   fmt.Sprintf("127.0.0.1:%d", t.config.ADBPort),
			OnReady: func(serial string, port int) {
				slog.Info("device proxy ready", "serial", serial, "port", port)
				t.window.Invalidate()
				// 自動 adb connect
				go func() {
					time.Sleep(300 * time.Millisecond)
					dialer := adb.NewDialer("")
					target := fmt.Sprintf("127.0.0.1:%d", port)
					if err := dialer.Connect(target); err != nil {
						slog.Debug("auto adb connect failed", "target", target, "error", err)
					}
				}()
			},
			OnRemoved: func(serial string, port int) {
				slog.Info("device proxy removed", "serial", serial, "port", port)
				t.window.Invalidate()
				// 自動 adb disconnect
				go func() {
					dialer := adb.NewDialer("")
					dialer.Disconnect(fmt.Sprintf("127.0.0.1:%d", port))
				}()
			},
		})

		t.mu.Lock()
		t.connected = true
		t.cancel = cancel
		t.dpm = dpm
		t.status = msg().Pair.StatusP2PConnected
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
			t.status = fmt.Sprintf(msg().Pair.StatusP2PDevicesFmt, count)
			t.mu.Unlock()
			t.window.Invalidate()
		}
	})
	if err != nil {
		t.mu.Lock()
		t.status = msg().Pair.StatusControlClosed
		t.connected = false
		t.mu.Unlock()
		t.window.Invalidate()
	}
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

	t.mu.Lock()
	t.status = msg().Common.CheckingADB
	t.mu.Unlock()
	t.window.Invalidate()

	go func() {
		startedAt := time.Now()
		adbAddr := fmt.Sprintf("127.0.0.1:%d", adbPort)

		// 確保 ADB 可用
		stepStarted := time.Now()
		if err := adb.EnsureADB(context.Background(), adbAddr, func(status string) {
			t.mu.Lock()
			t.status = status
			t.mu.Unlock()
			t.window.Invalidate()
		}); err != nil {
			slog.Warn("pair answer failed", "step", "ensure_adb", "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			t.mu.Lock()
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
			t.mu.Lock()
			t.status = fmt.Sprintf(msg().Pair.ErrInvalidOfferFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		iceConfig := parseICEConfig(t.config)
		if t.config.TURNMode == TURNModeCloudflare {
			t.mu.Lock()
			t.status = msg().Pair.StatusPreparingTURN
			t.mu.Unlock()
			t.window.Invalidate()

			stepStarted = time.Now()
			servers, warning := t.tc.getServers(0)
			slog.Debug("pair answer step", "step", "turn_cache", "elapsed_ms", time.Since(stepStarted).Milliseconds(), "servers", len(servers), "warning", warning != "")
			if warning != "" {
				slog.Warn("Cloudflare TURN unavailable for answer generation", "warning", warning)
				t.mu.Lock()
				t.turnWarning = warning
				t.mu.Unlock()
				t.window.Invalidate()
			} else {
				iceConfig.TURNServers = servers
			}
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
			t.mu.Lock()
			t.status = fmt.Sprintf(msg().Pair.ErrCreatePCFmt, err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		ctx, cancel := context.WithCancel(context.Background())

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
			t.mu.Lock()
			t.status = msg().Pair.StatusP2PDisconnected
			t.connected = false
			t.relayed = false
			t.srvDevices = nil
			t.mu.Unlock()
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
		t.status = msg().Pair.StatusAnswerReady
		t.mu.Unlock()
		t.srvAnswerOutEditor.SetText(answerToken)
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

// rttPollLoop 每 2 秒輪詢 WebRTC RTT 並更新 latencyMs。
func (t *pairTab) rttPollLoop(ctx context.Context, pm *webrtc.PeerManager) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed := false

			rtt := pm.GetRTT()
			ms := rtt.Milliseconds()
			if old := t.latencyMs.Load(); old != ms {
				t.latencyMs.Store(ms)
				changed = true
			}

			if addr := pm.GetRemoteAddr(); addr != "" {
				t.mu.Lock()
				if t.remoteAddr != addr {
					t.remoteAddr = addr
					changed = true
				}
				t.mu.Unlock()
			}

			if changed {
				t.window.Invalidate()
			}
		}
	}
}

// disconnect 結束 P2P 連線，清理所有資源並恢復初始 UI 狀態。
// 清理範圍：forward listeners、proxy listener、control channel、PeerConnection、adb disconnect。
func (t *pairTab) disconnect() {
	t.cleanup()
	t.latencyMs.Store(0)

	// 清除 UI 編輯器內容，恢復初始狀態
	t.cliOfferOutEditor.SetText("")
	t.cliAnswerInEditor.SetText("")
	t.srvOfferInEditor.SetText("")
	t.srvAnswerOutEditor.SetText("")
	t.srvProcessedOffer = ""
	t.cliProcessedAnswer = ""

	t.mu.Lock()
	t.remoteHostname = ""
	t.remoteAddr = ""
	t.turnWarning = ""
	t.status = msg().Pair.StatusNotStarted
	isServer := t.isServer
	t.mu.Unlock()
	t.window.Invalidate()

	if !isServer {
		t.maybeStartOfferPrewarm()
	}
}

func (t *pairTab) cleanup() {
	// 鎖內僅擷取資源引用並重置狀態，避免持鎖時呼叫可能阻塞的 Close()
	t.mu.Lock()
	cancel := t.cancel
	dpm := t.dpm
	controlCh := t.controlCh
	pm := t.pm
	prewarmControl := t.prewarmControl
	prewarmPM := t.prewarmPM
	t.cancel = nil
	t.dpm = nil
	t.controlCh = nil
	t.pm = nil
	t.prewarmControl = nil
	t.prewarmPM = nil
	t.prewarmOffer = ""
	t.prewarmDone = nil
	t.prewarmInFlight = false
	t.connected = false
	t.relayed = false
	t.srvDevices = nil
	t.mu.Unlock()

	// 鎖外依序關閉資源：
	// 1. cancel 停止背景 goroutine（非阻塞）
	// 2. dpm.Close() 停止所有 per-device proxy（含 auto adb disconnect）
	// 3. pm.Close() → close(doneCh)，解除 controlCh.Close() 的 pendingChannel.wait() 阻塞
	// 4. controlCh.Close() 此時 wait() 已可立即回傳
	if cancel != nil {
		cancel()
	}
	if dpm != nil {
		dpm.Close()
	}
	if pm != nil {
		pm.Close()
	}
	if controlCh != nil {
		controlCh.Close()
	}
	if prewarmPM != nil {
		prewarmPM.Close()
	}
	if prewarmControl != nil {
		prewarmControl.Close()
	}
}
