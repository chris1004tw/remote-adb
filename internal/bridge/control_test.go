package bridge

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

func TestControlReadLoop_MultipleMessages(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()

	msgs := []CtrlMessage{
		{Type: "hello", Hostname: "test-host"},
		{Type: "devices", Devices: []DeviceInfo{{Serial: "SN1", State: "device"}}},
		{Type: "devices", Devices: []DeviceInfo{{Serial: "SN1", State: "device"}, {Serial: "SN2", State: "device"}}},
	}

	// 背景寫入訊息後關閉 writer
	go func() {
		enc := json.NewEncoder(w)
		for _, m := range msgs {
			enc.Encode(m)
		}
		w.Close()
	}()

	var mu sync.Mutex
	var received []CtrlMessage
	err := ControlReadLoop(context.Background(), nopWriteCloser{r}, func(msg CtrlMessage) {
		mu.Lock()
		received = append(received, msg)
		mu.Unlock()
	})

	// EOF 後應回傳 error（非 ctx cancel）
	if err == nil {
		t.Fatal("expected error on EOF, got nil")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != len(msgs) {
		t.Fatalf("received %d messages, want %d", len(received), len(msgs))
	}
	if received[0].Type != "hello" || received[0].Hostname != "test-host" {
		t.Errorf("first message: got %+v", received[0])
	}
	if len(received[2].Devices) != 2 {
		t.Errorf("third message devices: got %d, want 2", len(received[2].Devices))
	}
}

func TestControlReadLoop_MalformedJSON(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()

	go func() {
		w.Write([]byte("not valid json\n"))
		w.Close()
	}()

	err := ControlReadLoop(context.Background(), nopWriteCloser{r}, func(msg CtrlMessage) {
		t.Error("onMessage should not be called for malformed JSON")
	})

	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestControlReadLoop_CtxCancel(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())

	// 寫一則訊息後取消 context
	go func() {
		enc := json.NewEncoder(w)
		enc.Encode(CtrlMessage{Type: "hello", Hostname: "h"})
		time.Sleep(50 * time.Millisecond)
		cancel()
		// 關閉 writer 讓 Decode 解除阻塞
		w.Close()
	}()

	var called int
	err := ControlReadLoop(ctx, nopWriteCloser{r}, func(msg CtrlMessage) {
		called++
	})

	// ctx 取消後應回傳 nil
	if err != nil {
		t.Errorf("expected nil on ctx cancel, got %v", err)
	}
	if called != 1 {
		t.Errorf("onMessage called %d times, want 1", called)
	}
}

func TestControlReadLoop_EOF(t *testing.T) {
	r, w := io.Pipe()

	// 立即關閉 writer（EOF）
	w.Close()

	err := ControlReadLoop(context.Background(), nopWriteCloser{r}, func(msg CtrlMessage) {
		t.Error("onMessage should not be called on immediate EOF")
	})

	if err == nil {
		t.Fatal("expected error on EOF, got nil")
	}
}

func TestControlReadLoop_PingMessage(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()

	go func() {
		enc := json.NewEncoder(w)
		enc.Encode(CtrlMessage{Type: "ping"})
		enc.Encode(CtrlMessage{Type: "devices"})
		w.Close()
	}()

	var types []string
	ControlReadLoop(context.Background(), nopWriteCloser{r}, func(msg CtrlMessage) {
		types = append(types, msg.Type)
	})

	if len(types) != 2 {
		t.Fatalf("got %d messages, want 2", len(types))
	}
	if types[0] != "ping" {
		t.Errorf("first type: got %q, want %q", types[0], "ping")
	}
}

// nopWriteCloser 包裝 io.Reader 為 io.ReadWriteCloser，
// Write 和 Close 為 no-op（ControlReadLoop 只讀取不寫入）。
type nopWriteCloser struct {
	io.Reader
}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
