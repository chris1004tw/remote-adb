// peer.go 實作 WebRTC PeerConnection 的完整生命週期管理。
//
// 核心概念：
//   - DataChannel detach 模式：pion/webrtc 預設使用 message-based API（OnMessage 回呼），
//     但 ADB 流量是連續的 TCP 位元串流，不適合逐訊息處理。啟用 detach 後，
//     DataChannel 會回傳 io.ReadWriteCloser（底層為 SCTP stream），
//     可直接當成 TCP 連線般進行 Read/Write，大幅簡化 proxy 橋接邏輯。
//
//   - SDP 交換流程（三步驟）：
//     1. Client 呼叫 CreateOffer() → 產生 SDP Offer → 透過信令傳給 Agent
//     2. Agent 呼叫 HandleOffer(sdp) → 設定 RemoteDescription → 產生 Answer → 回傳
//     3. Client 呼叫 HandleAnswer(sdp) → 設定 RemoteDescription → 連線建立
//
//   - ICE candidate 交換可透過 Trickle ICE（即時交換）或等 gathering 完成後
//     將所有 candidate 嵌入 SDP 一次傳送（本專案兩種皆支援）。

package webrtc

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/datachannel"
	pionwebrtc "github.com/pion/webrtc/v4"
)

// ErrPeerClosed 表示 PeerManager 已關閉，所有尚未就緒的 DataChannel 不再等待。
var ErrPeerClosed = errors.New("peer connection closed")

// PeerManager 管理與單一遠端對等方的 WebRTC 連線。
//
// 職責包含：
//   - 建立與管理底層 PeerConnection
//   - SDP Offer/Answer 交換
//   - ICE candidate 處理
//   - DataChannel 建立與 detach（取得 io.ReadWriteCloser）
//   - 連線狀態監控與斷線通知
//
// 執行緒安全：所有公開方法皆可安全地從不同 goroutine 呼叫。
type PeerManager struct {
	pc     *pionwebrtc.PeerConnection // 底層 pion PeerConnection
	config ICEConfig                  // ICE 伺服器設定（STUN/TURN）
	doneCh chan struct{}              // 關閉通知，Close() 時 close，讓所有 pendingChannel.wait() 解除阻塞

	mu             sync.Mutex                                 // 保護以下回呼函式與 closed 旗標
	onChannelFn    func(label string, rwc io.ReadWriteCloser) // 對方開啟 DataChannel 時的回呼
	onDisconnectFn func()                                     // 連線斷開時的回呼
	onConnectedFn  func(relayed bool)                         // 連線建立時的回呼（relayed 表示是否走 TURN 中繼）
	closed         bool                                       // 防止重複關閉
}

