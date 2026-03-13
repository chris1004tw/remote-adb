package webrtc

import (
	"testing"

	pionwebrtc "github.com/pion/webrtc/v4"
)

func TestDefaultICEConfig(t *testing.T) {
	cfg := DefaultICEConfig()

	if len(cfg.STUNServers) == 0 {
		t.Fatal("DefaultICEConfig should have at least one STUN server")
	}
	if cfg.STUNServers[0] != "stun:stun.l.google.com:19302" {
		t.Errorf("STUN server: got %q, want %q", cfg.STUNServers[0], "stun:stun.l.google.com:19302")
	}
	if len(cfg.TURNServers) != 0 {
		t.Errorf("DefaultICEConfig should have no TURN servers, got %d", len(cfg.TURNServers))
	}
}

func TestToWebRTCConfig_STUNOnly(t *testing.T) {
	cfg := ICEConfig{
		STUNServers: []string{"stun:stun1.example.com:3478", "stun:stun2.example.com:3478"},
	}

	wc := cfg.toWebRTCConfig()

	if len(wc.ICEServers) != 1 {
		t.Fatalf("expected 1 ICEServer entry (STUN combined), got %d", len(wc.ICEServers))
	}
	if len(wc.ICEServers[0].URLs) != 2 {
		t.Errorf("expected 2 STUN URLs in single entry, got %d", len(wc.ICEServers[0].URLs))
	}
	if wc.ICEServers[0].Username != "" {
		t.Error("STUN entry should not have username")
	}
}

func TestToWebRTCConfig_TURNOnly(t *testing.T) {
	cfg := ICEConfig{
		TURNServers: []TURNServer{
			{URL: "turn:relay.example.com:3478", Username: "user1", Credential: "pass1"},
		},
	}

	wc := cfg.toWebRTCConfig()

	if len(wc.ICEServers) != 1 {
		t.Fatalf("expected 1 ICEServer entry, got %d", len(wc.ICEServers))
	}
	s := wc.ICEServers[0]
	if len(s.URLs) != 1 || s.URLs[0] != "turn:relay.example.com:3478" {
		t.Errorf("unexpected TURN URL: %v", s.URLs)
	}
	if s.Username != "user1" || s.Credential != "pass1" {
		t.Errorf("unexpected credentials: user=%q, cred=%v", s.Username, s.Credential)
	}
	if s.CredentialType != pionwebrtc.ICECredentialTypePassword {
		t.Errorf("expected ICECredentialTypePassword, got %v", s.CredentialType)
	}
}

func TestToWebRTCConfig_Mixed(t *testing.T) {
	cfg := ICEConfig{
		STUNServers: []string{"stun:stun.example.com:3478"},
		TURNServers: []TURNServer{
			{URL: "turn:relay1.example.com:3478", Username: "u1", Credential: "p1"},
			{URL: "turns:relay2.example.com:5349", Username: "u2", Credential: "p2"},
		},
	}

	wc := cfg.toWebRTCConfig()

	// 1 STUN entry + 2 TURN entries = 3 total
	if len(wc.ICEServers) != 3 {
		t.Fatalf("expected 3 ICEServer entries, got %d", len(wc.ICEServers))
	}

	// 第一個是 STUN
	if wc.ICEServers[0].Username != "" {
		t.Error("first entry (STUN) should not have username")
	}

	// 第二和第三是 TURN，各自有獨立帳密
	if wc.ICEServers[1].Username != "u1" {
		t.Errorf("second entry username: got %q, want %q", wc.ICEServers[1].Username, "u1")
	}
	if wc.ICEServers[2].URLs[0] != "turns:relay2.example.com:5349" {
		t.Errorf("third entry URL: got %q", wc.ICEServers[2].URLs[0])
	}
}

func TestToWebRTCConfig_Empty(t *testing.T) {
	cfg := ICEConfig{}

	wc := cfg.toWebRTCConfig()

	if len(wc.ICEServers) != 0 {
		t.Errorf("empty config should produce 0 ICEServers, got %d", len(wc.ICEServers))
	}
}
