package bridge

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// trackingRWC 追蹤 Close 是否被呼叫，用於驗證 HandleChannel 的行為。
type trackingRWC struct {
	io.Reader
	closed bool
	mu     sync.Mutex
}

func (t *trackingRWC) Write(p []byte) (int, error) { return len(p), nil }
func (t *trackingRWC) Read(p []byte) (int, error)  { return 0, io.EOF }
func (t *trackingRWC) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}
func (t *trackingRWC) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func TestHandleChannel_ADBServer(t *testing.T) {
	h := &ServerHandler{ADBAddr: "127.0.0.1:15037"}
	rwc := &trackingRWC{}

	// HandleADBServerConn 會嘗試連線 ADB server（會失敗），
	// 但 HandleChannel 應回傳 true 表示已分派。
	result := h.HandleChannel(context.Background(), "adb-server/1", rwc)
	if !result {
		t.Error("HandleChannel should return true for adb-server label")
	}

	// 等待 goroutine 執行完畢（連線失敗後會 close rwc）
	time.Sleep(100 * time.Millisecond)
	if !rwc.isClosed() {
		t.Error("rwc should be closed after HandleADBServerConn completes")
	}
}

func TestHandleChannel_ADBStream(t *testing.T) {
	h := &ServerHandler{ADBAddr: "127.0.0.1:15037"}
	rwc := &trackingRWC{}

	result := h.HandleChannel(context.Background(), "adb-stream/1/SN123/shell:ls", rwc)
	if !result {
		t.Error("HandleChannel should return true for adb-stream label")
	}

	// 等待 goroutine 執行完畢（連線失敗後寫入 0x00 並 close）
	time.Sleep(100 * time.Millisecond)
	if !rwc.isClosed() {
		t.Error("rwc should be closed after HandleADBStreamConn completes")
	}
}

func TestHandleChannel_ADBStreamWritesFailureByte(t *testing.T) {
	h := &ServerHandler{ADBAddr: "127.0.0.1:15037"}

	// 使用可記錄寫入的 buffer
	var buf bytes.Buffer
	rwc := &bufferRWC{Buffer: &buf}

	h.HandleADBStreamConn(context.Background(), rwc, "SN123", "shell:ls")

	// ADB server 連線失敗時應寫入 0x00
	if buf.Len() == 0 {
		t.Fatal("expected failure byte to be written")
	}
	if buf.Bytes()[0] != 0x00 {
		t.Errorf("expected 0x00 failure byte, got 0x%02x", buf.Bytes()[0])
	}
}

func TestHandleChannel_ADBStreamInvalidParts(t *testing.T) {
	h := &ServerHandler{ADBAddr: "127.0.0.1:15037"}
	rwc := &trackingRWC{}

	// parts < 4（只有 "adb-stream/1/SN123"，缺少 service）
	result := h.HandleChannel(context.Background(), "adb-stream/1/SN123", rwc)
	if !result {
		t.Error("HandleChannel should return true even for invalid adb-stream (handled with close)")
	}

	// 應直接關閉 rwc
	time.Sleep(50 * time.Millisecond)
	if !rwc.isClosed() {
		t.Error("rwc should be closed for invalid adb-stream parts")
	}
}

func TestHandleChannel_ADBForward(t *testing.T) {
	h := &ServerHandler{ADBAddr: "127.0.0.1:15037"}
	rwc := &trackingRWC{}

	result := h.HandleChannel(context.Background(), "adb-fwd/1/SN123/localabstract:scrcpy", rwc)
	if !result {
		t.Error("HandleChannel should return true for adb-fwd label")
	}

	// 等待 goroutine（ADB server 連線失敗後會 close rwc）
	time.Sleep(100 * time.Millisecond)
	if !rwc.isClosed() {
		t.Error("rwc should be closed after HandleADBForwardConn completes")
	}
}

func TestHandleChannel_ADBForwardInvalidParts(t *testing.T) {
	h := &ServerHandler{ADBAddr: "127.0.0.1:15037"}
	rwc := &trackingRWC{}

	// parts < 4（只有 "adb-fwd/1/SN123"，缺少 remoteSpec）
	result := h.HandleChannel(context.Background(), "adb-fwd/1/SN123", rwc)
	if !result {
		t.Error("HandleChannel should return true even for invalid adb-fwd (handled with close)")
	}

	time.Sleep(50 * time.Millisecond)
	if !rwc.isClosed() {
		t.Error("rwc should be closed for invalid adb-fwd parts")
	}
}

func TestHandleChannel_UnknownLabel(t *testing.T) {
	h := &ServerHandler{ADBAddr: "127.0.0.1:15037"}
	rwc := &trackingRWC{}

	result := h.HandleChannel(context.Background(), "unknown/1", rwc)
	if result {
		t.Error("HandleChannel should return false for unknown label")
	}
}

func TestHandleChannel_TooFewParts(t *testing.T) {
	h := &ServerHandler{ADBAddr: "127.0.0.1:15037"}
	rwc := &trackingRWC{}

	result := h.HandleChannel(context.Background(), "noslash", rwc)
	if result {
		t.Error("HandleChannel should return false when label has no slash")
	}
}

// bufferRWC 是用於測試的 ReadWriteCloser，Write 寫入 buffer、Read 回傳 EOF。
type bufferRWC struct {
	*bytes.Buffer
}

func (b *bufferRWC) Read(p []byte) (int, error)  { return 0, io.EOF }
func (b *bufferRWC) Close() error                { return nil }
