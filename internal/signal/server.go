// Package signal 實作 WebRTC 信令伺服器（Signaling Server）。
//
// 信令伺服器的核心職責：
//   - 接受 Agent（遠端主機）與 Client（開發者本機）的 WebSocket 連線
//   - 透過 PSK（Pre-Shared Key）驗證連線端的合法性
//   - 為每條連線分配唯一 ID，並維護連線清冊（由 Hub 管理）
//   - 在 Agent 與 Client 之間路由信令訊息（設備清單、鎖定請求、WebRTC SDP/ICE 等）
//
// WebSocket 連線生命週期：
//  1. HTTP 升級為 WebSocket
//  2. 5 秒內完成 PSK 認證（防止惡意連線佔用資源）
//  3. 認證成功後分配 "{role}-{隨機hex}" 格式的 ID
//  4. 註冊至 Hub → 啟動 WritePump goroutine → 進入 readLoop 訊息路由
//  5. 連線斷開時自動從 Hub 移除，並清理對應的 Agent 設備資訊
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
	"sync"
	"time"

	"github.com/chris1004tw/remote-adb/pkg/protocol"
	"github.com/coder/websocket"
)

// Server 是信令伺服器，處理 WebSocket 連線與訊息路由。
// hub 負責連線管理與訊息派發，auth 負責驗證連線端的合法性。
type Server struct {
	hub  *Hub          // 中央路由器，管理所有活躍連線
	auth Authenticator // 認證策略（目前使用 PSK）
}

// NewServer 建立一個新的信令伺服器。
func NewServer(hub *Hub, auth Authenticator) *Server {
	return &Server{hub: hub, auth: auth}
}

// Handler 回傳處理 WebSocket 升級的 HTTP handler。
// 提供兩個端點：
//   - /ws     — WebSocket 信令通道（Agent/Client 連線入口）
//   - /health — 健康檢查端點，供負載平衡器或監控使用
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// handleWebSocket 處理單條 WebSocket 連線的完整生命週期。
//
// 流程：HTTP 升級 → 認證（5 秒超時）→ ID 分配 → 註冊 Hub → 訊息路由
// 任何階段失敗都會立即關閉連線，避免資源洩漏。
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Step 1: 將 HTTP 連線升級為 WebSocket
	// InsecureSkipVerify 允許跨域連線，因為 Agent/Client 可能來自不同網域
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // 允許跨域連線
	})
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}

	ctx := r.Context()

	// Step 2: 等待認證訊息，設定 5 秒超時
	// 超時理由：防止惡意連線長時間佔用資源而不發送認證訊息，
	// 正常的 Agent/Client 應在連線後立即發送 auth 訊息，5 秒足以涵蓋網路延遲。
	authCtx, authCancel := context.WithTimeout(ctx, 5*time.Second)
	defer authCancel()

	_, data, err := ws.Read(authCtx)
	if err != nil {
		slog.Debug("auth message read failed", "error", err)
		ws.Close(websocket.StatusPolicyViolation, "認證超時")
		return
	}

	// Step 3: 解析並驗證認證訊息
	// 連線後的第一筆訊息必須是 auth 類型，否則視為協定違規
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
		// 認證失敗：先回傳失敗原因讓對端知道，再關閉連線
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

	// Step 4: 認證成功，分配唯一 ID（格式："{role}-{16字元隨機hex}"）
	connID := generateID(authPayload.Role)
	conn := NewConn(connID, authPayload.Role, env.Hostname, ws)

	// 將分配的 ID 回傳給連線端，後續所有訊息都用此 ID 作為身分識別
	ack, _ := protocol.NewEnvelope(
		protocol.MsgTypeAuthAck,
		hostname(),
		"signal",
		connID,
		protocol.AuthAckPayload{Success: true, AssignID: connID},
	)
	if err := writeJSON(ctx, ws, ack); err != nil {
		slog.Debug("auth_ack write failed", "error", err)
		ws.CloseNow()
		return
	}

	// Step 5: 將連線註冊至 Hub，斷線時自動清理
	s.hub.Register(conn)
	defer s.hub.Unregister(connID)

	// Step 6: 啟動獨立的寫入 goroutine，負責從佇列取出訊息寫入 WebSocket
	go conn.WritePump(ctx)

	// Step 7: 進入讀取迴圈，阻塞至連線斷開
	s.readLoop(ctx, conn)
}

