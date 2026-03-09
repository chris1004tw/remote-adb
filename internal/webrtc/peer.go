package webrtc

import (
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/pion/datachannel"
	pionwebrtc "github.com/pion/webrtc/v4"
)

// PeerManager 管理與單一遠端對等方的 WebRTC 連線。
type PeerManager struct {
	pc     *pionwebrtc.PeerConnection
	config ICEConfig

	mu             sync.Mutex
	onChannelFn    func(label string, rwc io.ReadWriteCloser)
	onDisconnectFn func()
	closed         bool
}

// NewPeerManager 建立一個新的 PeerManager。
func NewPeerManager(config ICEConfig) (*PeerManager, error) {
	// 啟用 DataChannel detach 模式，取得 io.ReadWriteCloser
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
	}

	// 監聽連線狀態變化
	pc.OnConnectionStateChange(func(state pionwebrtc.PeerConnectionState) {
		slog.Debug("PeerConnection 狀態變化", "state", state.String())
		if state == pionwebrtc.PeerConnectionStateFailed ||
			state == pionwebrtc.PeerConnectionStateDisconnected ||
			state == pionwebrtc.PeerConnectionStateClosed {
			pm.mu.Lock()
			fn := pm.onDisconnectFn
			pm.mu.Unlock()
			if fn != nil {
				fn()
			}
		}
	})

	// 監聽對方開啟的 DataChannel
	pc.OnDataChannel(func(dc *pionwebrtc.DataChannel) {
		dc.OnOpen(func() {
			pm.mu.Lock()
			fn := pm.onChannelFn
			pm.mu.Unlock()

			if fn == nil {
				return
			}

			raw, err := dc.Detach()
			if err != nil {
				slog.Error("DataChannel detach 失敗", "label", dc.Label(), "error", err)
				return
			}
			fn(dc.Label(), raw)
		})
	})

	return pm, nil
}

// CreateOffer 產生 SDP Offer 並設定為 local description。
// 回傳 SDP 字串，呼叫者需透過信令將其傳送給遠端。
func (pm *PeerManager) CreateOffer() (string, error) {
	offer, err := pm.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("建立 offer 失敗: %w", err)
	}

	if err := pm.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("設定 local description 失敗: %w", err)
	}

	// 等待 ICE gathering 完成
	<-pionwebrtc.GatheringCompletePromise(pm.pc)

	return pm.pc.LocalDescription().SDP, nil
}

// HandleOffer 處理遠端的 SDP Offer，產生 Answer 並回傳。
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

	<-pionwebrtc.GatheringCompletePromise(pm.pc)

	return pm.pc.LocalDescription().SDP, nil
}

// HandleAnswer 處理遠端的 SDP Answer。
func (pm *PeerManager) HandleAnswer(sdp string) error {
	answer := pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeAnswer,
		SDP:  sdp,
	}
	return pm.pc.SetRemoteDescription(answer)
}

// AddICECandidate 加入遠端的 ICE candidate。
func (pm *PeerManager) AddICECandidate(candidate string, sdpMid string, sdpMLineIndex uint16) error {
	return pm.pc.AddICECandidate(pionwebrtc.ICECandidateInit{
		Candidate:     candidate,
		SDPMid:        &sdpMid,
		SDPMLineIndex: &sdpMLineIndex,
	})
}

// OnICECandidate 註冊本地 ICE candidate 產生時的回呼。
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
// 回傳的 io.ReadWriteCloser 可在 SDP 交換前呼叫（非阻塞建立）。
// Read/Write 操作會自動等到 DataChannel 開啟後才執行。
// 典型流程：OpenChannel → CreateOffer → 交換 SDP → 開始讀寫。
func (pm *PeerManager) OpenChannel(label string) (io.ReadWriteCloser, error) {
	dc, err := pm.pc.CreateDataChannel(label, &pionwebrtc.DataChannelInit{})
	if err != nil {
		return nil, fmt.Errorf("建立 DataChannel 失敗: %w", err)
	}

	pch := &pendingChannel{
		readyCh: make(chan struct{}),
	}

	dc.OnOpen(func() {
		raw, detachErr := dc.Detach()
		if detachErr != nil {
			slog.Error("DataChannel detach 失敗", "label", label, "error", detachErr)
			pch.setError(detachErr)
			return
		}
		pch.setReady(raw)
	})

	return pch, nil
}

// pendingChannel 包裝一個尚未就緒的 DataChannel。
// Read/Write 會阻塞直到底層 DataChannel 開啟。
type pendingChannel struct {
	readyCh chan struct{}

	mu  sync.Mutex
	rwc datachannel.ReadWriteCloser
	err error
}

func (p *pendingChannel) setReady(rwc datachannel.ReadWriteCloser) {
	p.mu.Lock()
	p.rwc = rwc
	p.mu.Unlock()
	close(p.readyCh)
}

func (p *pendingChannel) setError(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
	close(p.readyCh)
}

func (p *pendingChannel) wait() (datachannel.ReadWriteCloser, error) {
	<-p.readyCh
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	return p.rwc, nil
}

func (p *pendingChannel) Read(buf []byte) (int, error) {
	rwc, err := p.wait()
	if err != nil {
		return 0, err
	}
	return rwc.Read(buf)
}

func (p *pendingChannel) Write(data []byte) (int, error) {
	rwc, err := p.wait()
	if err != nil {
		return 0, err
	}
	return rwc.Write(data)
}

func (p *pendingChannel) Close() error {
	rwc, err := p.wait()
	if err != nil {
		return err
	}
	return rwc.Close()
}

// OnChannel 註冊「對方開啟 DataChannel」的回呼。
// handler 會收到 channel label 和 io.ReadWriteCloser。
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

// Close 關閉 PeerConnection 及所有 DataChannel。
func (pm *PeerManager) Close() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.closed {
		return nil
	}
	pm.closed = true
	return pm.pc.Close()
}

// detachedChannel 包裝 datachannel.ReadWriteCloser 為標準的 io.ReadWriteCloser。
type detachedChannel struct {
	dc datachannel.ReadWriteCloser
}

func wrapDetachedChannel(dc datachannel.ReadWriteCloser) io.ReadWriteCloser {
	return &detachedChannel{dc: dc}
}

func (d *detachedChannel) Read(p []byte) (int, error) {
	return d.dc.Read(p)
}

func (d *detachedChannel) Write(p []byte) (int, error) {
	return d.dc.Write(p)
}

func (d *detachedChannel) Close() error {
	return d.dc.Close()
}