// NewPeerManager 建立一個新的 PeerManager。
//
// 初始化流程：
//  1. 啟用 detach 模式（DetachDataChannels），使 DataChannel 開啟後可取得
//     原始 SCTP stream 的 io.ReadWriteCloser，而非預設的 message-based 回呼
//  2. 用自訂 SettingEngine 建立 API，再透過 API 建立 PeerConnection
//  3. 註冊兩個核心回呼：連線狀態監聽、遠端 DataChannel 開啟監聽
func NewPeerManager(config ICEConfig) (*PeerManager, error) {
	// 啟用 DataChannel detach 模式：讓 dc.Detach() 回傳 io.ReadWriteCloser，
	// 可直接用 Read/Write 操作位元串流，適合 TCP 流量轉發場景
	se := pionwebrtc.SettingEngine{}
	se.DetachDataChannels()

	api := pionwebrtc.NewAPI(pionwebrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(config.toWebRTCConfig())
	if err != nil {
		return nil, fmt.Errorf("建立 PeerConnection 失敗: %w", err)
	}

	pm := &PeerManager{
		pc:     pc,
		config: config,
		doneCh: make(chan struct{}),
	}

	// 監聯連線狀態變化：
	// - Connected：通知上層連線已建立，並回報是否走 TURN 中繼
	// - Failed/Disconnected/Closed：通知上層連線已斷開
	pc.OnConnectionStateChange(func(state pionwebrtc.PeerConnectionState) {
		slog.Debug("PeerConnection state changed", "state", state.String())
		switch state {
		case pionwebrtc.PeerConnectionStateConnected:
			relayed := pm.IsRelayed()
			slog.Info("PeerConnection connected", "relayed", relayed)
			pm.mu.Lock()
			fn := pm.onConnectedFn
			pm.mu.Unlock()
			if fn != nil {
				fn(relayed)
			}
		case pionwebrtc.PeerConnectionStateFailed,
			pionwebrtc.PeerConnectionStateDisconnected,
			pionwebrtc.PeerConnectionStateClosed:
			pm.mu.Lock()
			fn := pm.onDisconnectFn
			pm.onDisconnectFn = nil // 確保只觸發一次，避免 Disconnected→Failed→Closed 連續轉換重複呼叫
			pm.mu.Unlock()
			if fn != nil {
				fn()
			}
		}
	})

	// 監聽對方開啟的 DataChannel（Agent 端主要走此路徑）
	// 當遠端建立 DataChannel 時，等待 OnOpen 觸發後 detach 取得原始串流
	pc.OnDataChannel(func(dc *pionwebrtc.DataChannel) {
		dc.OnOpen(func() {
			pm.mu.Lock()
			fn := pm.onChannelFn
			pm.mu.Unlock()

			if fn == nil {
				return
			}

			// detach 取得底層 SCTP stream，轉為 io.ReadWriteCloser
			raw, err := dc.Detach()
			if err != nil {
				slog.Error("DataChannel detach failed", "label", dc.Label(), "error", err)
				return
			}
			fn(dc.Label(), raw)
		})
	})

	return pm, nil
}

// CreateOffer 產生 SDP Offer 並設定為 local description。
// 回傳 SDP 字串，呼叫者需透過信令將其傳送給遠端。
//
// 這是 SDP 三步驟交換的第一步（Client 端呼叫）：
//  1. CreateOffer → 產生包含本地媒體能力的 SDP
//  2. SetLocalDescription → 觸發 ICE agent 開始蒐集 candidate
//  3. 等待 GatheringComplete → 確保所有 ICE candidate 已嵌入 SDP
//     （避免 Trickle ICE 的額外信令往返，簡化手動 SDP 配對流程）
//  4. 回傳完整 SDP（含所有 candidate）→ 呼叫者傳送給遠端
func (pm *PeerManager) CreateOffer() (string, error) {
	offer, err := pm.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("建立 offer 失敗: %w", err)
	}

	if err := pm.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("設定 local description 失敗: %w", err)
	}

	pm.waitGatheringComplete("offer")

	local := pm.pc.LocalDescription()
	if local == nil {
		return "", errors.New("local description is nil after CreateOffer")
	}
	return local.SDP, nil
}

// CreateOfferWithGatherTimeout 產生 SDP Offer，但只等待指定時間蒐集 ICE candidate。
// timeout 到達後會回傳目前已蒐集到的 candidate，適用於需要快速產生邀請碼的場景。
func (pm *PeerManager) CreateOfferWithGatherTimeout(timeout time.Duration) (string, error) {
	offer, err := pm.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("建立 offer 失敗: %w", err)
	}

	if err := pm.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("設定 local description 失敗: %w", err)
	}

	pm.waitGatheringCompleteWithTimeout("offer", timeout)

	local := pm.pc.LocalDescription()
	if local == nil {
		return "", errors.New("local description is nil after CreateOfferWithGatherTimeout")
	}
	return local.SDP, nil
}

// HandleOffer 處理遠端的 SDP Offer，產生 Answer 並回傳。
//
// 這是 SDP 三步驟交換的第二步（Agent 端呼叫）：
//  1. SetRemoteDescription(offer) → 解析對方的媒體能力與 ICE candidate
//  2. CreateAnswer → 根據對方 Offer 產生相容的 Answer SDP
//  3. SetLocalDescription(answer) → 觸發本地 ICE gathering
//  4. 等待 GatheringComplete → 確保 Answer 包含所有本地 candidate
//  5. 回傳完整 Answer SDP → 呼叫者傳回給 Client
func (pm *PeerManager) HandleOffer(sdp string) (string, error) {
	offer := pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeOffer,
		SDP:  sdp,
	}

	if err := pm.pc.SetRemoteDescription(offer); err != nil {
		return "", fmt.Errorf("設定 remote description 失敗: %w", err)
	}

	answer, err := pm.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("建立 answer 失敗: %w", err)
	}

	if err := pm.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("設定 local description 失敗: %w", err)
	}

	pm.waitGatheringComplete("answer")

	local := pm.pc.LocalDescription()
	if local == nil {
		return "", errors.New("local description is nil after HandleOffer")
	}
	return local.SDP, nil
}

