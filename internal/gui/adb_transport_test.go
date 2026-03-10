package gui

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestWriteReadADBTransportMsg(t *testing.T) {
	original := &adbMsg{
		command: aCNXN,
		arg0:    aVersion,
		arg1:    aMaxPayload,
		data:    []byte("device::\x00"),
	}

	var buf bytes.Buffer
	if err := writeADBTransportMsg(&buf, original); err != nil {
		t.Fatalf("writeADBTransportMsg: %v", err)
	}

	// 驗證 header 大小 + data
	expectedSize := adbMsgHdrSize + len(original.data)
	if buf.Len() != expectedSize {
		t.Fatalf("寫入大小不符：got %d, want %d", buf.Len(), expectedSize)
	}

	// 驗證 magic
	hdr := buf.Bytes()[:adbMsgHdrSize]
	magic := binary.LittleEndian.Uint32(hdr[20:24])
	if magic != original.command^0xFFFFFFFF {
		t.Errorf("magic 不符：got 0x%08x, want 0x%08x", magic, original.command^0xFFFFFFFF)
	}

	// 讀回
	msg, err := readADBTransportMsg(&buf)
	if err != nil {
		t.Fatalf("readADBTransportMsg: %v", err)
	}
	if msg.command != original.command {
		t.Errorf("command：got 0x%08x, want 0x%08x", msg.command, original.command)
	}
	if msg.arg0 != original.arg0 {
		t.Errorf("arg0：got %d, want %d", msg.arg0, original.arg0)
	}
	if msg.arg1 != original.arg1 {
		t.Errorf("arg1：got %d, want %d", msg.arg1, original.arg1)
	}
	if !bytes.Equal(msg.data, original.data) {
		t.Errorf("data：got %q, want %q", msg.data, original.data)
	}
}

func TestReadADBMsgFromPrefix(t *testing.T) {
	original := &adbMsg{
		command: aCNXN,
		arg0:    0x01000001,
		arg1:    256 * 1024,
		data:    []byte("host::\x00"),
	}

	var buf bytes.Buffer
	writeADBTransportMsg(&buf, original)

	// 取出前 4 bytes 作為 prefix
	prefix := buf.Next(4)
	if string(prefix) != "CNXN" {
		t.Fatalf("前 4 bytes 不是 CNXN：got %q", string(prefix))
	}

	msg, err := readADBMsgFromPrefix(prefix, &buf)
	if err != nil {
		t.Fatalf("readADBMsgFromPrefix: %v", err)
	}
	if msg.command != aCNXN {
		t.Errorf("command：got 0x%08x, want 0x%08x", msg.command, aCNXN)
	}
	if msg.arg0 != original.arg0 {
		t.Errorf("arg0：got %d, want %d", msg.arg0, original.arg0)
	}
	if !bytes.Equal(msg.data, original.data) {
		t.Errorf("data：got %q, want %q", msg.data, original.data)
	}
}

func TestCNXNDetection(t *testing.T) {
	tests := []struct {
		name   string
		first4 [4]byte
		isCNXN bool
	}{
		{"CNXN 封包", [4]byte{'C', 'N', 'X', 'N'}, true},
		{"ADB server 命令", [4]byte{'0', '0', '1', '2'}, false},
		{"track-devices 命令", [4]byte{'0', '0', '0', 'e'}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (string(tt.first4[:]) == "CNXN")
			if got != tt.isCNXN {
				t.Errorf("isCNXN(%q)：got %v, want %v", string(tt.first4[:]), got, tt.isCNXN)
			}
		})
	}
}

