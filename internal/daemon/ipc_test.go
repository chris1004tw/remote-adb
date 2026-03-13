package daemon_test

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/chris1004tw/remote-adb/internal/daemon"
)

// TestSendCommand_OK 驗證 SendCommand 正確編碼命令並解碼回應。
func TestSendCommand_OK(t *testing.T) {
	// 建立 TCP pipe
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// mock server：讀取命令，回傳成功回應
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var cmd daemon.IPCCommand
		json.NewDecoder(conn).Decode(&cmd)
		resp := daemon.IPCResponse{Success: true, Error: "", Data: nil}
		resp = daemon.SuccessResponse("ok")
		json.NewEncoder(conn).Encode(resp)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	resp, err := daemon.SendCommand(conn, daemon.IPCCommand{Action: "test"})
	if err != nil {
		t.Fatalf("SendCommand 應成功: %v", err)
	}
	if !resp.Success {
		t.Error("回應應為 Success")
	}
}

// TestSendCommand_ServerCloseEarly 驗證 server 端提前關閉連線時回傳錯誤。
func TestSendCommand_ServerCloseEarly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// mock server：接受連線後立即關閉（不回應）
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, err = daemon.SendCommand(conn, daemon.IPCCommand{Action: "test"})
	if err == nil {
		t.Fatal("server 提前關閉時應回傳錯誤")
	}
}
