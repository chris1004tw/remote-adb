package agent

import "testing"

func TestNew(t *testing.T) {
	cfg := Config{
		ServerURL: "ws://localhost:8080",
		Token:     "test-token",
		HostID:    "test-host",
		ADBAddr:   "127.0.0.1:5037",
	}
	a := New(cfg)
	if a == nil {
		t.Fatal("New() 應回傳非 nil Agent")
	}
	if a.config.ServerURL != cfg.ServerURL {
		t.Errorf("ServerURL = %q, 預期 %q", a.config.ServerURL, cfg.ServerURL)
	}
	if a.config.Token != cfg.Token {
		t.Errorf("Token = %q, 預期 %q", a.config.Token, cfg.Token)
	}
	if a.hostname == "" {
		t.Error("hostname 不應為空")
	}
}
