package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResolveSerial(t *testing.T) {
	t.Run("命中遠端真實 serial", func(t *testing.T) {
		fm := NewForwardManager()
		fm.devices = []DeviceInfo{{Serial: "R58X40L07QP", State: "device"}}

		got, ok := fm.ResolveSerial("R58X40L07QP")
		if !ok || got != "R58X40L07QP" {
			t.Fatalf("got (%q, %v), want (%q, true)", got, ok, "R58X40L07QP")
		}
	})

	t.Run("本機 adb serial 映射到唯一遠端設備", func(t *testing.T) {
		fm := NewForwardManager()
		fm.devices = []DeviceInfo{{Serial: "R58X40L07QP", State: "device"}}

		got, ok := fm.ResolveSerial("127.0.0.1:15037")
		if !ok || got != "R58X40L07QP" {
			t.Fatalf("got (%q, %v), want (%q, true)", got, ok, "R58X40L07QP")
		}
	})

	t.Run("多裝置且 serial 不可解析時失敗", func(t *testing.T) {
		fm := NewForwardManager()
		fm.devices = []DeviceInfo{
			{Serial: "R58X40L07QP", State: "device"},
			{Serial: "ABC123", State: "device"},
		}

		_, ok := fm.ResolveSerial("127.0.0.1:15037")
		if ok {
			t.Fatal("expected resolve failure for ambiguous multi-device")
		}
	})
}

func TestParseForwardCmd(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		wantNil    bool
		wantSerial string
		wantLocal  string
		wantRemote string
	}{
		{
			name:       "host:forward 基本格式",
			cmd:        "host:forward:tcp:27183;localabstract:scrcpy",
			wantLocal:  "tcp:27183",
			wantRemote: "localabstract:scrcpy",
		},
		{
			name:       "host:forward:norebind",
			cmd:        "host:forward:norebind:tcp:27183;localabstract:scrcpy",
			wantLocal:  "tcp:27183",
			wantRemote: "localabstract:scrcpy",
		},
		{
			name:       "host-serial 指定裝置",
			cmd:        "host-serial:R58X40L07QP:forward:tcp:0;localabstract:scrcpy",
			wantSerial: "R58X40L07QP",
			wantLocal:  "tcp:0",
			wantRemote: "localabstract:scrcpy",
		},
		{
			name:       "host-serial + norebind",
			cmd:        "host-serial:ABC123:forward:norebind:tcp:5000;tcp:6000",
			wantSerial: "ABC123",
			wantLocal:  "tcp:5000",
			wantRemote: "tcp:6000",
		},
		{
			name:    "無效格式：缺少分號",
			cmd:     "host:forward:tcp:27183",
			wantNil: true,
		},
		{
			name:    "無效格式：不是 forward 命令",
			cmd:     "host:version",
			wantNil: true,
		},
		{
			name:    "host-serial 但不是 forward",
			cmd:     "host-serial:ABC:shell:ls",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := ParseForwardCmd(tt.cmd)
			if tt.wantNil {
				if fc != nil {
					t.Errorf("期望 nil，但得到 %+v", fc)
				}
				return
			}
			if fc == nil {
				t.Fatal("期望非 nil，但得到 nil")
			}
			if fc.Serial != tt.wantSerial {
				t.Errorf("Serial: got %q, want %q", fc.Serial, tt.wantSerial)
			}
			if fc.LocalSpec != tt.wantLocal {
				t.Errorf("LocalSpec: got %q, want %q", fc.LocalSpec, tt.wantLocal)
			}
			if fc.RemoteSpec != tt.wantRemote {
				t.Errorf("RemoteSpec: got %q, want %q", fc.RemoteSpec, tt.wantRemote)
			}
		})
	}
}

