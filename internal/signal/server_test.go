package signal_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chris1004tw/remote-adb/internal/signal"
	"github.com/chris1004tw/remote-adb/pkg/protocol"
	"github.com/coder/websocket"
)

// 建立測試用的 signal server 並回傳 WebSocket URL。
func setupTestServer(t *testing.T) string {
	t.Helper()
	hub := signal.NewHub()
	auth := signal.NewPSKAuth("test-token")
	srv := signal.NewServer(hub, auth)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return "ws" + ts.URL[len("http"):] + "/ws"
}

// dialAndAuth 連線並完成認證，回傳已認證的 WebSocket 連線和分配的 ID。
func dialAndAuth(t *testing.T, wsURL string, role protocol.Role) (*websocket.Conn, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket 連線失敗: %v", err)
	}

	// 發送 auth
	authEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeAuth, "test-host", "temp", "",
		protocol.AuthPayload{Token: "test-token", Role: role},
	)
	sendMsg(t, ctx, ws, authEnv)

	// 接收 auth_ack
	ack := recvMsg(t, ctx, ws)
	if ack.Type != protocol.MsgTypeAuthAck {
		t.Fatalf("預期 auth_ack，收到 %q", ack.Type)
	}

	var ackPayload protocol.AuthAckPayload
	if err := ack.DecodePayload(&ackPayload); err != nil {
		t.Fatalf("解析 auth_ack 失敗: %v", err)
	}
	if !ackPayload.Success {
		t.Fatalf("認證應成功，但收到失敗: %s", ackPayload.Reason)
	}

	return ws, ackPayload.AssignID
}

func sendMsg(t *testing.T, ctx context.Context, ws *websocket.Conn, env protocol.Envelope) {
	t.Helper()
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("序列化失敗: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("寫入 WebSocket 失敗: %v", err)
	}
}

func recvMsg(t *testing.T, ctx context.Context, ws *websocket.Conn) protocol.Envelope {
	t.Helper()
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("讀取 WebSocket 失敗: %v", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("反序列化失敗: %v", err)
	}
	return env
}

func TestServer_AuthSuccess(t *testing.T) {
	wsURL := setupTestServer(t)
	ws, assignID := dialAndAuth(t, wsURL, protocol.RoleAgent)
	defer ws.CloseNow()

	if assignID == "" {
		t.Error("分配的 ID 不應為空")
	}
}

func TestServer_AuthFailure_WrongToken(t *testing.T) {
	wsURL := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket 連線失敗: %v", err)
	}
	defer ws.CloseNow()

	// 發送錯誤 token
	authEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeAuth, "test-host", "temp", "",
		protocol.AuthPayload{Token: "wrong-token", Role: protocol.RoleAgent},
	)
	sendMsg(t, ctx, ws, authEnv)

	// 應收到失敗的 auth_ack
	ack := recvMsg(t, ctx, ws)
	var ackPayload protocol.AuthAckPayload
	if err := ack.DecodePayload(&ackPayload); err != nil {
		t.Fatalf("解析 auth_ack 失敗: %v", err)
	}
	if ackPayload.Success {
		t.Error("錯誤 token 不應認證成功")
	}
}