// defaultGatherTimeout 是 ICE gathering 的最大等待時間。
// 當 STUN/TURN server 全不可達時，gathering 可能永遠不完成，
// 此上限確保所有 CreateOffer/HandleOffer 路徑不會永久阻塞。
const defaultGatherTimeout = 30 * time.Second

// waitGatheringComplete 等待 ICE candidate 蒐集完成，最多等待 defaultGatherTimeout。
func (pm *PeerManager) waitGatheringComplete(phase string) {
	pm.waitGatheringCompleteWithTimeout(phase, defaultGatherTimeout)
}

// waitGatheringCompleteWithTimeout 等待 ICE candidate 蒐集，逾時或 PeerManager 關閉時提前返回。
// timeout <= 0 時使用 defaultGatherTimeout 作為上限。
func (pm *PeerManager) waitGatheringCompleteWithTimeout(phase string, timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultGatherTimeout
	}

	start := time.Now()
	done := pionwebrtc.GatheringCompletePromise(pm.pc)
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		slog.Debug("ICE gathering complete", "phase", phase, "elapsed_ms", time.Since(start).Milliseconds())
	case <-pm.doneCh:
		slog.Warn(
			"ICE gathering aborted: peer closed",
			"phase", phase,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
	case <-timer.C:
		slog.Warn(
			"ICE gathering timeout; proceeding with partial candidates",
			"phase", phase,
			"elapsed_ms", time.Since(start).Milliseconds(),
			"timeout_ms", timeout.Milliseconds(),
			"state", pm.pc.ICEGatheringState().String(),
		)
	}
}

// HandleAnswer 處理遠端的 SDP Answer。
//
// 這是 SDP 三步驟交換的第三步（Client 端呼叫）：
// 設定 RemoteDescription 後，雙方的 ICE agent 會開始配對 candidate，
// 成功配對後 PeerConnection 狀態會轉為 Connected，DataChannel 隨即可用。
func (pm *PeerManager) HandleAnswer(sdp string) error {
	answer := pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeAnswer,
		SDP:  sdp,
	}
	return pm.pc.SetRemoteDescription(answer)
}

// AddICECandidate 加入遠端的 ICE candidate（Trickle ICE 模式使用）。
// 當透過信令逐一接收遠端 candidate 時呼叫此方法，讓 ICE agent 嘗試配對。
// 在手動 SDP 配對模式下，candidate 已嵌入 SDP，通常不需要額外呼叫此方法。
func (pm *PeerManager) AddICECandidate(candidate string, sdpMid string, sdpMLineIndex uint16) error {
	return pm.pc.AddICECandidate(pionwebrtc.ICECandidateInit{
		Candidate:     candidate,
		SDPMid:        &sdpMid,
		SDPMLineIndex: &sdpMLineIndex,
	})
}

// OnICECandidate 註冊本地 ICE candidate 產生時的回呼（Trickle ICE 模式使用）。
// 每當 ICE agent 發現新的 candidate（host/srflx/relay）時觸發，
// 呼叫者需透過信令將 candidate 傳送給遠端。
// 當 c 為 nil 時表示 gathering 結束，此處已過濾不會觸發 handler。
func (pm *PeerManager) OnICECandidate(handler func(candidate string, sdpMid string, sdpMLineIndex uint16)) {
	pm.pc.OnICECandidate(func(c *pionwebrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		mid := ""
		if init.SDPMid != nil {
			mid = *init.SDPMid
		}
		var idx uint16
		if init.SDPMLineIndex != nil {
			idx = *init.SDPMLineIndex
		}
		handler(init.Candidate, mid, idx)
	})
}