func TestParseKillForwardCmd(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		wantSpec string
		wantOK   bool
	}{
		{
			name:     "host:killforward",
			cmd:      "host:killforward:tcp:27183",
			wantSpec: "tcp:27183",
			wantOK:   true,
		},
		{
			name:     "host-serial:killforward",
			cmd:      "host-serial:ABC:killforward:tcp:5000",
			wantSpec: "tcp:5000",
			wantOK:   true,
		},
		{
			name:   "非 killforward 命令",
			cmd:    "host:forward:tcp:0;tcp:1",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := ParseKillForwardCmd(tt.cmd)
			if ok != tt.wantOK {
				t.Errorf("ok: got %v, want %v", ok, tt.wantOK)
			}
			if ok && spec != tt.wantSpec {
				t.Errorf("spec: got %q, want %q", spec, tt.wantSpec)
			}
		})
	}
}

func TestIsKillForwardAll(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"host:killforward-all", true},
		{"host-serial:ABC:killforward-all", true},
		{"host:killforward:tcp:0", false},
		{"host:version", false},
	}
	for _, tt := range tests {
		if got := IsKillForwardAll(tt.cmd); got != tt.want {
			t.Errorf("IsKillForwardAll(%q): got %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestIsListForward(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"host:list-forward", true},
		{"host-serial:ABC:list-forward", true},
		{"host:killforward-all", false},
	}
	for _, tt := range tests {
		if got := IsListForward(tt.cmd); got != tt.want {
			t.Errorf("IsListForward(%q): got %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

// TestGetDevice_ImmediateReturn 測試 GetDevice 在已有設備時立即回傳。
func TestGetDevice_ImmediateReturn(t *testing.T) {
	fm := NewForwardManager()
	fm.devices = []DeviceInfo{
		{Serial: "R58X40L07QP", State: "device", Features: "shell_v2,cmd"},
	}

	serial, features := fm.GetDevice(context.Background(), time.Second)
	if serial != "R58X40L07QP" {
		t.Errorf("serial: got %q, want %q", serial, "R58X40L07QP")
	}
	if features != "shell_v2,cmd" {
		t.Errorf("features: got %q, want %q", features, "shell_v2,cmd")
	}
}

// TestGetDevice_WaitsForDevice 測試 GetDevice 在無設備時等待 deviceReadyCh，
// 模擬 PeerConnection 尚在 connecting 時 CNXN 到達的場景。
func TestGetDevice_WaitsForDevice(t *testing.T) {
	fm := NewForwardManager()

	done := make(chan struct{})
	var serial, features string
	go func() {
		serial, features = fm.GetDevice(context.Background(), 5*time.Second)
		close(done)
	}()

	// 短暫延遲後模擬 control channel 推送設備清單
	time.Sleep(100 * time.Millisecond)
	fm.UpdateDevices([]DeviceInfo{
		{Serial: "R58X40L07QP", State: "device", Features: "shell_v2"},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GetDevice 應在 deviceReadyCh 信號後回傳，但逾時")
	}

	if serial != "R58X40L07QP" {
		t.Errorf("serial: got %q, want %q", serial, "R58X40L07QP")
	}
	if features != "shell_v2" {
		t.Errorf("features: got %q, want %q", features, "shell_v2")
	}
}

// TestGetDevice_Timeout 測試 GetDevice 在逾時後回傳空值。
func TestGetDevice_Timeout(t *testing.T) {
	fm := NewForwardManager()

	start := time.Now()
	serial, _ := fm.GetDevice(context.Background(), 200*time.Millisecond)
	elapsed := time.Since(start)

	if serial != "" {
		t.Errorf("逾時後 serial 應為空，got %q", serial)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("回傳過快，僅 %v", elapsed)
	}
}

// TestGetDevice_ContextCancel 測試 context 取消時 GetDevice 立即回傳。
func TestGetDevice_ContextCancel(t *testing.T) {
	fm := NewForwardManager()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		fm.GetDevice(ctx, 30*time.Second)
		close(done)
	}()

	// 取消 context
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("context 取消後 GetDevice 應立即回傳")
	}
}

// TestGetDevice_NilReadyCh 測試 deviceReadyCh 為 nil 時直接回傳。
func TestGetDevice_NilReadyCh(t *testing.T) {
	fm := &ForwardManager{} // deviceReadyCh = nil, devices = nil

	serial, _ := fm.GetDevice(context.Background(), time.Second)
	if serial != "" {
		t.Errorf("deviceReadyCh 為 nil 時應直接回傳空值，got %q", serial)
	}
}

func TestParseLocalSpec(t *testing.T) {
	tests := []struct {
		spec    string
		want    int
		wantErr bool
	}{
		{"tcp:27183", 27183, false},
		{"tcp:0", 0, false},
		{"tcp:65535", 65535, false},
		{"localabstract:scrcpy", 0, true},
		{"tcp:abc", 0, true},
	}
	for _, tt := range tests {
		port, err := ParseLocalSpec(tt.spec)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseLocalSpec(%q): err=%v, wantErr=%v", tt.spec, err, tt.wantErr)
		}
		if !tt.wantErr && port != tt.want {
			t.Errorf("ParseLocalSpec(%q): got %d, want %d", tt.spec, port, tt.want)
		}
	}
}

// TestUpdateDevices_Signal 測試 UpdateDevices 正確管理 deviceReadyCh 信號。
func TestUpdateDevices_Signal(t *testing.T) {
	fm := NewForwardManager()

	// 初始狀態：channel 未關閉
	select {
	case <-fm.deviceReadyCh:
		t.Fatal("deviceReadyCh 不應在無設備時被關閉")
	default:
	}

	// 加入在線設備：channel 應被關閉
	fm.UpdateDevices([]DeviceInfo{{Serial: "ABC", State: "device"}})
	select {
	case <-fm.deviceReadyCh:
	default:
		t.Fatal("deviceReadyCh 應在設備出現後被關閉")
	}

	// 再次呼叫不應 panic（已關閉的 channel）
	fm.UpdateDevices([]DeviceInfo{{Serial: "ABC", State: "device"}})

	// 設備消失：channel 應被重建
	fm.UpdateDevices([]DeviceInfo{})
	select {
	case <-fm.deviceReadyCh:
		t.Fatal("deviceReadyCh 不應在設備消失後仍被關閉")
	default:
	}
}

// TestOnlineDevices 測試 OnlineDevices 只回傳在線設備。
func TestOnlineDevices(t *testing.T) {
	fm := NewForwardManager()
	fm.devices = []DeviceInfo{
		{Serial: "A", State: "device"},
		{Serial: "B", State: "offline"},
		{Serial: "C", State: "device"},
	}

	online := fm.OnlineDevices()
	if len(online) != 2 {
		t.Fatalf("expected 2 online devices, got %d", len(online))
	}
	if online[0].Serial != "A" || online[1].Serial != "C" {
		t.Errorf("unexpected devices: %+v", online)
	}
}

// =============================================================================
// 1. ADB 協定輔助函式（純 I/O 格式測試）
// =============================================================================

// TestSendADBCmd 驗證 SendADBCmd 輸出格式為 "%04x" + cmd。
func TestSendADBCmd(t *testing.T) {
	var buf bytes.Buffer
	if err := SendADBCmd(&buf, "host:version"); err != nil {
		t.Fatalf("SendADBCmd error: %v", err)
	}
	got := buf.String()
	want := "000chost:version"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestWriteADBOkay 驗證 WriteADBOkay 輸出 "OKAY"。
func TestWriteADBOkay(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteADBOkay(&buf); err != nil {
		t.Fatalf("WriteADBOkay error: %v", err)
	}
	got := buf.String()
	if got != "OKAY" {
		t.Errorf("got %q, want %q", got, "OKAY")
	}
}

// TestWriteADBFail 驗證 WriteADBFail 輸出 "FAIL" + "%04x" + msg。
func TestWriteADBFail(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteADBFail(&buf, "error"); err != nil {
		t.Fatalf("WriteADBFail error: %v", err)
	}
	got := buf.String()
	want := "FAIL0005error"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestReadADBStatus_Okay 驗證讀取 "OKAY" 回傳 nil。
func TestReadADBStatus_Okay(t *testing.T) {
	r := bytes.NewReader([]byte("OKAY"))
	if err := ReadADBStatus(r); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

// TestReadADBStatus_Fail 驗證讀取 "FAIL" 回傳含 "FAIL" 的 error。
func TestReadADBStatus_Fail(t *testing.T) {
	r := bytes.NewReader([]byte("FAIL"))
	err := ReadADBStatus(r)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "FAIL") {
		t.Errorf("error should contain 'FAIL', got %q", err.Error())
	}
}

// TestReadADBStatus_ShortRead 驗證不足 4 bytes 回傳 error。
func TestReadADBStatus_ShortRead(t *testing.T) {
	r := bytes.NewReader([]byte("OK"))
	err := ReadADBStatus(r)
	if err == nil {
		t.Fatal("expected error for short read, got nil")
	}
}

// =============================================================================
// 2. QueryDeviceFeatures（mock ADB server）
// =============================================================================

// mockADBServer 啟動一個 mock ADB server，根據 handler 處理每個連線。
// 回傳 listener 地址（"127.0.0.1:PORT"）和關閉函式。
func mockADBServer(t *testing.T, handler func(conn net.Conn)) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create mock ADB server: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestQueryDeviceFeatures_Success 測試正常查詢設備 features。
func TestQueryDeviceFeatures_Success(t *testing.T) {
	features := "shell_v2,cmd,stat_v2"
	addr, cleanup := mockADBServer(t, func(conn net.Conn) {
		defer conn.Close()
		// 讀取 4 bytes hex length
		var lenBuf [4]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			t.Errorf("mock: read len failed: %v", err)
			return
		}
		n, _ := strconv.ParseInt(string(lenBuf[:]), 16, 32)
		// 讀取命令
		cmdBuf := make([]byte, n)
		if _, err := io.ReadFull(conn, cmdBuf); err != nil {
			t.Errorf("mock: read cmd failed: %v", err)
			return
		}
		wantCmd := "host-serial:SN123:features"
		if string(cmdBuf) != wantCmd {
			t.Errorf("mock: got cmd %q, want %q", string(cmdBuf), wantCmd)
			return
		}
		// 回應 OKAY + hex-length + features
		conn.Write([]byte("OKAY"))
		resp := fmt.Sprintf("%04x%s", len(features), features)
		conn.Write([]byte(resp))
	})
	defer cleanup()

	got, err := QueryDeviceFeatures(addr, "SN123")
	if err != nil {
		t.Fatalf("QueryDeviceFeatures error: %v", err)
	}
	if got != features {
		t.Errorf("got %q, want %q", got, features)
	}
}

// TestQueryDeviceFeatures_ServerFail 測試 mock server 回應 FAIL 時回傳 error。
func TestQueryDeviceFeatures_ServerFail(t *testing.T) {
	addr, cleanup := mockADBServer(t, func(conn net.Conn) {
		defer conn.Close()
		// 讀取命令（消耗掉）
		var lenBuf [4]byte
		io.ReadFull(conn, lenBuf[:])
		n, _ := strconv.ParseInt(string(lenBuf[:]), 16, 32)
		cmdBuf := make([]byte, n)
		io.ReadFull(conn, cmdBuf)
		// 回應 FAIL
		conn.Write([]byte("FAIL0005error"))
	})
	defer cleanup()

	_, err := QueryDeviceFeatures(addr, "SN123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestQueryDeviceFeatures_ConnectTimeout 測試連線到不可達位址時回傳 error。
func TestQueryDeviceFeatures_ConnectTimeout(t *testing.T) {
	// 使用 RFC 5737 的 TEST-NET-1 位址，保證不可達
	_, err := QueryDeviceFeatures("192.0.2.1:1", "SN123")
	if err == nil {
		t.Fatal("expected error for unreachable address, got nil")
	}
}

// =============================================================================
// 3. Forward 操作測試
// =============================================================================

// newTestForwardManager 建立帶有單一設備的 ForwardManager，用於 forward 測試。
func newTestForwardManager() *ForwardManager {
	fm := NewForwardManager()
	fm.devices = []DeviceInfo{{Serial: "TEST001", State: "device"}}
	// 關閉 deviceReadyCh 模擬設備已就緒
	close(fm.deviceReadyCh)
	return fm
}

// nopOpenCh 回傳不阻塞的 nopRWC，用於不需要實際 DataChannel 的測試。
func nopOpenCh(label string) (io.ReadWriteCloser, error) {
	return nopRWC{}, nil
}

// TestHandleForward_Success 驗證正常 forward 回應包含兩個 OKAY，且 fwdListeners 有 entry。
func TestHandleForward_Success(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	client, server := net.Pipe()
	defer client.Close()

	fc := &FwdCmd{LocalSpec: "tcp:0", RemoteSpec: "localabstract:scrcpy"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		fm.HandleForward(context.Background(), server, fc, nopOpenCh)
		server.Close()
	}()

	resp, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read response error: %v", err)
	}
	<-done

	// tcp:0 回應格式：OKAY + OKAY + hex-len + port
	respStr := string(resp)
	if !strings.HasPrefix(respStr, "OKAYOKAY") {
		t.Errorf("response should start with 'OKAYOKAY', got %q", respStr)
	}

	// 驗證 fwdListeners 有 entry
	fm.fwdMu.Lock()
	count := len(fm.fwdListeners)
	fm.fwdMu.Unlock()
	if count != 1 {
		t.Errorf("expected 1 fwdListener, got %d", count)
	}
}

// TestHandleForward_TcpZero 驗證 tcp:0 回傳包含實際 port 號。
func TestHandleForward_TcpZero(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	client, server := net.Pipe()
	defer client.Close()

	fc := &FwdCmd{LocalSpec: "tcp:0", RemoteSpec: "tcp:5555"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		fm.HandleForward(context.Background(), server, fc, nopOpenCh)
		server.Close()
	}()

	resp, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read response error: %v", err)
	}
	<-done

	// 格式：OKAY(4) + OKAY(4) + hex-len(4) + port-str
	respStr := string(resp)
	if len(respStr) < 12 {
		t.Fatalf("response too short: %q", respStr)
	}
	prefix := respStr[:8]
	if prefix != "OKAYOKAY" {
		t.Errorf("expected 'OKAYOKAY' prefix, got %q", prefix)
	}
	hexLen := respStr[8:12]
	n, err := strconv.ParseInt(hexLen, 16, 32)
	if err != nil {
		t.Fatalf("failed to parse hex length %q: %v", hexLen, err)
	}
	portStr := respStr[12:]
	if int64(len(portStr)) != n {
		t.Errorf("port string length mismatch: got %d, hex said %d", len(portStr), n)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("failed to parse port %q: %v", portStr, err)
	}
	if port <= 0 || port > 65535 {
		t.Errorf("port out of range: %d", port)
	}
}

// TestHandleForward_BindFail 驗證 port 已被佔用時回傳 OKAY + FAIL。
func TestHandleForward_BindFail(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	// 先佔用一個 port
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create blocker listener: %v", err)
	}
	defer blocker.Close()
	blockerPort := blocker.Addr().(*net.TCPAddr).Port

	client, server := net.Pipe()
	defer client.Close()

	fc := &FwdCmd{LocalSpec: fmt.Sprintf("tcp:%d", blockerPort), RemoteSpec: "tcp:5555"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		fm.HandleForward(context.Background(), server, fc, nopOpenCh)
		server.Close()
	}()

	resp, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read response error: %v", err)
	}
	<-done

	respStr := string(resp)
	// 預期：OKAY + FAIL + hex-len + error msg
	if !strings.HasPrefix(respStr, "OKAYFAIL") {
		t.Errorf("expected 'OKAYFAIL' prefix, got %q", respStr)
	}
}

// TestHandleKillForward_Success 驗證建立 forward 後 kill 成功回傳兩個 OKAY。
func TestHandleKillForward_Success(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	// 先建立一個 forward
	c1, s1 := net.Pipe()
	defer c1.Close()
	fc := &FwdCmd{LocalSpec: "tcp:0", RemoteSpec: "tcp:9999"}
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		fm.HandleForward(context.Background(), s1, fc, nopOpenCh)
		s1.Close()
	}()
	resp1, _ := io.ReadAll(c1)
	<-done1

	// 從回應中解析實際 port 以建構 kill 的 localSpec
	r1 := string(resp1)
	// OKAYOKAY + hex-len(4) + port
	hexLen := r1[8:12]
	n, _ := strconv.ParseInt(hexLen, 16, 32)
	portStr := r1[12 : 12+n]
	localSpec := "tcp:" + portStr

	// 執行 kill
	c2, s2 := net.Pipe()
	defer c2.Close()
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		fm.HandleKillForward(s2, localSpec)
		s2.Close()
	}()
	resp2, _ := io.ReadAll(c2)
	<-done2

	if string(resp2) != "OKAYOKAY" {
		t.Errorf("expected 'OKAYOKAY', got %q", string(resp2))
	}

	// 驗證 fwdListeners 已移除
	fm.fwdMu.Lock()
	count := len(fm.fwdListeners)
	fm.fwdMu.Unlock()
	if count != 0 {
		t.Errorf("expected 0 fwdListeners after kill, got %d", count)
	}
}

