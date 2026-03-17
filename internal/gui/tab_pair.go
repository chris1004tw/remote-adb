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

	"github.com/chris1004tw/remote-adb/internal/bridge"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
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

	// 從背景 goroutine 延遲設定 Editor 文字。
	// Gio Editor 非 goroutine-safe，直接從背景 goroutine 呼叫 SetText 會與
	// UI 執行緒的 layout 競爭 Editor 內部 buffer，造成 slice bounds out of range panic。
	// goroutine 將文字存入此欄位（mutex 保護），layout() 在 UI 執行緒消費並呼叫 SetText。
	pendingCliOfferSet  *string // 非 nil 時，下一幀對 cliOfferOutEditor 呼叫 SetText
	pendingSrvAnswerSet *string // 非 nil 時，下一幀對 srvAnswerOutEditor 呼叫 SetText

	// 背景預產生邀請碼（完整 ICE gathering）
	prewarmInFlight bool
	prewarmDone     chan struct{}
	prewarmPM       *webrtc.PeerManager
	prewarmControl  io.ReadWriteCloser
	prewarmOffer    string

	// 防重入旗標（mutex 保護）
	generatingOffer bool // 防止 clientGenerateOffer 重複觸發
	processingOffer bool // 防止 serverProcessOffer 並行執行

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

	// 結束連線 / 清除 / 重新加入 ADB / 重新整理設備 按鈕
	disconnectBtn     widget.Clickable
	clearBtn          widget.Clickable
	reconnectADBBtn   widget.Clickable
	refreshDevicesBtn widget.Clickable

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

	// 消費背景 goroutine 的延遲操作（必須在 UI 執行緒執行）
	t.mu.Lock()
	clip := t.pendingClipboard
	t.pendingClipboard = ""
	cliOfferSet := t.pendingCliOfferSet
	srvAnswerSet := t.pendingSrvAnswerSet
	t.pendingCliOfferSet = nil
	t.pendingSrvAnswerSet = nil
	t.mu.Unlock()
	if clip != "" {
		gtx.Execute(clipboard.WriteCmd{
			Type: "application/text",
			Data: io.NopCloser(strings.NewReader(clip)),
		})
	}
	// Editor.SetText 必須在 UI 執行緒呼叫，避免與 layout 的 text shaping 競爭 buffer
	if cliOfferSet != nil {
		t.cliOfferOutEditor.SetText(*cliOfferSet)
	}
	if srvAnswerSet != nil {
		t.srvAnswerOutEditor.SetText(*srvAnswerSet)
	}

	// 角色切換：切換時呼叫 disconnect() 清理舊資源與 Editor 文字，
	// 避免殘留失效的邀請碼/回應碼（對應的 PeerManager 已不存在）誤導使用者。
	// disconnect() 內部會根據新的 isServer 值決定是否啟動 prewarm。
	for t.clientBtn.Clicked(gtx) {
		if t.isServer {
			t.isServer = false
			t.disconnect()
		}
	}
	for t.serverBtn.Clicked(gtx) {
		if !t.isServer {
			t.isServer = true
			t.disconnect()
		}
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
						c = colorStatusOnline
					}
					return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return statusText(gtx, th, msg().Common.StatusPrefix+status, c)
					})
				})

				// TURN 不可用警告（橘色文字）
				if turnWarning != "" {
					widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return statusText(gtx, th, turnWarning, colorWarning)
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
	t.processingOffer = false
	t.generatingOffer = false
	t.mu.Unlock()

	// 在 goroutine 中關閉資源，避免 pm.Close()（pion PeerConnection 關閉 ICE/DTLS）
	// 阻塞 UI 執行緒導致視窗 "Not Responding"。
	// 上方 lock+nil 已即時更新狀態，Close 可安全非同步執行。
	go func() {
		// 關閉順序：
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
	}()
}

// onConnectedHandler 是 pm.OnConnected 的共用回呼，主控端與被控端共用。
// 記錄 relay 狀態並通知 UI 刷新。
func (t *pairTab) onConnectedHandler(relayed bool) {
	t.mu.Lock()
	t.relayed = relayed
	t.mu.Unlock()
	if relayed {
		slog.Info("P2P connection is relayed through TURN server")
	}
	t.window.Invalidate()
}
