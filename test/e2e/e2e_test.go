// Package e2e 包含跨模組的端對端整合測試。
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	ws "github.com/coder/websocket"

	"github.com/chris1004tw/remote-adb/internal/proxy"
	signalpkg "github.com/chris1004tw/remote-adb/internal/signal"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
)

// --- 信令完整流程 E2E ---

func TestSignalingFlow(t *testing.T) {
	// 1. 啟動 Signal Server
	hub := signalpkg.NewHub()
	auth := signalpkg.NewPSKAuth("test-token")
	srv := signalpkg.NewServer(hub, auth)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wsURL := "ws" + ts.URL[4:] + "/ws"
	ctx := context.Background()

	// 2. Agent 連線並認證
	agentConn, agentID := connectAndAuth(t, ctx, wsURL, protocol.RoleAgent)
	defer agentConn.CloseNow()

	// 3. Agent 註冊設備
	sendMsg(t, ctx, agentConn, protocol.MsgTypeRegister, agentID, "", protocol.RegisterPayload{
		HostID:   agentID,
		Hostname: "lab-server",
		Devices: []protocol.DeviceInfo{
			{Serial: "pixel-7", State: protocol.DeviceStateOnline, Lock: protocol.LockAvailable},
			{Serial: "galaxy-s24", State: protocol.DeviceStateOnline, Lock: protocol.LockAvailable},
		},
	})

	// 等待 Signal Server 處理註冊
	time.Sleep(50 * time.Millisecond)

	// 4. Client 連線並認證
	clientConn, clientID := connectAndAuth(t, ctx, wsURL, protocol.RoleClient)
	defer clientConn.CloseNow()

	// 5. Client 查詢主機列表
	sendMsg(t, ctx, clientConn, protocol.MsgTypeHostList, clientID, "", struct{}{})
	hostListMsg := readMsg(t, ctx, clientConn)

	if hostListMsg.Type != protocol.MsgTypeHostListResp {
		t.Fatalf("預期 host_list_resp，收到 %s", hostListMsg.Type)
	}

	var hostList protocol.HostListRespPayload
	hostListMsg.DecodePayload(&hostList)
	if len(hostList.Hosts) != 1 {
		t.Fatalf("預期 1 台主機，收到 %d", len(hostList.Hosts))
	}
	if hostList.Hosts[0].Hostname != "lab-server" {
		t.Errorf("主機名稱 = %s，預期 lab-server", hostList.Hosts[0].Hostname)
	}
	if len(hostList.Hosts[0].Devices) != 2 {
		t.Fatalf("預期 2 個設備，收到 %d", len(hostList.Hosts[0].Devices))
	}

	// 6. Client 發送 lock_req → 轉送到 Agent
	sendMsg(t, ctx, clientConn, protocol.MsgTypeLockReq, clientID, agentID,
		protocol.LockReqPayload{HostID: agentID, Serial: "pixel-7"})

	lockReqMsg := readMsg(t, ctx, agentConn)
	if lockReqMsg.Type != protocol.MsgTypeLockReq {
		t.Fatalf("Agent 應收到 lock_req，收到 %s", lockReqMsg.Type)
	}

	// 7. Agent 回應 lock_resp → 轉送回 Client
	sendMsg(t, ctx, agentConn, protocol.MsgTypeLockResp, agentID, clientID,
		protocol.LockRespPayload{Success: true, Serial: "pixel-7"})

	lockRespMsg := readMsg(t, ctx, clientConn)
	if lockRespMsg.Type != protocol.MsgTypeLockResp {
		t.Fatalf("Client 應收到 lock_resp，收到 %s", lockRespMsg.Type)
	}
	var lockResp protocol.LockRespPayload
	lockRespMsg.DecodePayload(&lockResp)
	if !lockResp.Success {
		t.Fatal("鎖定應成功")
	}
	if lockResp.Serial != "pixel-7" {
		t.Errorf("Serial = %s，預期 pixel-7", lockResp.Serial)
	}

	// 8. SDP 交換：Client → Agent（offer）
	sendMsg(t, ctx, clientConn, protocol.MsgTypeOffer, clientID, agentID,
		protocol.SDPPayload{SDP: "v=0\r\noffer-sdp...", Type: "offer"})

	offerMsg := readMsg(t, ctx, agentConn)
	if offerMsg.Type != protocol.MsgTypeOffer {
		t.Fatalf("Agent 應收到 offer，收到 %s", offerMsg.Type)
	}

	// 9. SDP 交換：Agent → Client（answer）
	sendMsg(t, ctx, agentConn, protocol.MsgTypeAnswer, agentID, clientID,
		protocol.SDPPayload{SDP: "v=0\r\nanswer-sdp...", Type: "answer"})

	answerMsg := readMsg(t, ctx, clientConn)
	if answerMsg.Type != protocol.MsgTypeAnswer {
		t.Fatalf("Client 應收到 answer，收到 %s", answerMsg.Type)
	}

	// 10. ICE Candidate 交換
	sendMsg(t, ctx, clientConn, protocol.MsgTypeCandidate, clientID, agentID,
		protocol.CandidatePayload{Candidate: "candidate:1 ...", SDPMid: "0", SDPMLineIndex: 0})

	iceMsg := readMsg(t, ctx, agentConn)
	if iceMsg.Type != protocol.MsgTypeCandidate {
		t.Fatalf("Agent 應收到 candidate，收到 %s", iceMsg.Type)
	}

	t.Log("完整信令流程 E2E 測試通過：auth → register → host_list → lock → offer/answer → ICE")
}