// TestHandleKillForward_NotFound 驗證 kill 不存在的 forward 回傳 OKAY + FAIL。
func TestHandleKillForward_NotFound(t *testing.T) {
	fm := NewForwardManager()

	client, server := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fm.HandleKillForward(server, "tcp:9999")
		server.Close()
	}()
	resp, _ := io.ReadAll(client)
	<-done

	respStr := string(resp)
	if !strings.HasPrefix(respStr, "OKAYFAIL") {
		t.Errorf("expected 'OKAYFAIL' prefix, got %q", respStr)
	}
}

// TestHandleKillForwardAll 驗證建立多個 forward 後 kill all 回傳兩個 OKAY 且清空清單。
func TestHandleKillForwardAll(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	// 建立兩個 forward
	for i := 0; i < 2; i++ {
		c, s := net.Pipe()
		fc := &FwdCmd{LocalSpec: "tcp:0", RemoteSpec: fmt.Sprintf("tcp:%d", 9000+i)}
		done := make(chan struct{})
		go func() {
			defer close(done)
			fm.HandleForward(context.Background(), s, fc, nopOpenCh)
			s.Close()
		}()
		io.ReadAll(c)
		c.Close()
		<-done
	}

	// 確認有 2 個 listener
	fm.fwdMu.Lock()
	before := len(fm.fwdListeners)
	fm.fwdMu.Unlock()
	if before != 2 {
		t.Fatalf("expected 2 fwdListeners before kill-all, got %d", before)
	}

	// kill all
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fm.HandleKillForwardAll(server)
		server.Close()
	}()
	resp, _ := io.ReadAll(client)
	<-done

	if string(resp) != "OKAYOKAY" {
		t.Errorf("expected 'OKAYOKAY', got %q", string(resp))
	}

	fm.fwdMu.Lock()
	after := len(fm.fwdListeners)
	fm.fwdMu.Unlock()
	if after != 0 {
		t.Errorf("expected 0 fwdListeners after kill-all, got %d", after)
	}
}

