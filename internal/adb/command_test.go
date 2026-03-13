package adb

import (
	"bytes"
	"strings"
	"testing"
)

// TestSendCommand 驗證 SendCommand 輸出格式為 4 位 hex 長度前綴 + 命令字串。
func TestSendCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		{
			name: "標準 host:version 命令",
			cmd:  "host:version",
			want: "000chost:version",
		},
		{
			name: "host:transport 命令",
			cmd:  "host:transport:SN123",
			want: "0014host:transport:SN123",
		},
		{
			name: "空命令",
			cmd:  "",
			want: "0000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := SendCommand(&buf, tt.cmd); err != nil {
				t.Fatalf("SendCommand error: %v", err)
			}
			got := buf.String()
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestReadStatus_OKAY 驗證讀取 "OKAY" 回傳 nil。
func TestReadStatus_OKAY(t *testing.T) {
	r := bytes.NewReader([]byte("OKAY"))
	if err := ReadStatus(r); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

// TestReadStatus_FAIL 驗證讀取 "FAIL" + hex length + message 回傳包含錯誤訊息的 error。
func TestReadStatus_FAIL(t *testing.T) {
	// 格式：FAIL + 4 位 hex 長度 + 錯誤訊息
	r := bytes.NewReader([]byte("FAIL0010device not found"))
	err := ReadStatus(r)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "device not found") {
		t.Errorf("error should contain 'device not found', got %q", err.Error())
	}
}

// TestReadStatus_Unknown 驗證讀取非 "OKAY"/"FAIL" 的 4 bytes 回傳錯誤。
func TestReadStatus_Unknown(t *testing.T) {
	r := bytes.NewReader([]byte("WHAT"))
	err := ReadStatus(r)
	if err == nil {
		t.Fatal("expected error for unknown status, got nil")
	}
	if !strings.Contains(err.Error(), "WHAT") {
		t.Errorf("error should contain the unknown status 'WHAT', got %q", err.Error())
	}
}

// TestReadStatus_ShortRead 驗證不足 4 bytes 時回傳 error。
func TestReadStatus_ShortRead(t *testing.T) {
	r := bytes.NewReader([]byte("OK"))
	err := ReadStatus(r)
	if err == nil {
		t.Fatal("expected error for short read, got nil")
	}
}

// TestReadStatus_FAIL_NoMessage 驗證 FAIL 後無法讀取錯誤長度時回傳適當的錯誤。
func TestReadStatus_FAIL_NoMessage(t *testing.T) {
	// 只有 "FAIL" 沒有後續訊息長度
	r := bytes.NewReader([]byte("FAIL"))
	err := ReadStatus(r)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "FAIL") {
		t.Errorf("error should contain 'FAIL', got %q", err.Error())
	}
}
