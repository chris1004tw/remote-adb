package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/chris1004tw/remote-adb/internal/proxy"
)

// --- Proxy 整合測試 ---

// testChannel 模擬 DataChannel / 遠端連線，實作 io.ReadWriteCloser。
// 內部用 pipe 實現雙向通訊。
type testChannel struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func newTestChannelPair() (*testChannel, *testChannel) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	return &testChannel{reader: r1, writer: w2},
		&testChannel{reader: r2, writer: w1}
}

func (c *testChannel) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *testChannel) Write(p []byte) (int, error) { return c.writer.Write(p) }
func (c *testChannel) Close() error {
	c.reader.Close()
	c.writer.Close()
	return nil
}

// TestProxy_SingleConnection 驗證單一連線的雙向資料傳輸。
func TestProxy_SingleConnection(t *testing.T) {
	local, remote := newTestChannelPair()
	defer local.Close()
	defer remote.Close()

	p, err := proxy.New(0, local)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	// 連線到 proxy
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 等 proxy 接受連線並設定好 goroutine
	time.Sleep(50 * time.Millisecond)

	// conn → channel 方向
	sent := []byte("hello from client")
	conn.Write(sent)

	buf := make([]byte, 256)
	n, err := remote.Read(buf)
	if err != nil {
		t.Fatalf("remote 讀取失敗: %v", err)
	}
	if !bytes.Equal(buf[:n], sent) {
		t.Errorf("remote 收到 %q, 預期 %q", buf[:n], sent)
	}

	// channel → conn 方向
	reply := []byte("hello from remote")
	remote.Write(reply)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("conn 讀取失敗: %v", err)
	}
	if !bytes.Equal(buf[:n], reply) {
		t.Errorf("conn 收到 %q, 預期 %q", buf[:n], reply)
	}
}

// TestProxy_ConnectionReplacement 驗證新連線到達時，舊連線被正確替換。
func TestProxy_ConnectionReplacement(t *testing.T) {
	local, remote := newTestChannelPair()
	defer local.Close()
	defer remote.Close()

	p, err := proxy.New(0, local)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	// 第一條連線
	conn1, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()
	time.Sleep(50 * time.Millisecond)

	// conn1 → channel
	conn1.Write([]byte("from conn1"))
	buf := make([]byte, 256)
	n, err := remote.Read(buf)
	if err != nil {
		t.Fatalf("remote 讀取 conn1 失敗: %v", err)
	}
	if string(buf[:n]) != "from conn1" {
		t.Errorf("remote 收到 %q, 預期 %q", buf[:n], "from conn1")
	}

	// 第二條連線（應替換第一條）
	conn2, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	time.Sleep(100 * time.Millisecond) // 等待替換完成

	// conn2 → channel
	conn2.Write([]byte("from conn2"))
	n, err = remote.Read(buf)
	if err != nil {
		t.Fatalf("remote 讀取 conn2 失敗: %v", err)
	}
	if string(buf[:n]) != "from conn2" {
		t.Errorf("remote 收到 %q, 預期 %q", buf[:n], "from conn2")
	}

	// channel → conn2（新連線應收到回應）
	remote.Write([]byte("reply to conn2"))
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = conn2.Read(buf)
	if err != nil {
		t.Fatalf("conn2 讀取失敗: %v", err)
	}
	if string(buf[:n]) != "reply to conn2" {
		t.Errorf("conn2 收到 %q, 預期 %q", buf[:n], "reply to conn2")
	}

	// conn1 應已被關閉（讀取應失敗）
	conn1.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = conn1.Read(buf)
	if err == nil {
		t.Error("conn1 應已關閉，但讀取成功")
	}
}

// TestProxy_NoDataCorruption 驗證快速連續連線不會造成 channel 資料交錯。
func TestProxy_NoDataCorruption(t *testing.T) {
	local, remote := newTestChannelPair()
	defer local.Close()
	defer remote.Close()

	p, err := proxy.New(0, local)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	// 快速建立 5 條連線
	var lastConn net.Conn
	for i := 0; i < 5; i++ {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
		if err != nil {
			t.Fatalf("連線 %d 失敗: %v", i, err)
		}
		defer conn.Close()
		lastConn = conn
		time.Sleep(20 * time.Millisecond)
	}

	// 等替換穩定
	time.Sleep(100 * time.Millisecond)

	// 最後一條連線應正常工作
	msg := []byte("final connection test")
	lastConn.Write(msg)

	buf := make([]byte, 256)
	n, err := remote.Read(buf)
	if err != nil {
		t.Fatalf("remote 讀取失敗: %v", err)
	}
	if !bytes.Equal(buf[:n], msg) {
		t.Errorf("remote 收到 %q, 預期 %q", buf[:n], msg)
	}
}

// TestProxy_StopCleansUp 驗證 Stop 正確關閉所有資源。
func TestProxy_StopCleansUp(t *testing.T) {
	local, remote := newTestChannelPair()
	defer remote.Close()

	p, err := proxy.New(0, local)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	p.Start(ctx)

	// 建立一條連線
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)

	// Stop 應在合理時間內完成
	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()

	select {
	case <-done:
		// 成功
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 超時（>3s）")
	}

	// 連線應被關閉
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = conn.Read(make([]byte, 1))
	if err == nil {
		t.Error("Stop 後連線應已關閉")
	}
}

// TestProxy_NewWithListener 驗證使用已建立的 listener 建構 Proxy，
// 可正常接受連線並進行雙向轉發。
func TestProxy_NewWithListener(t *testing.T) {
	local, remote := newTestChannelPair()
	defer local.Close()
	defer remote.Close()

	// 預先建立 listener（模擬 PortAllocator.AllocateListener 的回傳）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("建立 listener 失敗: %v", err)
	}
	expectedPort := ln.Addr().(*net.TCPAddr).Port

	p := proxy.NewWithListener(ln, local)
	if p.Port() != expectedPort {
		t.Errorf("Port() = %d, 預期 %d", p.Port(), expectedPort)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	// 驗證可正常連線與雙向傳輸
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatalf("Dial 失敗: %v", err)
	}
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// conn → channel
	sent := []byte("via NewWithListener")
	conn.Write(sent)

	buf := make([]byte, 256)
	n, err := remote.Read(buf)
	if err != nil {
		t.Fatalf("remote 讀取失敗: %v", err)
	}
	if !bytes.Equal(buf[:n], sent) {
		t.Errorf("remote 收到 %q, 預期 %q", buf[:n], sent)
	}

	// channel → conn
	reply := []byte("reply via NewWithListener")
	remote.Write(reply)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("conn 讀取失敗: %v", err)
	}
	if !bytes.Equal(buf[:n], reply) {
		t.Errorf("conn 收到 %q, 預期 %q", buf[:n], reply)
	}
}

// TestProxy_ConcurrentConnections 驗證多條並行連線不會 panic 或 deadlock。
func TestProxy_ConcurrentConnections(t *testing.T) {
	local, remote := newTestChannelPair()
	defer local.Close()

	p, err := proxy.New(0, local)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	// 持續從 remote 消耗資料（避免 pipe 阻塞）
	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := remote.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	// 同時開啟 10 條連線
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
			if err != nil {
				return
			}
			defer conn.Close()
			conn.Write([]byte(fmt.Sprintf("conn-%d", idx)))
			time.Sleep(50 * time.Millisecond)
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("並行連線測試超時")
	}
}