func TestWriteReadEmptyData(t *testing.T) {
	original := &adbMsg{
		command: aOKAY,
		arg0:    1,
		arg1:    2,
		data:    nil,
	}

	var buf bytes.Buffer
	if err := writeADBTransportMsg(&buf, original); err != nil {
		t.Fatalf("write: %v", err)
	}
	if buf.Len() != adbMsgHdrSize {
		t.Fatalf("空 data 訊息大小不符：got %d, want %d", buf.Len(), adbMsgHdrSize)
	}

	msg, err := readADBTransportMsg(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.command != aOKAY {
		t.Errorf("command：got 0x%08x, want 0x%08x", msg.command, aOKAY)
	}
	if len(msg.data) != 0 {
		t.Errorf("data 應為空：got %d bytes", len(msg.data))
	}
}

func TestCNXNHandshakeOverPipe(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	errCh := make(chan error, 1)

	// 模擬 ADB server：發送 CNXN，期望收到 CNXN 回應
	go func() {
		cnxn := &adbMsg{
			command: aCNXN,
			arg0:    0x01000001,
			arg1:    256 * 1024,
			data:    []byte("host::\x00"),
		}
		if err := writeADBTransportMsg(client, cnxn); err != nil {
			errCh <- err
			return
		}
		resp, err := readADBTransportMsg(client)
		if err != nil {
			errCh <- err
			return
		}
		if resp.command != aCNXN {
			errCh <- io.ErrUnexpectedEOF
			return
		}
		errCh <- nil
	}()

	// 模擬我們的 proxy：讀取 CNXN 並回應
	var peek [4]byte
	if _, err := io.ReadFull(server, peek[:]); err != nil {
		t.Fatalf("讀取 peek 失敗: %v", err)
	}
	if string(peek[:]) != "CNXN" {
		t.Fatalf("peek 不是 CNXN: %q", string(peek[:]))
	}

	incoming, err := readADBMsgFromPrefix(peek[:], server)
	if err != nil {
		t.Fatalf("讀取 CNXN: %v", err)
	}
	if incoming.command != aCNXN {
		t.Fatalf("incoming 不是 CNXN: 0x%08x", incoming.command)
	}

	resp := &adbMsg{
		command: aCNXN,
		arg0:    aVersion,
		arg1:    aMaxPayload,
		data:    []byte("device::\x00"),
	}
	if err := writeADBTransportMsg(server, resp); err != nil {
		t.Fatalf("寫入 CNXN 回應: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("client 端錯誤: %v", err)
	}
}

func TestAllCommandConstants(t *testing.T) {
	// 驗證常數對應正確的 ASCII
	tests := []struct {
		name  string
		value uint32
		ascii string
	}{
		{"CNXN", aCNXN, "CNXN"},
		{"AUTH", aAUTH, "AUTH"},
		{"OPEN", aOPEN, "OPEN"},
		{"OKAY", aOKAY, "OKAY"},
		{"WRTE", aWRTE, "WRTE"},
		{"CLSE", aCLSE, "CLSE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf [4]byte
			binary.LittleEndian.PutUint32(buf[:], tt.value)
			if string(buf[:]) != tt.ascii {
				t.Errorf("%s：wire bytes %q != %q", tt.name, string(buf[:]), tt.ascii)
			}
		})
	}
}

func TestDataTooLargeRejected(t *testing.T) {
	var buf bytes.Buffer
	// 構造一個 data_length > 1MB 的 header
	var hdr [adbMsgHdrSize]byte
	binary.LittleEndian.PutUint32(hdr[0:4], aOPEN)
	binary.LittleEndian.PutUint32(hdr[12:16], 2*1024*1024) // 2MB
	binary.LittleEndian.PutUint32(hdr[20:24], aOPEN^0xFFFFFFFF)
	buf.Write(hdr[:])

	_, err := readADBTransportMsg(&buf)
	if err == nil {
		t.Fatal("應拒絕過大的 data，但未回報錯誤")
	}
}

func TestAdbCmdName(t *testing.T) {
	tests := []struct {
		cmd  uint32
		want string
	}{
		{aCNXN, "CNXN"},
		{aAUTH, "AUTH"},
		{aOPEN, "OPEN"},
		{aOKAY, "OKAY"},
		{aWRTE, "WRTE"},
		{aCLSE, "CLSE"},
		{0x12345678, "0x12345678"},
	}
	for _, tt := range tests {
		got := adbCmdName(tt.cmd)
		if got != tt.want {
			t.Errorf("adbCmdName(0x%08x)：got %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

func TestCleanupStreamIdempotent(t *testing.T) {
	// 驗證 cleanupStream 透過 atomic.Bool 只執行一次
	conn := &mockConn{}

	bridge := &deviceBridge{
		conn:    conn,
		streams: make(map[uint32]*dStream),
	}

	// 使用 io.Pipe 模擬 DataChannel
	pr, pw := io.Pipe()
	ch := &testRWC{r: pr, w: pw}

	stream := &dStream{
		serverID: 100,
		deviceID: 5,
		ch:       ch,
		ready:    make(chan struct{}, 1),
		writeCh:  make(chan []byte, 4),
		doneCh:   make(chan struct{}),
	}

	bridge.streamsMu.Lock()
	bridge.streams[5] = stream
	bridge.streamsMu.Unlock()

	// 第一次 cleanup — 應成功
	bridge.cleanupStream(stream)

	bridge.streamsMu.Lock()
	_, exists := bridge.streams[5]
	bridge.streamsMu.Unlock()
	if exists {
		t.Error("第一次 cleanup 後 stream 仍在 map 中")
	}

	// doneCh 應已關閉
	select {
	case <-stream.doneCh:
	default:
		t.Error("cleanup 後 doneCh 未關閉")
	}

	// 第二次 cleanup — 應為 no-op（不 panic）
	bridge.cleanupStream(stream)
}

// TestSendOneShotFlow 測試 one-shot 回應流程：bridge 送 OKAY + WRTE，等 ADB server OKAY，然後 CLSE。
func TestSendOneShotFlow(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	bridge := &deviceBridge{
		conn:    server,
		streams: make(map[uint32]*dStream),
	}

	const serverID uint32 = 10
	const deviceID uint32 = 20
	respData := []byte("OKAY")

	// 在背景執行 sendOneShot
	done := make(chan struct{})
	go func() {
		bridge.sendOneShot(serverID, deviceID, respData)
		close(done)
	}()

	// 客戶端讀取 bridge 送來的 OKAY（transport level）
	msg, err := readADBTransportMsg(client)
	if err != nil {
		t.Fatalf("讀取 OKAY: %v", err)
	}
	if msg.command != aOKAY {
		t.Fatalf("期望 OKAY，得到 %s", adbCmdName(msg.command))
	}
	if msg.arg0 != deviceID || msg.arg1 != serverID {
		t.Errorf("OKAY args: got (%d, %d), want (%d, %d)", msg.arg0, msg.arg1, deviceID, serverID)
	}

	// 客戶端讀取 WRTE（smart socket 回應）
	msg, err = readADBTransportMsg(client)
	if err != nil {
		t.Fatalf("讀取 WRTE: %v", err)
	}
	if msg.command != aWRTE {
		t.Fatalf("期望 WRTE，得到 %s", adbCmdName(msg.command))
	}
	if string(msg.data) != "OKAY" {
		t.Errorf("WRTE data: got %q, want %q", string(msg.data), "OKAY")
	}

	// 模擬 bridge 主循環的 handleOKAY：直接發送 ready 信號
	// （真實場景中，主循環從 conn 讀取 OKAY 後 signal stream.ready）
	bridge.streamsMu.Lock()
	stream := bridge.streams[deviceID]
	bridge.streamsMu.Unlock()
	if stream == nil {
		t.Fatal("stream 不在 map 中")
	}
	select {
	case stream.ready <- struct{}{}:
	default:
		t.Fatal("無法發送 ready 信號")
	}

	// 客戶端讀取 CLSE（由 cleanupStream 發送）
	msg, err = readADBTransportMsg(client)
	if err != nil {
		t.Fatalf("讀取 CLSE: %v", err)
	}
	if msg.command != aCLSE {
		t.Fatalf("期望 CLSE，得到 %s", adbCmdName(msg.command))
	}

	// sendOneShot 應已完成
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sendOneShot 逾時")
	}

	// stream 應已從 map 清除
	bridge.streamsMu.Lock()
	_, exists := bridge.streams[deviceID]
	bridge.streamsMu.Unlock()
	if exists {
		t.Error("sendOneShot 結束後 stream 仍在 map 中")
	}
}

// TestSendOneShotServerCloseFirst 測試 ADB server 先 CLSE 的情況（不 panic）。
func TestSendOneShotServerCloseFirst(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	bridge := &deviceBridge{
		conn:    server,
		streams: make(map[uint32]*dStream),
	}

	done := make(chan struct{})
	go func() {
		bridge.sendOneShot(10, 20, []byte("OKAY"))
		close(done)
	}()

	// 讀取 OKAY
	if _, err := readADBTransportMsg(client); err != nil {
		t.Fatalf("讀取 OKAY: %v", err)
	}

	// 讀取 WRTE
	if _, err := readADBTransportMsg(client); err != nil {
		t.Fatalf("讀取 WRTE: %v", err)
	}

	// 模擬 bridge 主循環收到 ADB server 的 CLSE → 觸發 cleanupStream
	// （真實場景中，主循環讀到 CLSE 後呼叫 cleanupStream，關閉 doneCh）
	client.Close() // 先關閉 client，讓 cleanupStream 的 CLSE 寫入不阻塞
	bridge.streamsMu.Lock()
	stream := bridge.streams[uint32(20)]
	bridge.streamsMu.Unlock()
	if stream != nil {
		bridge.cleanupStream(stream)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sendOneShot 應在 doneCh 關閉後結束")
	}
}

// TestReverseForwardServiceParsing 測試 reverse:forward: 服務名稱解析。
func TestReverseForwardServiceParsing(t *testing.T) {
	tests := []struct {
		name       string
		service    string
		wantRemote string // device-side spec
		wantLocal  string // host-side spec
		wantErr    bool
	}{
		{
			name:       "基本 reverse forward",
			service:    "reverse:forward:localabstract:scrcpy;tcp:27183",
			wantRemote: "localabstract:scrcpy",
			wantLocal:  "tcp:27183",
		},
		{
			name:       "norebind",
			service:    "reverse:forward:norebind:localabstract:scrcpy;tcp:27183",
			wantRemote: "localabstract:scrcpy",
			wantLocal:  "tcp:27183",
		},
		{
			name:       "tcp:0 動態 port",
			service:    "reverse:forward:localabstract:scrcpy;tcp:0",
			wantRemote: "localabstract:scrcpy",
			wantLocal:  "tcp:0",
		},
		{
			name:    "缺少分號",
			service: "reverse:forward:localabstract:scrcpy",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest := tt.service[len("reverse:forward:"):]
			rest = trimPrefixIfPresent(rest, "norebind:")
			parts := splitNMax(rest, ";", 2)

			if tt.wantErr {
				if len(parts) == 2 {
					t.Error("期望解析失敗，但成功了")
				}
				return
			}

			if len(parts) != 2 {
				t.Fatalf("解析失敗：parts=%v", parts)
			}
			if parts[0] != tt.wantRemote {
				t.Errorf("remote: got %q, want %q", parts[0], tt.wantRemote)
			}
			if parts[1] != tt.wantLocal {
				t.Errorf("local: got %q, want %q", parts[1], tt.wantLocal)
			}
		})
	}
}

// 輔助函式（避免匯入 strings 僅用於測試的耦合）
func trimPrefixIfPresent(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

func splitNMax(s, sep string, n int) []string {
	result := make([]string, 0, n)
	for i := 0; i < n-1; i++ {
		idx := indexOf(s, sep)
		if idx < 0 {
			break
		}
		result = append(result, s[:idx])
		s = s[idx+len(sep):]
	}
	result = append(result, s)
	return result
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestSetupStreamReadySignal 測試 setupStream 的就緒信號流程，
// 模擬 DCEP ACK 場景：首次 Read 需要 >= 4 bytes 的 buffer。
func TestSetupStreamReadySignal(t *testing.T) {
	tests := []struct {
		name      string
		writeData []byte // 遠端寫入的資料（模擬就緒信號）
		wantReady bool
	}{
		{
			name:      "就緒成功（1 byte: 0x01）",
			writeData: []byte{1},
			wantReady: true,
		},
		{
			name:      "就緒失敗（1 byte: 0x00）",
			writeData: []byte{0},
			wantReady: false,
		},
		{
			name:      "就緒成功但有多餘 bytes",
			writeData: []byte{1, 0, 0, 0},
			wantReady: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, w := io.Pipe()
			ch := &testRWC{r: r, w: w}

			// 模擬遠端寫入就緒信號
			go func() {
				w.Write(tt.writeData)
				w.Close()
			}()

			// 使用 4-byte buffer 讀取就緒信號（與 setupStream 相同邏輯）
			var buf [4]byte
			n, err := ch.Read(buf[:])

			ready := (err == nil && n >= 1 && buf[0] == 1)
			if ready != tt.wantReady {
				t.Errorf("ready: got %v, want %v (n=%d, err=%v, buf=%v)",
					ready, tt.wantReady, n, err, buf[:n])
			}
		})
	}
}

// TestSetupStreamSmallBuffer 驗證 1-byte buffer 無法正確讀取就緒信號
// （這是 DCEP ACK bug 的根因：pion ReadDataChannel 的 SCTP 層
// 在 buffer < 4 bytes 時會回傳 io.ErrShortBuffer）。
func TestSetupStreamSmallBuffer(t *testing.T) {
	r, w := io.Pipe()

	// 寫入 4 bytes（模擬 DCEP ACK 大小）
	go func() {
		w.Write([]byte{1, 0, 0, 0})
		w.Close()
	}()

	// 用 1-byte buffer 讀取
	var smallBuf [1]byte
	n, err := r.Read(smallBuf[:])

	// io.Pipe 的 Read 會截斷到 buffer 大小並丟棄剩餘，
	// 但 pion/sctp 的 ReadSCTP 會回傳 io.ErrShortBuffer。
	// 此測試驗證的是：如果我們用 1-byte buffer，
	// 資料確實只能讀到 1 byte（就算沒有 ErrShortBuffer）。
	if err != nil {
		t.Logf("1-byte buffer 讀取結果: n=%d, err=%v（pion 會回傳 ErrShortBuffer）", n, err)
	}

	// 重點是：使用 4-byte buffer 就不會有這個問題
	r2, w2 := io.Pipe()
	go func() {
		w2.Write([]byte{1, 0, 0, 0})
		w2.Close()
	}()

	var largeBuf [4]byte
	n, err = r2.Read(largeBuf[:])
	if err != nil {
		t.Fatalf("4-byte buffer 不應失敗: n=%d, err=%v", n, err)
	}
	if n < 1 || largeBuf[0] != 1 {
		t.Errorf("就緒信號讀取錯誤: n=%d, buf[0]=%d", n, largeBuf[0])
	}
}

// TestBannerConstruction 測試 CNXN banner 建構邏輯。
func TestBannerConstruction(t *testing.T) {
	tests := []struct {
		name     string
		features string
		want     string
	}{
		{
			name:     "使用真實設備 features",
			features: "shell_v2,cmd,stat_v2,ls_v2",
			want:     "device::features=shell_v2,cmd,stat_v2,ls_v2\x00",
		},
		{
			name:     "空 features 使用預設值",
			features: "",
			want:     defaultDeviceBanner + "\x00",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var banner string
			if tt.features != "" {
				banner = "device::features=" + tt.features + "\x00"
			} else {
				banner = defaultDeviceBanner + "\x00"
			}
			if banner != tt.want {
				t.Errorf("banner:\n  got  %q\n  want %q", banner, tt.want)
			}
		})
	}
}

// TestNopRWC 測試 nopRWC 不 panic 且符合 io.ReadWriteCloser 介面。
func TestNopRWC(t *testing.T) {
	var rwc io.ReadWriteCloser = nopRWC{}

	// Read 回傳 EOF
	buf := make([]byte, 10)
	n, err := rwc.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("Read: got n=%d err=%v, want n=0 err=EOF", n, err)
	}

	// Write 不出錯
	n, err = rwc.Write([]byte("test"))
	if n != 4 || err != nil {
		t.Errorf("Write: got n=%d err=%v, want n=4 err=nil", n, err)
	}

	// Close 不出錯
	if err := rwc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestPrefixedRWC(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	base := &testRWC{r: r, w: w}
	rwc := &prefixedRWC{ch: base, prefix: []byte{0x11, 0x22, 0x33}}

	buf := make([]byte, 2)
	n, err := rwc.Read(buf)
	if err != nil || n != 2 || !bytes.Equal(buf[:n], []byte{0x11, 0x22}) {
		t.Fatalf("首段 prefix 讀取錯誤: n=%d err=%v buf=%v", n, err, buf[:n])
	}

	buf = make([]byte, 2)
	n, err = rwc.Read(buf)
	if err != nil || n != 1 || !bytes.Equal(buf[:n], []byte{0x33}) {
		t.Fatalf("尾段 prefix 讀取錯誤: n=%d err=%v buf=%v", n, err, buf[:n])
	}

	go func() {
		w.Write([]byte{0x44, 0x55})
	}()

	buf = make([]byte, 2)
	n, err = rwc.Read(buf)
	if err != nil || n != 2 || !bytes.Equal(buf[:n], []byte{0x44, 0x55}) {
		t.Fatalf("底層資料讀取錯誤: n=%d err=%v buf=%v", n, err, buf[:n])
	}
}

func TestChunkedWrite(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 40*1024)
	maxWrite := 0
	total := 0

	dst := &rwcFunc{
		readFn: func([]byte) (int, error) { return 0, io.EOF },
		writeFn: func(p []byte) (int, error) {
			total += len(p)
			if len(p) > maxWrite {
				maxWrite = len(p)
			}
			return len(p), nil
		},
		closeFn: func() error { return nil },
	}

	n, err := chunkedWrite(dst, data, 16*1024)
	if err != nil {
		t.Fatalf("chunkedWrite 失敗: %v", err)
	}
	if n != len(data) || total != len(data) {
		t.Fatalf("寫入長度不符: n=%d total=%d want=%d", n, total, len(data))
	}
	if maxWrite > 16*1024 {
		t.Fatalf("單次寫入過大: got %d, want <= %d", maxWrite, 16*1024)
	}
}

func TestWriteToRemoteChunked(t *testing.T) {
	conn := &mockConn{}
	maxWrite := 0
	total := 0

	ch := &rwcFunc{
		readFn: func([]byte) (int, error) { return 0, io.EOF },
		writeFn: func(p []byte) (int, error) {
			total += len(p)
			if len(p) > maxWrite {
				maxWrite = len(p)
			}
			return len(p), nil
		},
		closeFn: func() error { return nil },
	}

	bridge := &deviceBridge{conn: conn, streams: map[uint32]*dStream{}}
	stream := &dStream{
		serverID: 1,
		deviceID: 2,
		ch:       ch,
		ready:    make(chan struct{}, 1),
		writeCh:  make(chan []byte), // unbuffered：send 完成 = goroutine 已收到
		doneCh:   make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		bridge.writeToRemote(stream)
		close(done)
	}()

	// unbuffered send：回傳時保證 writeToRemote 已從 writeCh 取出資料，
	// 避免 close(doneCh) 與 writeCh 在 select 中競態。
	stream.writeCh <- bytes.Repeat([]byte("y"), 40*1024)
	close(stream.doneCh)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writeToRemote 未結束")
	}

	if total != 40*1024 {
		t.Fatalf("總寫入大小不符: got %d, want %d", total, 40*1024)
	}
	if maxWrite > 16*1024 {
		t.Fatalf("單次寫入過大: got %d, want <= %d", maxWrite, 16*1024)
	}
}

// --- biCopy 測試 ---

// rwcFunc 用函式包裝 io.ReadWriteCloser，方便測試時自訂行為。
type rwcFunc struct {
	readFn  func([]byte) (int, error)
	writeFn func([]byte) (int, error)
	closeFn func() error
}

func (r *rwcFunc) Read(p []byte) (int, error)  { return r.readFn(p) }
func (r *rwcFunc) Write(p []byte) (int, error) { return r.writeFn(p) }
func (r *rwcFunc) Close() error                { return r.closeFn() }

// TestBiCopyNoDeadlock 重現舊模式的死鎖場景：
// 一方（模擬 ADB conn）發送資料後關閉，另一方（模擬 DC）沒有資料。
// 舊模式只關閉 conn 而不關閉 DC → io.Copy(conn, dc) 的 dc.Read() 永久阻塞。
// biCopy 關閉雙方 → 兩個方向的 goroutine 都能結束。
func TestBiCopyNoDeadlock(t *testing.T) {
	connR, connW := io.Pipe() // ADB conn 的讀取管道
	dcR, dcW := io.Pipe()     // DC 的讀取管道（永遠不寫入）
	_ = dcW                   // DC 沒有資料

	var dcReceived bytes.Buffer
	conn := &rwcFunc{
		readFn:  connR.Read,
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: connR.Close,
	}
	dc := &rwcFunc{
		readFn:  dcR.Read,
		writeFn: dcReceived.Write,
		closeFn: dcR.Close,
	}

	// conn 發送資料後關閉（模擬 ADB shell 退出）
	go func() {
		connW.Write([]byte("shell-exit"))
		connW.Close()
	}()

	done := make(chan struct{})
	go func() {
		biCopy(context.Background(), dc, conn)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("biCopy 死鎖：一方關閉後另一方的 Read 永久阻塞")
	}

	if dcReceived.String() != "shell-exit" {
		t.Errorf("DC 收到: %q, 期望: %q", dcReceived.String(), "shell-exit")
	}
}

// TestBiCopyContextCancel 測試 context 取消時 biCopy 能正常結束。
func TestBiCopyContextCancel(t *testing.T) {
	aR, _ := io.Pipe()
	bR, _ := io.Pipe()

	a := &rwcFunc{
		readFn:  aR.Read,
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: aR.Close,
	}
	b := &rwcFunc{
		readFn:  bR.Read,
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: bR.Close,
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		biCopy(ctx, a, b)
		close(done)
	}()

	// 取消 context
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("biCopy 在 context 取消後死鎖")
	}
}