// OpenChannel 建立一條新的 DataChannel（使用 detach 模式）。
//
// 非阻塞設計：此方法立即回傳 io.ReadWriteCloser（實際為 pendingChannel），
// 不需等待 SDP 交換完成。底層 DataChannel 尚未開啟時：
//   - Read/Write 呼叫會阻塞在 pendingChannel.wait()，等待 readyCh 被 close
//   - 當 SDP 交換完成且 ICE 連線建立後，DataChannel OnOpen 觸發 → detach → setReady
//   - 此後所有 Read/Write 直接委派給底層 SCTP stream
//
// 典型使用流程：
//  1. rwc := OpenChannel("adb-device-xxx")  ← 立即回傳，不阻塞
//  2. sdp := CreateOffer()                  ← 產生包含此 channel 的 Offer
//  3. 透過信令交換 SDP                       ← 網路往返
//  4. rwc.Read/Write(...)                   ← 此時才真正開始資料傳輸
func (pm *PeerManager) OpenChannel(label string) (io.ReadWriteCloser, error) {
	dc, err := pm.pc.CreateDataChannel(label, &pionwebrtc.DataChannelInit{})
	if err != nil {
		return nil, fmt.Errorf("建立 DataChannel 失敗: %w", err)
	}

	// 建立 pendingChannel 包裝，readyCh 用於同步等待 DataChannel 開啟，
	// doneCh 用於 PeerManager 關閉時解除阻塞
	pch := &pendingChannel{
		readyCh: make(chan struct{}),
		doneCh:  pm.doneCh,
	}

	dc.OnOpen(func() {
		raw, detachErr := dc.Detach()
		if detachErr != nil {
			slog.Error("DataChannel detach failed", "label", label, "error", detachErr)
			pch.setError(detachErr)
			return
		}
		pch.setReady(raw)
	})

	return pch, nil
}

// pendingChannel 包裝一個尚未就緒的 DataChannel，實作 io.ReadWriteCloser 介面。
//
// 設計意圖：讓 OpenChannel 能在 SDP 交換前就回傳可用的介面，
// 呼叫者不需要關心底層連線何時真正建立。同步機制透過兩個 channel：
//   - readyCh：DataChannel 就緒信號。setReady/setError 在賦值 rwc/err 後 close，
//     Go 的 channel close 提供 happens-before 保證，故不需額外 mutex。
//   - doneCh：PeerManager 關閉信號。Close() 時 close，讓 wait() 解除阻塞。
//
// 狀態轉換：
//   - DataChannel 開啟前：Read/Write/Close 呼叫 wait() → select 阻塞
//   - DataChannel 開啟後：close(readyCh) → 所有等待者被喚醒，讀取 rwc/err
//   - PeerManager 關閉：close(doneCh) → 所有等待者回傳 ErrPeerClosed
type pendingChannel struct {
	readyCh chan struct{}               // DataChannel 就緒信號，close 後表示 rwc 或 err 已設定
	doneCh  chan struct{}               // PeerManager 關閉信號，close 後 wait() 回傳 ErrPeerClosed
	rwc     datachannel.ReadWriteCloser // detach 後的底層 SCTP stream
	err     error                       // detach 失敗時的錯誤
}

// setReady 在 DataChannel 成功 detach 後呼叫，儲存底層串流並喚醒所有等待者。
// close(readyCh) 提供 happens-before 保證：rwc 的賦值對所有 <-readyCh 接收方可見。
func (p *pendingChannel) setReady(rwc datachannel.ReadWriteCloser) {
	p.rwc = rwc
	close(p.readyCh)
}

// setError 在 detach 失敗時呼叫，儲存錯誤並喚醒所有等待者。
// close(readyCh) 提供 happens-before 保證：err 的賦值對所有 <-readyCh 接收方可見。
func (p *pendingChannel) setError(err error) {
	p.err = err
	close(p.readyCh)
}

// wait 阻塞直到 DataChannel 就緒、失敗、或 PeerManager 關閉。
// 若 readyCh 或 doneCh 已被 close，則立即回傳。
func (p *pendingChannel) wait() (datachannel.ReadWriteCloser, error) {
	select {
	case <-p.readyCh:
		if p.err != nil {
			return nil, p.err
		}
		return p.rwc, nil
	case <-p.doneCh:
		return nil, ErrPeerClosed
	}
}

// Read 實作 io.Reader，阻塞等待 DataChannel 就緒後委派給底層串流。
func (p *pendingChannel) Read(buf []byte) (int, error) {
	rwc, err := p.wait()
	if err != nil {
		return 0, err
	}
	return rwc.Read(buf)
}

// Write 實作 io.Writer，阻塞等待 DataChannel 就緒後委派給底層串流。
func (p *pendingChannel) Write(data []byte) (int, error) {
	rwc, err := p.wait()
	if err != nil {
		return 0, err
	}
	return rwc.Write(data)
}

// Close 實作 io.Closer，阻塞等待 DataChannel 就緒後關閉底層串流。
func (p *pendingChannel) Close() error {
	rwc, err := p.wait()
	if err != nil {
		return err
	}
	return rwc.Close()
}

