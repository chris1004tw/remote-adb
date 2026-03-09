package signal_test

import (
	"testing"

	"github.com/chris1004tw/remote-adb/internal/signal"
)

func TestPSKAuth_ValidToken(t *testing.T) {
	auth := signal.NewPSKAuth("my-secret")
	if !auth.Validate("my-secret") {
		t.Error("正確的 token 應該通過驗證")
	}
}

func TestPSKAuth_InvalidToken(t *testing.T) {
	auth := signal.NewPSKAuth("my-secret")
	if auth.Validate("wrong-token") {
		t.Error("錯誤的 token 不應該通過驗證")
	}
}

func TestPSKAuth_EmptyToken(t *testing.T) {
	auth := signal.NewPSKAuth("my-secret")
	if auth.Validate("") {
		t.Error("空的 token 不應該通過驗證")
	}
}