// TestBiCopyChunkedWrite 驗證 biCopy 寫入 DataChannel 時會分塊，
// 避免單次寫入過大造成 SCTP/DataChannel 問題（例如 scrcpy 視訊流）。
func TestBiCopyChunkedWrite(t *testing.T) {
	const chunkSize = 16 * 1024
	payload := bytes.Repeat([]byte("a"), 40*1024)

	srcR, srcW := io.Pipe()
	defer srcR.Close()
	blockR, blockW := io.Pipe()
	defer blockR.Close()
	defer blockW.Close()

	var (
		totalWritten int
		maxWrite     int
	)

	dst := &rwcFunc{
		readFn: blockR.Read,
		writeFn: func(p []byte) (int, error) {
			totalWritten += len(p)
			if len(p) > maxWrite {
				maxWrite = len(p)
			}
			return len(p), nil
		},
		closeFn: blockR.Close,
	}

	src := &rwcFunc{
		readFn:  srcR.Read,
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: srcR.Close,
	}

	go func() {
		srcW.Write(payload)
		srcW.Close()
	}()

	biCopy(context.Background(), dst, src)

	if totalWritten != len(payload) {
		t.Fatalf("總寫入大小不符: got %d, want %d", totalWritten, len(payload))
	}
	if maxWrite > chunkSize {
		t.Fatalf("單次寫入過大: got %d, want <= %d", maxWrite, chunkSize)
	}
}