// OnChannel 註冊「對方開啟 DataChannel」的回呼。
// handler 會收到 channel label 和已 detach 的 io.ReadWriteCloser。
// 此方法通常由 Agent 端使用：Client 透過 OpenChannel 建立 channel，
// Agent 透過 OnChannel 接收並開始轉發 ADB 流量。
func (pm *PeerManager) OnChannel(handler func(label string, rwc io.ReadWriteCloser)) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onChannelFn = handler
}

// OnDisconnect 註冊斷線回呼。
func (pm *PeerManager) OnDisconnect(handler func()) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onDisconnectFn = handler
}

// OnConnected 註冊連線建立時的回呼。
// relayed 參數表示連線是否透過 TURN 中繼伺服器（relay candidate），
// 若為 true，表示 STUN 穿透失敗，雙方透過中繼通訊。
// 回呼在 PeerConnectionStateConnected 狀態時觸發。
func (pm *PeerManager) OnConnected(handler func(relayed bool)) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onConnectedFn = handler
}

// IsRelayed 檢查目前選定的 ICE candidate pair 是否為 relay 類型。
// 若本地或遠端 candidate 任一為 relay（TURN 中繼），回傳 true。
// 連線尚未建立或無法取得 candidate pair 時回傳 false。
//
// 查詢路徑與 GetRemoteAddr 相同：
// PeerConnection → SCTP → DTLS Transport → ICE Transport → 選定的 candidate pair
func (pm *PeerManager) IsRelayed() bool {
	sctp := pm.pc.SCTP()
	if sctp == nil {
		return false
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return false
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return false
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return false
	}
	if pair.Local != nil && pair.Local.Typ == pionwebrtc.ICECandidateTypeRelay {
		return true
	}
	if pair.Remote != nil && pair.Remote.Typ == pionwebrtc.ICECandidateTypeRelay {
		return true
	}
	return false
}

// GetRTT 回傳目前成功的 ICE candidate pair 往返延遲（Round-Trip Time）。
// 從 WebRTC 統計資料中找出狀態為 Succeeded 的 candidate pair，取其 RTT。
// 可用於 GUI 顯示連線品質。如果尚無成功配對的 candidate 則回傳 0。
func (pm *PeerManager) GetRTT() time.Duration {
	report := pm.pc.GetStats()
	for _, s := range report {
		cp, ok := s.(pionwebrtc.ICECandidatePairStats)
		if !ok {
			continue
		}
		if cp.State == pionwebrtc.StatsICECandidatePairStateSucceeded && cp.CurrentRoundTripTime > 0 {
			return time.Duration(cp.CurrentRoundTripTime * float64(time.Second))
		}
	}
	return 0
}

// GetRemoteAddr 回傳目前連線的遠端 IP:port。
// 如果尚未建立連線則回傳空字串。
//
// 查詢路徑：PeerConnection → SCTP → DTLS Transport → ICE Transport → 選定的 candidate pair
// 這條鏈路反映了 WebRTC 的協定堆疊：ICE（網路穿透）→ DTLS（加密）→ SCTP（可靠傳輸）
func (pm *PeerManager) GetRemoteAddr() string {
	sctp := pm.pc.SCTP()
	if sctp == nil {
		return ""
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return ""
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return ""
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return ""
	}
	if pair.Remote == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d", pair.Remote.Address, pair.Remote.Port)
}

// Close 關閉 PeerConnection 及所有 DataChannel。
// 同時 close doneCh 通知所有 pendingChannel.wait() 解除阻塞。
// 使用 closed 旗標防止重複關閉（PeerConnection.Close 不是冪等的）。
//
// 注意：必須先 unlock 再呼叫 pc.Close()，因為 pc.Close() 會觸發
// OnPeerConnectionStateChange callback，而 callback 內會 lock pm.mu
// 讀取 onDisconnectFn。若持鎖呼叫 pc.Close() 會造成 deadlock。
func (pm *PeerManager) Close() error {
	pm.mu.Lock()
	if pm.closed {
		pm.mu.Unlock()
		return nil
	}
	pm.closed = true
	close(pm.doneCh) // 讓所有尚未就緒的 pendingChannel 立即回傳 ErrPeerClosed
	pm.mu.Unlock()
	return pm.pc.Close()
}
