// Package webrtc 封裝 pion/webrtc，提供 PeerConnection 和 DataChannel 管理。
package webrtc

import (
	pionwebrtc "github.com/pion/webrtc/v4"
)

// ICEConfig 定義 STUN/TURN 伺服器設定。
type ICEConfig struct {
	STUNServers []string
	TURNServers []TURNServer
}

// TURNServer 定義單一 TURN 伺服器的設定。
type TURNServer struct {
	URL        string
	Username   string
	Credential string
}

// DefaultICEConfig 回傳使用 Google 公開 STUN server 的預設設定。
func DefaultICEConfig() ICEConfig {
	return ICEConfig{
		STUNServers: []string{"stun:stun.l.google.com:19302"},
	}
}

// toWebRTCConfig 將 ICEConfig 轉換為 pion/webrtc 的 Configuration。
func (c ICEConfig) toWebRTCConfig() pionwebrtc.Configuration {
	var iceServers []pionwebrtc.ICEServer

	if len(c.STUNServers) > 0 {
		iceServers = append(iceServers, pionwebrtc.ICEServer{
			URLs: c.STUNServers,
		})
	}

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