// readLoop 持續從連線讀取訊息並依類型分派處理。
// 此函式會阻塞直到連線斷開或讀取發生錯誤。
//
// 訊息路由邏輯分為三大類：
//  1. 狀態管理：register（Agent 註冊）、device_update（設備清單變更）、host_list（查詢主機列表）
//  2. 設備鎖定：lock_req/unlock_req 轉發給目標 Agent，lock_resp/unlock_resp 轉發回 Client
//  3. WebRTC 信令：offer/answer/candidate 點對點轉發，用於建立 P2P DataChannel
func (s *Server) readLoop(ctx context.Context, conn *Conn) {
	for {
		env, err := conn.ReadMessage(ctx)
		if err != nil {
			slog.Debug("message read failed", "conn_id", conn.ID(), "error", err)
			return
		}

		// 強制覆寫 source_id 為伺服器分配的 ID，防止客戶端偽造來源身分
		env.SourceID = conn.ID()

		switch env.Type {
		// --- 狀態管理類 ---
		case protocol.MsgTypeRegister:
			// Agent 上線後發送 register，攜帶主機名稱與初始設備列表
			s.handleRegister(conn, env)

		case protocol.MsgTypeDeviceUpdate:
			// Agent 偵測到 USB 設備插拔時，推送最新的設備列表
			s.handleDeviceUpdate(conn, env)

		case protocol.MsgTypeHostList:
			// Client 查詢目前所有在線 Agent 的設備清冊
			s.handleHostList(conn)

		// --- 設備鎖定類（Client ↔ Agent 雙向轉發）---
		case protocol.MsgTypeLockReq, protocol.MsgTypeUnlockReq:
			// Client 發起設備鎖定/解鎖請求，轉發給指定 Agent
			if !s.hub.Route(env) {
				s.sendError(conn, protocol.ErrCodeTargetOffline, "目標主機離線")
			}

		case protocol.MsgTypeLockResp, protocol.MsgTypeUnlockResp:
			// Agent 回應鎖定/解鎖結果，轉發回發起請求的 Client
			if !s.hub.Route(env) {
				slog.Debug("response forwarding failed, target offline", "target_id", env.TargetID)
			}

		// --- WebRTC 信令類（點對點透傳）---
		case protocol.MsgTypeOffer, protocol.MsgTypeAnswer, protocol.MsgTypeCandidate:
			// SDP offer/answer 與 ICE candidate 直接轉發，不做任何解析
			// 伺服器僅作為信令中繼，不介入 WebRTC 連線建立過程
			if !s.hub.Route(env) {
				s.sendError(conn, protocol.ErrCodeTargetOffline, "信令轉發失敗：目標離線")
			}

		default:
			slog.Warn("unknown message type", "type", env.Type, "conn_id", conn.ID())
		}
	}
}

// handleRegister 處理 Agent 的 register 訊息。
// Agent 連線後會發送此訊息，攜帶主機名稱與目前掛載的 Android 設備清單。
// 處理完成後會主動廣播更新後的主機列表給所有在線 Client，
// 讓 Client 端的設備列表即時反映最新狀態。
func (s *Server) handleRegister(conn *Conn, env protocol.Envelope) {
	var payload protocol.RegisterPayload
	if err := env.DecodePayload(&payload); err != nil {
		slog.Error("register payload decode failed", "error", err)
		return
	}

	info := protocol.HostInfo{
		HostID:   conn.ID(),
		Hostname: payload.Hostname,
		Devices:  payload.Devices,
	}
	s.hub.RegisterAgent(conn.ID(), info)

	// Agent 註冊後立即廣播完整主機列表給所有 Client，確保 Client 端即時更新
	agents := s.hub.Agents()
	broadcast, _ := protocol.NewEnvelope(
		protocol.MsgTypeHostListResp,
		hostname(),
		"signal",
		"", // TargetID 為空表示廣播
		protocol.HostListRespPayload{Hosts: agents},
	)
	s.hub.BroadcastToClients(broadcast)
}

// handleDeviceUpdate 處理 Agent 的 device_update 訊息。
// 當 Agent 偵測到 USB 設備插入或拔除時，會發送此訊息通知伺服器。
// 伺服器更新內部設備清冊後，將變更廣播給所有 Client。
func (s *Server) handleDeviceUpdate(conn *Conn, env protocol.Envelope) {
	var payload protocol.DeviceUpdatePayload
	if err := env.DecodePayload(&payload); err != nil {
		slog.Error("device_update payload decode failed", "error", err)
		return
	}

	// 先更新 Hub 中的 Agent 設備清冊
	s.hub.UpdateAgentDevices(conn.ID(), payload.Devices)

	// 再將設備變更廣播給所有 Client
	broadcast, _ := protocol.NewEnvelope(
		protocol.MsgTypeDeviceUpdate,
		hostname(),
		conn.ID(),
		"", // TargetID 為空表示廣播
		protocol.DeviceUpdatePayload{
			HostID:  conn.ID(),
			Devices: payload.Devices,
		},
	)
	s.hub.BroadcastToClients(broadcast)
}

// handleHostList 處理 Client 的 host_list 查詢。
// 回傳所有在線 Agent 的主機資訊與設備清單，僅回覆給發起查詢的 Client。
func (s *Server) handleHostList(conn *Conn) {
	agents := s.hub.Agents()
	resp, _ := protocol.NewEnvelope(
		protocol.MsgTypeHostListResp,
		hostname(),
		"signal",
		conn.ID(), // 僅回覆給查詢者
		protocol.HostListRespPayload{Hosts: agents},
	)
	conn.Send(resp)
}

// sendError 將錯誤訊息回傳給指定的連線端。
// 用於通知 Client 路由失敗（例如目標 Agent 離線）等狀況。
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

// --- helpers（內部輔助函式）---

// generateID 為連線端產生唯一識別碼。
// 格式為 "{role}-{16字元隨機hex}"，例如 "agent-a1b2c3d4e5f67890"。
// 使用 crypto/rand 確保隨機性，避免 ID 碰撞。
func generateID(role protocol.Role) string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%s-%s", role, hex.EncodeToString(b))
}

// hostname 取得本機主機名稱，用於填入訊息的 Hostname 欄位。
// 使用 sync.OnceValue 快取結果，避免每次呼叫都執行系統呼叫。
var hostname = sync.OnceValue(func() string {
	h, _ := os.Hostname()
	return h
})

// parseJSON 將原始位元組解析為指定的結構體。
func parseJSON(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// writeJSON 將結構體序列化為 JSON 並透過 WebSocket 發送。
// 僅用於認證階段尚未建立 Conn 封裝前的直接寫入。
func writeJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, data)
}
