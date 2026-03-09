package signal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/chris1004tw/remote-adb/pkg/protocol"
	"github.com/coder/websocket"
)

// Server 是信令伺服器，處理 WebSocket 連線與訊息路由。
type Server struct {
	hub  *Hub
	auth Authenticator
}

// NewServer 建立一個新的信令伺服器。
func NewServer(hub *Hub, auth Authenticator) *Server {
	return &Server{hub: hub, auth: auth}
}

// Handler 回傳處理 WebSocket 升級的 HTTP handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return mux
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // 允許跨域連線
	})
	if err != nil {
		slog.Error("WebSocket 升級失敗", "error", err)
		return
	}

	ctx := r.Context()

	// 等待 auth 訊息（5 秒超時）
	authCtx, authCancel := context.WithTimeout(ctx, 5*time.Second)
	defer authCancel()

	_, data, err := ws.Read(authCtx)
	if err != nil {
		slog.Debug("讀取認證訊息失敗", "error", err)
		ws.Close(websocket.StatusPolicyViolation, "認證超時")
		return
	}

	var env protocol.Envelope
	if err := parseJSON(data, &env); err != nil {
		ws.Close(websocket.StatusPolicyViolation, "無效的 JSON 格式")
		return
	}

	if env.Type != protocol.MsgTypeAuth {
		ws.Close(websocket.StatusPolicyViolation, "第一筆訊息必須是 auth")
		return
	}

	var authPayload protocol.AuthPayload
	if err := env.DecodePayload(&authPayload); err != nil {
		ws.Close(websocket.StatusPolicyViolation, "無效的 auth payload")
		return
	}

	if !s.auth.Validate(authPayload.Token) {
		// 回傳認證失敗
		ack, _ := protocol.NewEnvelope(
			protocol.MsgTypeAuthAck,
			hostname(),
			"signal",
			env.SourceID,
			protocol.AuthAckPayload{Success: false, Reason: "認證失敗"},
		)
		writeJSON(ctx, ws, ack)
		ws.Close(websocket.StatusPolicyViolation, "認證失敗")
		return
	}

	// 認證成功，分配 ID
	connID := generateID(authPayload.Role)
	conn := NewConn(connID, authPayload.Role, env.Hostname, ws)

	// 回傳認證成功
	ack, _ := protocol.NewEnvelope(
		protocol.MsgTypeAuthAck,
		hostname(),
		"signal",
		connID,
		protocol.AuthAckPayload{Success: true, AssignID: connID},
	)
	if err := writeJSON(ctx, ws, ack); err != nil {
		slog.Debug("回傳 auth_ack 失敗", "error", err)
		ws.CloseNow()
		return
	}

	// 註冊到 hub
	s.hub.Register(conn)
	defer s.hub.Unregister(connID)

	// 啟動寫入 pump
	go conn.WritePump(ctx)

	// 讀取迴圈
	s.readLoop(ctx, conn)
}

func (s *Server) readLoop(ctx context.Context, conn *Conn) {
	for {
		env, err := conn.ReadMessage(ctx)
		if err != nil {
			slog.Debug("讀取訊息失敗", "conn_id", conn.ID(), "error", err)
			return
		}

		// 補上 source_id
		env.SourceID = conn.ID()

		switch env.Type {
		case protocol.MsgTypeRegister:
			s.handleRegister(conn, env)

		case protocol.MsgTypeDeviceUpdate:
			s.handleDeviceUpdate(conn, env)

		case protocol.MsgTypeHostList:
			s.handleHostList(conn)

		case protocol.MsgTypeLockReq, protocol.MsgTypeUnlockReq:
			// 轉發給目標 Agent
			if !s.hub.Route(env) {
				s.sendError(conn, protocol.ErrCodeTargetOffline, "目標主機離線")
			}

		case protocol.MsgTypeLockResp, protocol.MsgTypeUnlockResp:
			// 轉發給目標 Client
			if !s.hub.Route(env) {
				slog.Debug("轉發回應失敗，目標離線", "target_id", env.TargetID)
			}

		case protocol.MsgTypeOffer, protocol.MsgTypeAnswer, protocol.MsgTypeCandidate:
			// WebRTC 信令直接轉發
			if !s.hub.Route(env) {
				s.sendError(conn, protocol.ErrCodeTargetOffline, "信令轉發失敗：目標離線")
			}

		default:
			slog.Warn("未知的訊息類型", "type", env.Type, "conn_id", conn.ID())
		}
	}
}

func (s *Server) handleRegister(conn *Conn, env protocol.Envelope) {
	var payload protocol.RegisterPayload
	if err := env.DecodePayload(&payload); err != nil {
		slog.Error("解析 register payload 失敗", "error", err)
		return
	}

	info := protocol.HostInfo{
		HostID:   conn.ID(),
		Hostname: payload.Hostname,
		Devices:  payload.Devices,
	}
	s.hub.RegisterAgent(conn.ID(), info)
}

func (s *Server) handleDeviceUpdate(conn *Conn, env protocol.Envelope) {
	var payload protocol.DeviceUpdatePayload
	if err := env.DecodePayload(&payload); err != nil {
		slog.Error("解析 device_update payload 失敗", "error", err)
		return
	}

	s.hub.UpdateAgentDevices(conn.ID(), payload.Devices)

	// 廣播給所有 Client
	broadcast, _ := protocol.NewEnvelope(
		protocol.MsgTypeDeviceUpdate,
		hostname(),
		conn.ID(),
		"",
		protocol.DeviceUpdatePayload{
			HostID:  conn.ID(),
			Devices: payload.Devices,
		},
	)
	s.hub.BroadcastToClients(broadcast)
}

func (s *Server) handleHostList(conn *Conn) {
	agents := s.hub.Agents()
	resp, _ := protocol.NewEnvelope(
		protocol.MsgTypeHostListResp,
		hostname(),
		"signal",
		conn.ID(),
		protocol.HostListRespPayload{Hosts: agents},
	)
	conn.Send(resp)
}

func (s *Server) sendError(conn *Conn, code int, message string) {
	errMsg, _ := protocol.NewEnvelope(
		protocol.MsgTypeError,
		hostname(),
		"signal",
		conn.ID(),
		protocol.ErrorPayload{Code: code, Message: message},
	)
	conn.Send(errMsg)
}

// --- helpers ---

func generateID(role protocol.Role) string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%s-%s", role, hex.EncodeToString(b))
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func parseJSON(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func writeJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, data)
}