// TestHandleListForward_Empty 驗證無 listener 時回傳 OKAY + "0000"。
func TestHandleListForward_Empty(t *testing.T) {
	fm := NewForwardManager()

	client, server := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fm.HandleListForward(server)
		server.Close()
	}()
	resp, _ := io.ReadAll(client)
	<-done

	if string(resp) != "OKAY0000" {
		t.Errorf("expected 'OKAY0000', got %q", string(resp))
	}
}

// TestHandleListForward_WithEntries 驗證有 listener 時回傳格式 "serial localSpec remoteSpec\n"。
func TestHandleListForward_WithEntries(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	// 建立一個 forward
	c, s := net.Pipe()
	fc := &FwdCmd{LocalSpec: "tcp:0", RemoteSpec: "localabstract:scrcpy"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fm.HandleForward(context.Background(), s, fc, nopOpenCh)
		s.Close()
	}()
	io.ReadAll(c)
	c.Close()
	<-done

	// 取得 list
	client, server := net.Pipe()
	defer client.Close()
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		fm.HandleListForward(server)
		server.Close()
	}()
	resp, _ := io.ReadAll(client)
	<-done2

	respStr := string(resp)
	// 前 4 bytes 是 OKAY
	if !strings.HasPrefix(respStr, "OKAY") {
		t.Fatalf("expected 'OKAY' prefix, got %q", respStr)
	}
	// hex-len + body
	body := respStr[4:]
	if len(body) < 4 {
		t.Fatalf("response too short after OKAY: %q", body)
	}
	hexLen := body[:4]
	n, err := strconv.ParseInt(hexLen, 16, 32)
	if err != nil {
		t.Fatalf("failed to parse hex length %q: %v", hexLen, err)
	}
	list := body[4:]
	if int64(len(list)) != n {
		t.Errorf("list length mismatch: got %d, hex said %d", len(list), n)
	}
	// 驗證格式包含 serial 和 remoteSpec
	if !strings.Contains(list, "TEST001") {
		t.Errorf("list should contain serial 'TEST001', got %q", list)
	}
	if !strings.Contains(list, "localabstract:scrcpy") {
		t.Errorf("list should contain remoteSpec, got %q", list)
	}
	if !strings.HasSuffix(list, "\n") {
		t.Errorf("list should end with newline, got %q", list)
	}
}

