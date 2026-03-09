package directsrv

import (
	"testing"
)

// TestStartMDNS_NoError 測試 mDNS 服務能夠正常建立且不回傳錯誤。
func TestStartMDNS_NoError(t *testing.T) {
	shutdown, err := StartMDNS("test-host", 9000)
	if err != nil {
		t.Fatalf("StartMDNS 應成功，但回傳錯誤: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown 函式不應為 nil")
	}
	// 關閉不應 panic
	shutdown()
}

// TestStartMDNS_ShutdownIdempotent 測試重複呼叫 shutdown 不會 panic。
func TestStartMDNS_ShutdownIdempotent(t *testing.T) {
	shutdown, err := StartMDNS("test-host", 9001)
	if err != nil {
		t.Fatalf("StartMDNS 應成功，但回傳錯誤: %v", err)
	}
	// 連續呼叫兩次 shutdown，不應 panic
	shutdown()
	shutdown()
}