func TestServer_HostList(t *testing.T) {
	wsURL := setupTestServer(t)

	// Agent 連線並註冊
	agentWS, agentID := dialAndAuth(t, wsURL, protocol.RoleAgent)
	defer agentWS.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Agent 發送 register
	regEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeRegister, "lab-pc", agentID, "",
		protocol.RegisterPayload{
			HostID:   agentID,
			Hostname: "lab-pc-01",
			Devices: []protocol.DeviceInfo{
				{Serial: "DEV001", State: protocol.DeviceStateOnline, Lock: protocol.LockAvailable},
			},
		},
	)
	sendMsg(t, ctx, agentWS, regEnv)

	// 短暫等待 server 處理
	time.Sleep(100 * time.Millisecond)

	// Client 連線
	clientWS, clientID := dialAndAuth(t, wsURL, protocol.RoleClient)
	defer clientWS.CloseNow()

	// Client 請求 host_list
	listEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeHostList, "dev-pc", clientID, "",
		nil,
	)
	sendMsg(t, ctx, clientWS, listEnv)

	// 接收 host_list_resp
	resp := recvMsg(t, ctx, clientWS)
	if resp.Type != protocol.MsgTypeHostListResp {
		t.Fatalf("預期 host_list_resp，收到 %q", resp.Type)
	}

	var payload protocol.HostListRespPayload
	if err := resp.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 host_list_resp 失敗: %v", err)
	}

	if len(payload.Hosts) != 1 {
		t.Fatalf("Hosts 數量 = %d, 預期 1", len(payload.Hosts))
	}
	if payload.Hosts[0].Hostname != "lab-pc-01" {
		t.Errorf("Hostname = %q, 預期 %q", payload.Hosts[0].Hostname, "lab-pc-01")
	}
	if len(payload.Hosts[0].Devices) != 1 {
		t.Errorf("Devices 數量 = %d, 預期 1", len(payload.Hosts[0].Devices))
	}
}

func TestServer_SDPForward(t *testing.T) {
	wsURL := setupTestServer(t)

	// Agent 連線
	agentWS, agentID := dialAndAuth(t, wsURL, protocol.RoleAgent)
	defer agentWS.CloseNow()

	// Client 連線
	clientWS, clientID := dialAndAuth(t, wsURL, protocol.RoleClient)
	defer clientWS.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Client 發送 offer 給 Agent
	offerEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeOffer, "dev-pc", clientID, agentID,
		protocol.SDPPayload{SDP: "v=0\r\noffer-sdp", Type: "offer"},
	)
	sendMsg(t, ctx, clientWS, offerEnv)

	// Agent 應收到 offer
	received := recvMsg(t, ctx, agentWS)
	if received.Type != protocol.MsgTypeOffer {
		t.Fatalf("Agent 預期收到 offer，收到 %q", received.Type)
	}

	var sdp protocol.SDPPayload
	if err := received.DecodePayload(&sdp); err != nil {
		t.Fatalf("解析 SDP 失敗: %v", err)
	}
	if sdp.Type != "offer" {
		t.Errorf("SDP Type = %q, 預期 %q", sdp.Type, "offer")
	}

	// Agent 回傳 answer
	answerEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeAnswer, "lab-pc", agentID, clientID,
		protocol.SDPPayload{SDP: "v=0\r\nanswer-sdp", Type: "answer"},
	)
	sendMsg(t, ctx, agentWS, answerEnv)

	// Client 應收到 answer
	received2 := recvMsg(t, ctx, clientWS)
	if received2.Type != protocol.MsgTypeAnswer {
		t.Fatalf("Client 預期收到 answer，收到 %q", received2.Type)
	}
}

func TestServer_ICECandidateForward(t *testing.T) {
	wsURL := setupTestServer(t)

	agentWS, agentID := dialAndAuth(t, wsURL, protocol.RoleAgent)
	defer agentWS.CloseNow()

	clientWS, clientID := dialAndAuth(t, wsURL, protocol.RoleClient)
	defer clientWS.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Client 發送 ICE candidate
	candEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeCandidate, "dev-pc", clientID, agentID,
		protocol.CandidatePayload{
			Candidate:     "candidate:1 1 udp 2130706431 192.168.1.1 50000 typ host",
			SDPMid:        "0",
			SDPMLineIndex: 0,
		},
	)
	sendMsg(t, ctx, clientWS, candEnv)

	// Agent 應收到 candidate
	received := recvMsg(t, ctx, agentWS)
	if received.Type != protocol.MsgTypeCandidate {
		t.Fatalf("Agent 預期收到 candidate，收到 %q", received.Type)
	}

	var cand protocol.CandidatePayload
	if err := received.DecodePayload(&cand); err != nil {
		t.Fatalf("解析 candidate 失敗: %v", err)
	}
	if cand.SDPMid != "0" {
		t.Errorf("SDPMid = %q, 預期 %q", cand.SDPMid, "0")
	}
}