// TestAgentDisconnect_HostRemoved 驗證 Agent 斷線後主機列表自動清除。
func TestAgentDisconnect_HostRemoved(t *testing.T) {
	hub := signalpkg.NewHub()
	auth := signalpkg.NewPSKAuth("test-token")
	srv := signalpkg.NewServer(hub, auth)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wsURL := "ws" + ts.URL[4:] + "/ws"
	ctx := context.Background()

	// Agent 連線、註冊
	agentConn, agentID := connectAndAuth(t, ctx, wsURL, protocol.RoleAgent)
	sendMsg(t, ctx, agentConn, protocol.MsgTypeRegister, agentID, "", protocol.RegisterPayload{
		HostID:   agentID,
		Hostname: "temp-server",
		Devices:  []protocol.DeviceInfo{{Serial: "dev-1", State: protocol.DeviceStateOnline, Lock: protocol.LockAvailable}},
	})
	time.Sleep(50 * time.Millisecond)

	// Client 確認有 1 台主機
	clientConn, clientID := connectAndAuth(t, ctx, wsURL, protocol.RoleClient)
	defer clientConn.CloseNow()

	sendMsg(t, ctx, clientConn, protocol.MsgTypeHostList, clientID, "", struct{}{})
	msg := readMsg(t, ctx, clientConn)
	var hosts protocol.HostListRespPayload
	msg.DecodePayload(&hosts)
	if len(hosts.Hosts) != 1 {
		t.Fatalf("斷線前應有 1 台主機，收到 %d", len(hosts.Hosts))
	}

	// Agent 斷線
	agentConn.Close(ws.StatusNormalClosure, "bye")
	time.Sleep(100 * time.Millisecond)

	// Client 再次查詢 → 應為空
	sendMsg(t, ctx, clientConn, protocol.MsgTypeHostList, clientID, "", struct{}{})
	msg2 := readMsg(t, ctx, clientConn)
	var hosts2 protocol.HostListRespPayload
	msg2.DecodePayload(&hosts2)
	if len(hosts2.Hosts) != 0 {
		t.Errorf("斷線後應無主機，收到 %d", len(hosts2.Hosts))
	}
}

// --- TCP Proxy E2E ---

// duplexPipe 模擬 DataChannel 的雙向連線。
type duplexPipe struct {
	io.Reader
	io.Writer
}

func (d *duplexPipe) Close() error { return nil }

