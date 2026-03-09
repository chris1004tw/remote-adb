package directsrv

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/chris1004tw/remote-adb/internal/adb"
)

// dialEcho 回傳一個 mock DialDevice 函式，模擬 ADB 連線（echo 回傳所有寫入的資料）。
func dialEcho() func(string, int) (net.Conn, error) {
	return func(serial string, port int) (net.Conn, error) {
		server, client := net.Pipe()
		go func() {
			defer server.Close()
			io.Copy(server, server)
		}()
		return client, nil
	}
}

// dialSink 回傳一個 mock DialDevice 函式，透過 channel 傳出 ADB 端連線供測試檢查。
func dialSink(t *testing.T) (func(string, int) (net.Conn, error), <-chan net.Conn) {
	ch := make(chan net.Conn, 1)
	fn := func(serial string, port int) (net.Conn, error) {
		server, client := net.Pipe()
		ch <- server
		return client, nil
	}
	return fn, ch
}

func startTestServer(t *testing.T, cfg Config) (addr string, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = ln.Addr().String()

	srv := New(cfg)
	go srv.ServeListener(ctx, ln)

	t.Cleanup(func() {
		cancel()
		ln.Close()
	})

	return addr, cancel
}

func sendRequest(t *testing.T, addr string, req Request) (Response, net.Conn) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal("連線失敗:", err)
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		t.Fatal("發送失敗:", err)
	}

	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		conn.Close()
		t.Fatal("讀取回應失敗:", err)
	}

	return resp, conn
}

func TestList_回傳設備列表(t *testing.T) {
	table := adb.NewDeviceTable()
	table.Update([]adb.DeviceEvent{
		{Serial: "pixel-7", State: "device"},
		{Serial: "galaxy", State: "device"},
	})

	addr, _ := startTestServer(t, Config{
		DeviceTable: table,
		DialDevice:  dialEcho(),
		Hostname:    "test-host",
	})

	resp, conn := sendRequest(t, addr, Request{Action: "list"})
	defer conn.Close()

	if !resp.OK {
		t.Fatalf("預期 ok=true，實際 ok=false: %s", resp.Error)
	}
	if resp.Hostname != "test-host" {
		t.Errorf("預期 hostname=test-host，實際=%s", resp.Hostname)
	}
	if len(resp.Devices) != 2 {
		t.Fatalf("預期 2 台設備，實際=%d", len(resp.Devices))
	}
}

func TestConnect_成功轉發(t *testing.T) {
	table := adb.NewDeviceTable()
	table.Update([]adb.DeviceEvent{
		{Serial: "pixel-7", State: "device"},
	})

	dialFn, adbConnCh := dialSink(t)
	addr, _ := startTestServer(t, Config{
		DeviceTable: table,
		DialDevice:  dialFn,
		Hostname:    "test-host",
	})

	resp, conn := sendRequest(t, addr, Request{Action: "connect", Serial: "pixel-7"})
	if !resp.OK {
		t.Fatalf("預期 ok=true: %s", resp.Error)
	}

	// 取得 ADB 端連線
	var adbConn net.Conn
	select {
	case adbConn = <-adbConnCh:
	case <-time.After(2 * time.Second):
		t.Fatal("逾時：未收到 ADB 連線")
	}
	defer adbConn.Close()

	// 驗證 client → ADB 轉發
	testData := []byte("hello-adb")
	conn.Write(testData)

	buf := make([]byte, 64)
	n, err := adbConn.Read(buf)
	if err != nil {
		t.Fatal("ADB 端讀取失敗:", err)
	}
	if string(buf[:n]) != "hello-adb" {
		t.Errorf("預期 hello-adb，實際=%s", string(buf[:n]))
	}

	// 驗證 ADB → client 轉發
	adbConn.Write([]byte("reply"))
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatal("Client 端讀取失敗:", err)
	}
	if string(buf[:n]) != "reply" {
		t.Errorf("預期 reply，實際=%s", string(buf[:n]))
	}

	conn.Close()
}

