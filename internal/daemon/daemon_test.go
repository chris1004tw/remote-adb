package daemon_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/chris1004tw/remote-adb/internal/daemon"
)

// sendIPCCommand 連線到 IPC 服務，發送指令，回傳回應。
func sendIPCCommand(t *testing.T, addr string, cmd daemon.IPCCommand) daemon.IPCResponse {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("連線 IPC 失敗: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := json.NewEncoder(conn).Encode(cmd); err != nil {
		t.Fatalf("發送指令失敗: %v", err)
	}

	var resp daemon.IPCResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("讀取回應失敗: %v", err)
	}
	return resp
}

// startTestDaemon 啟動一個測試用的 Daemon（不連線 Server）。
func startTestDaemon(t *testing.T) (*daemon.Daemon, string) {
	t.Helper()

	d := daemon.NewDaemon(daemon.Config{})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		ln.Close()
	})

	go d.ServeIPC(ctx, ln)

	// 等待 server 啟動
	time.Sleep(10 * time.Millisecond)

	return d, ln.Addr().String()
}

func TestIPC_ListEmpty(t *testing.T) {
	_, addr := startTestDaemon(t)

	resp := sendIPCCommand(t, addr, daemon.IPCCommand{Action: "list"})
	if !resp.Success {
		t.Fatalf("list 指令應成功: %s", resp.Error)
	}

	var bindings []daemon.Binding
	if err := json.Unmarshal(resp.Data, &bindings); err != nil {
		t.Fatalf("解析回應失敗: %v", err)
	}
	if len(bindings) != 0 {
		t.Errorf("預期空列表，實際 %d 筆", len(bindings))
	}
}

func TestIPC_ListWithBindings(t *testing.T) {
	d, addr := startTestDaemon(t)

	d.Bindings().Add(daemon.Binding{
		LocalPort: 15555, HostID: "host1", Serial: "device1", Status: "active",
	})

	resp := sendIPCCommand(t, addr, daemon.IPCCommand{Action: "list"})
	if !resp.Success {
		t.Fatalf("list 指令應成功: %s", resp.Error)
	}

	var bindings []daemon.Binding
	json.Unmarshal(resp.Data, &bindings)
	if len(bindings) != 1 {
		t.Errorf("預期 1 筆，實際 %d 筆", len(bindings))
	}
	if len(bindings) > 0 && bindings[0].Serial != "device1" {
		t.Errorf("Serial = %s, 預期 device1", bindings[0].Serial)
	}
}

func TestIPC_Status(t *testing.T) {
	_, addr := startTestDaemon(t)

	resp := sendIPCCommand(t, addr, daemon.IPCCommand{Action: "status"})
	if !resp.Success {
		t.Fatalf("status 指令應成功: %s", resp.Error)
	}

	var status daemon.StatusInfo
	json.Unmarshal(resp.Data, &status)
	if status.Connected {
		t.Error("未連線 Server 時 Connected 應為 false")
	}
	if status.BindCount != 0 {
		t.Errorf("BindCount = %d, 預期 0", status.BindCount)
	}
}

func TestIPC_HostsEmpty(t *testing.T) {
	_, addr := startTestDaemon(t)

	resp := sendIPCCommand(t, addr, daemon.IPCCommand{Action: "hosts"})
	if !resp.Success {
		t.Fatalf("hosts 指令應成功: %s", resp.Error)
	}
}

func TestIPC_UnbindNotFound(t *testing.T) {
	_, addr := startTestDaemon(t)

	payload, _ := json.Marshal(daemon.UnbindRequest{LocalPort: 99999})
	resp := sendIPCCommand(t, addr, daemon.IPCCommand{
		Action:  "unbind",
		Payload: payload,
	})
	if resp.Success {
		t.Error("unbind 不存在的 port 應失敗")
	}
	if resp.Error == "" {
		t.Error("應包含錯誤訊息")
	}
}

func TestIPC_UnbindSuccess(t *testing.T) {
	d, addr := startTestDaemon(t)

	d.Bindings().Add(daemon.Binding{
		LocalPort: 15555, HostID: "host1", Serial: "device1", Status: "active",
	})

	payload, _ := json.Marshal(daemon.UnbindRequest{LocalPort: 15555})
	resp := sendIPCCommand(t, addr, daemon.IPCCommand{
		Action:  "unbind",
		Payload: payload,
	})
	if !resp.Success {
		t.Fatalf("unbind 應成功: %s", resp.Error)
	}

	// 確認已移除
	listResp := sendIPCCommand(t, addr, daemon.IPCCommand{Action: "list"})
	var bindings []daemon.Binding
	json.Unmarshal(listResp.Data, &bindings)
	if len(bindings) != 0 {
		t.Errorf("unbind 後應無綁定，實際 %d 筆", len(bindings))
	}
}

func TestIPC_BindWithoutServer(t *testing.T) {
	_, addr := startTestDaemon(t)

	payload, _ := json.Marshal(daemon.BindRequest{HostID: "host1", Serial: "device1"})
	resp := sendIPCCommand(t, addr, daemon.IPCCommand{
		Action:  "bind",
		Payload: payload,
	})
	if resp.Success {
		t.Error("未連線 Server 時 bind 應失敗")
	}
}

func TestIPC_UnknownCommand(t *testing.T) {
	_, addr := startTestDaemon(t)

	resp := sendIPCCommand(t, addr, daemon.IPCCommand{Action: "unknown"})
	if resp.Success {
		t.Error("未知指令應失敗")
	}
}