// TestCloseFwdListeners 驗證 CloseFwdListeners 關閉所有 listener 並將 map 設為 nil。
func TestCloseFwdListeners(t *testing.T) {
	fm := newTestForwardManager()

	// 建立一個 forward
	c, s := net.Pipe()
	fc := &FwdCmd{LocalSpec: "tcp:0", RemoteSpec: "tcp:5555"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fm.HandleForward(context.Background(), s, fc, nopOpenCh)
		s.Close()
	}()
	io.ReadAll(c)
	c.Close()
	<-done

	fm.CloseFwdListeners()

	fm.fwdMu.Lock()
	isNil := fm.fwdListeners == nil
	fm.fwdMu.Unlock()
	if !isNil {
		t.Error("fwdListeners should be nil after CloseFwdListeners")
	}
}

// =============================================================================
// 4. Reverse Forward 操作測試
// =============================================================================

// TestSetupReverseForward_Success 驗證建立 reverse forward 回傳 port > 0 且有 entry。
func TestSetupReverseForward_Success(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	port, err := fm.SetupReverseForward(context.Background(), "TEST001", "tcp:0", "localabstract:scrcpy", nopOpenCh)
	if err != nil {
		t.Fatalf("SetupReverseForward error: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Errorf("port out of range: %d", port)
	}

	fm.fwdMu.Lock()
	count := len(fm.fwdListeners)
	fm.fwdMu.Unlock()
	if count != 1 {
		t.Errorf("expected 1 fwdListener, got %d", count)
	}
}

// TestSetupReverseForward_TcpZero 驗證 tcp:0 自動分配後 key 被更新為實際 port。
func TestSetupReverseForward_TcpZero(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	port, err := fm.SetupReverseForward(context.Background(), "TEST001", "tcp:0", "tcp:8080", nopOpenCh)
	if err != nil {
		t.Fatalf("SetupReverseForward error: %v", err)
	}

	expectedKey := fmt.Sprintf("tcp:%d", port)
	fm.fwdMu.Lock()
	_, found := fm.fwdListeners[expectedKey]
	_, foundZero := fm.fwdListeners["tcp:0"]
	fm.fwdMu.Unlock()

	if !found {
		t.Errorf("expected key %q in fwdListeners", expectedKey)
	}
	if foundZero {
		t.Error("key 'tcp:0' should not exist; should be replaced by actual port")
	}
}

// TestKillReverseForward_Success 驗證建立後 kill 回傳 true。
func TestKillReverseForward_Success(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	_, err := fm.SetupReverseForward(context.Background(), "TEST001", "tcp:0", "localabstract:scrcpy", nopOpenCh)
	if err != nil {
		t.Fatalf("SetupReverseForward error: %v", err)
	}

	ok := fm.KillReverseForward("localabstract:scrcpy")
	if !ok {
		t.Error("KillReverseForward should return true")
	}

	fm.fwdMu.Lock()
	count := len(fm.fwdListeners)
	fm.fwdMu.Unlock()
	if count != 0 {
		t.Errorf("expected 0 fwdListeners after kill, got %d", count)
	}
}

// TestKillReverseForward_NotFound 驗證未建立任何 forward 時 kill 回傳 false。
func TestKillReverseForward_NotFound(t *testing.T) {
	fm := NewForwardManager()

	ok := fm.KillReverseForward("localabstract:scrcpy")
	if ok {
		t.Error("KillReverseForward should return false for non-existent entry")
	}
}

// TestKillReverseForwardAll 驗證建立多個 reverse forward 後全部 kill。
func TestKillReverseForwardAll(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	for i := 0; i < 3; i++ {
		_, err := fm.SetupReverseForward(context.Background(), "TEST001", "tcp:0", fmt.Sprintf("tcp:%d", 8000+i), nopOpenCh)
		if err != nil {
			t.Fatalf("SetupReverseForward[%d] error: %v", i, err)
		}
	}

	fm.fwdMu.Lock()
	before := len(fm.fwdListeners)
	fm.fwdMu.Unlock()
	if before != 3 {
		t.Fatalf("expected 3 fwdListeners, got %d", before)
	}

	fm.KillReverseForwardAll()

	fm.fwdMu.Lock()
	after := len(fm.fwdListeners)
	fm.fwdMu.Unlock()
	if after != 0 {
		t.Errorf("expected 0 fwdListeners after kill-all, got %d", after)
	}
}

// TestListReverseForwards 驗證 list 格式 "serial remoteSpec localSpec\n"。
func TestListReverseForwards(t *testing.T) {
	fm := newTestForwardManager()
	defer fm.CloseFwdListeners()

	port, err := fm.SetupReverseForward(context.Background(), "TEST001", "tcp:0", "localabstract:scrcpy", nopOpenCh)
	if err != nil {
		t.Fatalf("SetupReverseForward error: %v", err)
	}

	list := fm.ListReverseForwards()
	if list == "" {
		t.Fatal("expected non-empty list")
	}

	expectedLocal := fmt.Sprintf("tcp:%d", port)
	if !strings.Contains(list, "TEST001") {
		t.Errorf("list should contain serial 'TEST001', got %q", list)
	}
	if !strings.Contains(list, "localabstract:scrcpy") {
		t.Errorf("list should contain remoteSpec, got %q", list)
	}
	if !strings.Contains(list, expectedLocal) {
		t.Errorf("list should contain localSpec %q, got %q", expectedLocal, list)
	}
	if !strings.HasSuffix(list, "\n") {
		t.Errorf("list should end with newline, got %q", list)
	}

	// 驗證無 entry 時回傳空字串
	fm.KillReverseForwardAll()
	emptyList := fm.ListReverseForwards()
	if emptyList != "" {
		t.Errorf("expected empty string, got %q", emptyList)
	}
}