func TestProxy_RealTCP_Bidirectional(t *testing.T) {
	// 建立兩組 pipe 模擬 DataChannel
	remoteToProxyR, remoteToProxyW := io.Pipe()
	proxyToRemoteR, proxyToRemoteW := io.Pipe()

	channel := &duplexPipe{
		Reader: remoteToProxyR,
		Writer: proxyToRemoteW,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, err := proxy.New(0, channel)
	if err != nil {
		t.Fatal(err)
	}
	p.Start(ctx)
	defer p.Stop()

	// TCP 客戶端連線
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 方向 1：TCP → Channel（Client 寫入，Remote 讀取）
	clientData := []byte("hello from TCP client")
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 1024)
		n, _ := proxyToRemoteR.Read(buf)
		done <- buf[:n]
	}()

	conn.Write(clientData)
	received := <-done
	if string(received) != string(clientData) {
		t.Errorf("Channel 收到 %q，預期 %q", received, clientData)
	}

	// 方向 2：Channel → TCP（Remote 寫入，Client 讀取）
	remoteData := []byte("hello from remote device")
	go func() {
		remoteToProxyW.Write(remoteData)
	}()

	buf := make([]byte, 1024)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("TCP 讀取失敗: %v", err)
	}
	if string(buf[:n]) != string(remoteData) {
		t.Errorf("TCP 收到 %q，預期 %q", buf[:n], remoteData)
	}
}

func TestProxy_LargeTransfer(t *testing.T) {
	remoteToProxyR, _ := io.Pipe()
	proxyToRemoteR, proxyToRemoteW := io.Pipe()

	channel := &duplexPipe{
		Reader: remoteToProxyR,
		Writer: proxyToRemoteW,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, err := proxy.New(0, channel)
	if err != nil {
		t.Fatal(err)
	}
	p.Start(ctx)
	defer p.Stop()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 1MB 資料傳輸測試
	dataSize := 1024 * 1024
	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// 收集 remote 端收到的資料
	collected := make(chan []byte, 1)
	go func() {
		var buf []byte
		tmp := make([]byte, 32*1024)
		for {
			n, err := proxyToRemoteR.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if len(buf) >= dataSize || err != nil {
				collected <- buf
				return
			}
		}
	}()

	// TCP 寫入 1MB
	written := 0
	for written < dataSize {
		n, err := conn.Write(data[written:])
		if err != nil {
			t.Fatalf("寫入失敗 at %d: %v", written, err)
		}
		written += n
	}

	// 驗證
	select {
	case result := <-collected:
		if len(result) != dataSize {
			t.Errorf("收到 %d bytes，預期 %d", len(result), dataSize)
		}
		for i := 0; i < len(result) && i < dataSize; i++ {
			if result[i] != data[i] {
				t.Errorf("位元組 %d 不符：%d vs %d", i, result[i], data[i])
				break
			}
		}
		t.Logf("1MB 資料傳輸成功")
	case <-time.After(10 * time.Second):
		t.Fatal("1MB 傳輸逾時")
	}
}

// --- helpers ---

func connectAndAuth(t *testing.T, ctx context.Context, url string, role protocol.Role) (*ws.Conn, string) {
	t.Helper()

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	conn, _, err := ws.Dial(dialCtx, url, nil)
	if err != nil {
		t.Fatalf("WebSocket 連線失敗: %v", err)
	}

	// 發送 auth
	authEnv, _ := protocol.NewEnvelope(protocol.MsgTypeAuth, "test-host", "temp", "",
		protocol.AuthPayload{Token: "test-token", Role: role})
	data, _ := json.Marshal(authEnv)
	conn.Write(ctx, ws.MessageText, data)

	// 讀取 auth_ack
	_, ackData, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("讀取 auth_ack 失敗: %v", err)
	}

	var ack protocol.Envelope
	json.Unmarshal(ackData, &ack)

	var ackPayload protocol.AuthAckPayload
	ack.DecodePayload(&ackPayload)
	if !ackPayload.Success {
		t.Fatal("認證失敗")
	}

	return conn, ackPayload.AssignID
}

func sendMsg(t *testing.T, ctx context.Context, conn *ws.Conn, msgType protocol.MessageType, sourceID, targetID string, payload any) {
	t.Helper()

	env, err := protocol.NewEnvelope(msgType, "test-host", sourceID, targetID, payload)
	if err != nil {
		t.Fatalf("建立 Envelope 失敗: %v", err)
	}

	data, _ := json.Marshal(env)
	if err := conn.Write(ctx, ws.MessageText, data); err != nil {
		t.Fatalf("發送訊息失敗: %v", err)
	}
}

func readMsg(t *testing.T, ctx context.Context, conn *ws.Conn) protocol.Envelope {
	t.Helper()

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("讀取訊息失敗: %v", err)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("解析訊息失敗: %v", err)
	}
	return env
}
