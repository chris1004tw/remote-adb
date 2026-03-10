// Package webrtc 封裝 pion/webrtc，提供 PeerConnection 和 DataChannel 管理。
//
// 本套件負責 WebRTC 連線的建立、SDP 交換（Offer/Answer）、ICE candidate 處理，
// 以及 DataChannel 的建立與 detach。detach 模式讓 DataChannel 回傳原始的
// io.ReadWriteCloser，而非 pion 預設的 message-based API，使其能直接作為
// TCP 流量的轉發管道，與 proxy 套件無縫銜接。
package webrtc

import (
	pionwebrtc "github.com/pion/webrtc/v4"
)

// ICEConfig 定義 ICE（Interactive Connectivity Establishment）伺服器設定。
//
// WebRTC 連線建立時需要透過 ICE 框架來穿透 NAT，涉及兩種伺服器：
//   - STUN（Session Traversal Utilities for NAT）：用於 NAT 穿透偵測，
//     讓客戶端得知自己的公網 IP 與 port，成本低但無法穿透對稱型 NAT。
//   - TURN（Traversal Using Relays around NAT）：當 STUN 無法直連時的備援，
//     所有流量透過 TURN server 中繼轉發，能穿透任何 NAT 但需要自建伺服器承擔頻寬。
//
// 一般情境只需 STUN 即可；若雙方皆在嚴格 NAT（如企業防火牆）後方，才需啟用 TURN。
type ICEConfig struct {
	STUNServers []string     // STUN 伺服器 URI 列表，格式如 "stun:host:port"
	TURNServers []TURNServer // TURN 伺服器列表，需提供帳號密碼驗證
}

// TURNServer 定義單一 TURN 伺服器的連線資訊。
// TURN 採用帳號密碼認證，防止未授權使用者消耗伺服器頻寬。
type TURNServer struct {
	URL        string // TURN 伺服器 URI，格式如 "turn:host:port" 或 "turns:host:port"（TLS）
	Username   string // TURN 認證使用者名稱
	Credential string // TURN 認證密碼
}

// DefaultICEConfig 回傳使用 Google 公開 STUN server 的預設設定。
//
// Google 提供的 stun.l.google.com:19302 是全球可用的免費 STUN 服務，
// 穩定性高且延遲低，適合作為預設值。在大多數家用 NAT 環境下，
// 僅靠 STUN 即可完成 P2P 穿透，不需要額外的 TURN 伺服器。
// 若使用者有自建 TURN 需求，可透過 ICEConfig 額外設定。
func DefaultICEConfig() ICEConfig {
	return ICEConfig{
		STUNServers: []string{"stun:stun.l.google.com:19302"},
	}
}

// toWebRTCConfig 將自定義的 ICEConfig 轉換為 pion/webrtc 原生的 Configuration。
//
// 轉換邏輯：
//   - 所有 STUN URI 合併為單一 ICEServer 條目（STUN 不需要認證資訊）
//   - 每個 TURN 伺服器各自成為獨立的 ICEServer 條目（因為各自有不同的帳密）
//   - TURN 統一使用密碼認證（ICECredentialTypePassword）
func (c ICEConfig) toWebRTCConfig() pionwebrtc.Configuration {
	var iceServers []pionwebrtc.ICEServer

	// STUN 伺服器不需認證，可將所有 URI 合併為同一條目
	if len(c.STUNServers) > 0 {
		iceServers = append(iceServers, pionwebrtc.ICEServer{
			URLs: c.STUNServers,
		})
	}

	// 每個 TURN 伺服器有獨立帳密，需分別建立 ICEServer 條目
	for _, turn := range c.TURNServers {
		iceServers = append(iceServers, pionwebrtc.ICEServer{
			URLs:           []string{turn.URL},
			Username:       turn.Username,
			Credential:     turn.Credential,
			CredentialType: pionwebrtc.ICECredentialTypePassword,
		})
	}

	return pionwebrtc.Configuration{
		ICEServers: iceServers,
	}
}