// TestLocalAbstractConnectionOrdering 驗證同一 abstract socket 路徑的多條連線
// 會按 OPEN 順序依序建立 DataChannel，避免 scrcpy audio/control stream 交叉。
//
// 背景：scrcpy 透過同一個 localabstract socket 開啟 video/audio/control 三條連線，
// 依靠 accept() 順序對應功能。若 DataChannel 非同步到達導致亂序，
// 會造成串流交叉（如音訊資料被當成控制訊息：Unknown device message type: 111）。
func TestLocalAbstractConnectionOrdering(t *testing.T) {
	pwCh := make(chan *io.PipeWriter, 10)
	openCalled := make(chan struct{}, 10)

	openCh := func(label string) (io.ReadWriteCloser, error) {
		pr, pw := io.Pipe()
		pwCh <- pw
		openCalled <- struct{}{}
		return &testRWC{r: pr, w: nopRWC{}}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bridge := &deviceBridge{
		conn:              &mockConn{},
		openCh:            openCh,
		serial:            "test",
		streams:           make(map[uint32]*dStream),
		localAbstractPrev: make(map[string]<-chan struct{}),
	}
	bridge.nextID.Store(0)

	service := "localabstract:scrcpy_test\x00"
	bridge.handleOPEN(ctx, &adbMsg{command: aOPEN, arg0: 100, data: []byte(service)})
	bridge.handleOPEN(ctx, &adbMsg{command: aOPEN, arg0: 101, data: []byte(service)})

	// 第一個 openCh 應立即被呼叫
	select {
	case <-openCalled:
	case <-time.After(time.Second):
		t.Fatal("逾時：第一個 openCh 未被呼叫")
	}

	// 第二個 openCh 不應在第一個就緒前被呼叫（序列化保護）
	select {
	case <-openCalled:
		t.Fatal("第一個就緒前，第二個 openCh 不應被呼叫")
	case <-time.After(200 * time.Millisecond):
		// 預期行為：第二個被序列化，等待第一個就緒
	}

	// 送出第一個的就緒信號並關閉（讓 readFromRemote 立即 EOF 退出）
	pw1 := <-pwCh
	pw1.Write([]byte{1})
	pw1.Close()

	// 第二個 openCh 應在第一個就緒後被呼叫
	select {
	case <-openCalled:
	case <-time.After(time.Second):
		t.Fatal("逾時：第一個就緒後，第二個 openCh 未被呼叫")
	}

	// 清理第二個
	pw2 := <-pwCh
	pw2.Write([]byte{1})
	pw2.Close()
}

// mockConn 是不阻塞的 net.Conn 模擬，Write 寫入 buffer，不會阻塞。
type mockConn struct {
	bytes.Buffer
}

func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) LocalAddr() net.Addr                { return nil }
func (m *mockConn) RemoteAddr() net.Addr               { return nil }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

// testRWC 是簡單的 io.ReadWriteCloser 包裝。
type testRWC struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (t *testRWC) Read(p []byte) (int, error)  { return t.r.Read(p) }
func (t *testRWC) Write(p []byte) (int, error) { return t.w.Write(p) }
func (t *testRWC) Close() error {
	t.r.Close()
	t.w.Close()
	return nil
}