func TestConnect_設備不存在(t *testing.T) {
	table := adb.NewDeviceTable()
	// 空設備表

	addr, _ := startTestServer(t, Config{
		DeviceTable: table,
		DialDevice:  dialEcho(),
		Hostname:    "test-host",
	})

	resp, conn := sendRequest(t, addr, Request{Action: "connect", Serial: "not-exist"})
	defer conn.Close()

	if resp.OK {
		t.Fatal("預期 ok=false，設備不存在時應失敗")
	}
}

func TestConnect_設備已鎖定(t *testing.T) {
	table := adb.NewDeviceTable()
	table.Update([]adb.DeviceEvent{
		{Serial: "pixel-7", State: "device"},
	})
	table.Lock("pixel-7", "other-client")

	addr, _ := startTestServer(t, Config{
		DeviceTable: table,
		DialDevice:  dialEcho(),
		Hostname:    "test-host",
	})

	resp, conn := sendRequest(t, addr, Request{Action: "connect", Serial: "pixel-7"})
	defer conn.Close()

	if resp.OK {
		t.Fatal("預期 ok=false，設備已鎖定時應失敗")
	}
}

func TestToken驗證_正確(t *testing.T) {
	table := adb.NewDeviceTable()
	table.Update([]adb.DeviceEvent{
		{Serial: "pixel-7", State: "device"},
	})

	addr, _ := startTestServer(t, Config{
		DeviceTable: table,
		DialDevice:  dialEcho(),
		Hostname:    "test-host",
		Token:       "secret123",
	})

	resp, conn := sendRequest(t, addr, Request{Action: "list", Token: "secret123"})
	defer conn.Close()

	if !resp.OK {
		t.Fatalf("Token 正確時應成功: %s", resp.Error)
	}
}

func TestToken驗證_錯誤(t *testing.T) {
	table := adb.NewDeviceTable()

	addr, _ := startTestServer(t, Config{
		DeviceTable: table,
		DialDevice:  dialEcho(),
		Hostname:    "test-host",
		Token:       "secret123",
	})

	resp, conn := sendRequest(t, addr, Request{Action: "list", Token: "wrong"})
	defer conn.Close()

	if resp.OK {
		t.Fatal("Token 錯誤時應失敗")
	}
}

func TestToken驗證_未提供(t *testing.T) {
	table := adb.NewDeviceTable()

	addr, _ := startTestServer(t, Config{
		DeviceTable: table,
		DialDevice:  dialEcho(),
		Hostname:    "test-host",
		Token:       "secret123",
	})

	resp, conn := sendRequest(t, addr, Request{Action: "list"})
	defer conn.Close()

	if resp.OK {
		t.Fatal("Token 未提供時應失敗")
	}
}

func TestConnect_斷線自動解鎖(t *testing.T) {
	table := adb.NewDeviceTable()
	table.Update([]adb.DeviceEvent{
		{Serial: "pixel-7", State: "device"},
	})

	var wg sync.WaitGroup
	wg.Add(1)

	dialFn := func(serial string, port int) (net.Conn, error) {
		server, client := net.Pipe()
		go func() {
			defer wg.Done()
			defer server.Close()
			io.Copy(io.Discard, server)
		}()
		return client, nil
	}

	addr, _ := startTestServer(t, Config{
		DeviceTable: table,
		DialDevice:  dialFn,
		Hostname:    "test-host",
	})

	resp, conn := sendRequest(t, addr, Request{Action: "connect", Serial: "pixel-7"})
	if !resp.OK {
		t.Fatalf("預期連線成功: %s", resp.Error)
	}

	// 確認已鎖定
	locked, _ := table.IsLocked("pixel-7")
	if !locked {
		t.Fatal("連線後設備應被鎖定")
	}

	// 斷線
	conn.Close()
	wg.Wait()

	// 等一下讓 goroutine 完成解鎖
	time.Sleep(50 * time.Millisecond)

	// 確認已解鎖
	locked, _ = table.IsLocked("pixel-7")
	if locked {
		t.Fatal("斷線後設備應被解鎖")
	}
}