func TestServer_AgentDisconnect_RemovedFromHostList(t *testing.T) {
	wsURL := setupTestServer(t)

	// Agent 連線並註冊
	agentWS, agentID := dialAndAuth(t, wsURL, protocol.RoleAgent)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	regEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeRegister, "lab-pc", agentID, "",
		protocol.RegisterPayload{
			HostID:   agentID,
			Hostname: "lab-pc-01",
			Devices:  []protocol.DeviceInfo{},
		},
	)
	sendMsg(t, ctx, agentWS, regEnv)
	time.Sleep(100 * time.Millisecond)

	// Agent 斷線
	agentWS.Close(websocket.StatusNormalClosure, "bye")
	time.Sleep(200 * time.Millisecond)

	// Client 連線並查詢
	clientWS, clientID := dialAndAuth(t, wsURL, protocol.RoleClient)
	defer clientWS.CloseNow()

	listEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeHostList, "dev-pc", clientID, "",
		nil,
	)
	sendMsg(t, ctx, clientWS, listEnv)

	resp := recvMsg(t, ctx, clientWS)
	var payload protocol.HostListRespPayload
	if err := resp.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 host_list_resp 失敗: %v", err)
	}

	if len(payload.Hosts) != 0 {
		t.Errorf("Agent 斷線後 Hosts 數量 = %d, 預期 0", len(payload.Hosts))
	}
}

func TestServer_RegisterBroadcastsHostList(t *testing.T) {
	wsURL := setupTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Client 先連線
	clientWS, _ := dialAndAuth(t, wsURL, protocol.RoleClient)
	defer clientWS.CloseNow()

	// 2. Agent 後連線並註冊
	agentWS, agentID := dialAndAuth(t, wsURL, protocol.RoleAgent)
	defer agentWS.CloseNow()

	regEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeRegister, "lab-pc", agentID, "",
		protocol.RegisterPayload{
			HostID:   agentID,
			Hostname: "new-agent",
			Devices: []protocol.DeviceInfo{
				{Serial: "DEV001", State: protocol.DeviceStateOnline, Lock: protocol.LockAvailable},
			},
		},
	)
	sendMsg(t, ctx, agentWS, regEnv)

	// 3. Client 應自動收到 host_list_resp（不需主動查詢）
	resp := recvMsg(t, ctx, clientWS)
	if resp.Type != protocol.MsgTypeHostListResp {
		t.Fatalf("預期 host_list_resp，收到 %q", resp.Type)
	}

	var payload protocol.HostListRespPayload
	if err := resp.DecodePayload(&payload); err != nil {
		t.Fatalf("解析失敗: %v", err)
	}
	if len(payload.Hosts) != 1 {
		t.Fatalf("主機數量 = %d, 預期 1", len(payload.Hosts))
	}
	if payload.Hosts[0].Hostname != "new-agent" {
		t.Errorf("Hostname = %q, 預期 %q", payload.Hosts[0].Hostname, "new-agent")
	}
}

func TestServer_RouteToOfflineTarget_ReturnsError(t *testing.T) {
	wsURL := setupTestServer(t)

	clientWS, clientID := dialAndAuth(t, wsURL, protocol.RoleClient)
	defer clientWS.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 嘗試發送 offer 給不存在的 Agent
	offerEnv, _ := protocol.NewEnvelope(
		protocol.MsgTypeOffer, "dev-pc", clientID, "nonexistent-agent",
		protocol.SDPPayload{SDP: "v=0\r\n...", Type: "offer"},
	)
	sendMsg(t, ctx, clientWS, offerEnv)

	// 應收到錯誤
	resp := recvMsg(t, ctx, clientWS)
	if resp.Type != protocol.MsgTypeError {
		t.Fatalf("預期 error，收到 %q", resp.Type)
	}

	var errPayload protocol.ErrorPayload
	if err := resp.DecodePayload(&errPayload); err != nil {
		t.Fatalf("解析 error 失敗: %v", err)
	}
	if errPayload.Code != protocol.ErrCodeTargetOffline {
		t.Errorf("Code = %d, 預期 %d", errPayload.Code, protocol.ErrCodeTargetOffline)
	}
}
