// Package directsrv 實作 Agent 端的 TCP 直連服務（Direct 模式）。
//
// # Direct 模式概述
//
// 在區域網路（LAN）環境中，Client 與 Agent 不需透過 Signal Server 中繼信令，
// 可直接透過 TCP 連線進行設備查詢與 ADB 轉發。典型流程如下：
//
//  1. Agent 啟動 Direct Server，監聽指定 TCP 埠，並透過 mDNS 廣播自身存在
//  2. Client 透過 mDNS 發現或手動指定 Agent 地址後，建立 TCP 連線
//  3. Client 發送 JSON 格式的 Request（action: "list" 或 "connect"）
//  4. Server 回傳 JSON 格式的 Response；若為 connect 則進入雙向位元組轉發
//
// # TCP JSON 協定
//
// 每條 TCP 連線採用單次 Request/Response 模式：
//   - Client 送出一筆 JSON Request（含 action、serial、token 欄位）
//   - Server 回傳一筆 JSON Response（含 ok、error、devices 等欄位）
//   - 若 action 為 "connect" 且成功，Response 送出後同一條 TCP 連線會直接
//     轉為 raw bytes 雙向轉發（ADB 協定資料流），直到任一端斷線
package directsrv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/chris1004tw/remote-adb/internal/adb"
)

// Request 是 Client 發送給 Direct Server 的 JSON 請求。
// 每條 TCP 連線只發送一筆 Request，Server 處理完畢後回傳 Response。
type Request struct {
	Action  string `json:"action"`            // "list" / "connect" / "connect-server" / "connect-service"
	Serial  string `json:"serial,omitempty"`  // connect / connect-service 時必填
	Service string `json:"service,omitempty"` // connect-service 時必填（ADB service 字串）
	Token   string `json:"token,omitempty"`   // 可選的認證 token
}

// Response 是 Direct Server 回傳給 Client 的 JSON 回應。
// 若 OK 為 true 且 action 為 "connect"，此 Response 之後的資料流即為 ADB raw bytes。
type Response struct {
	OK       bool         `json:"ok"`                // 操作是否成功
	Error    string       `json:"error,omitempty"`   // 失敗時的錯誤訊息
	Hostname string       `json:"hostname,omitempty"` // Agent 主機名稱（list 回應時填入）
	Devices  []DeviceInfo `json:"devices,omitempty"` // 設備清單（list 回應時填入）
}

// DeviceInfo 是回應中的設備資訊，描述單一 Android 設備的連線與鎖定狀態。
type DeviceInfo struct {
	Serial   string `json:"serial"`              // 設備序號（如 "emulator-5554"）
	State    string `json:"state"`               // 硬體狀態："device"（在線）或 "offline"
	Lock     string `json:"lock"`                // 鎖定狀態："available" 或 "locked"
	LockedBy string `json:"locked_by,omitempty"` // 鎖定者 ID（僅在 locked 時有值）
}

// Config 是 Direct Server 的設定，由外部（通常是 cmd/radb）注入依賴。
type Config struct {
	DeviceTable *adb.DeviceTable                                // 設備狀態表（含鎖定管理），由 ADB track-devices 維護
	DialDevice  func(serial string, port int) (net.Conn, error) // ADB 設備撥號函式，用於建立與 Android 設備的 TCP 連線
	Hostname    string                                          // Agent 的主機名稱，回傳給 Client 用於顯示辨識
	Token       string                                          // 共享密鑰；空字串表示不啟用認證
	ADBAddr     string                                          // ADB server 地址（預設 127.0.0.1:5037），connect-server/connect-service 使用
}

// Server 是 Agent 端的 TCP 直連服務，負責接受 Client 連線、處理 JSON 請求、
// 並在 connect 時橋接 Client 與 ADB 設備之間的資料流。
type Server struct {
	cfg Config
}

// New 建立一個新的 Direct Server。
func New(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// Serve 在指定地址啟動 TCP server，並自動啟動 mDNS 廣播。
// 阻塞直到 ctx 取消。mDNS 廣播失敗為非致命錯誤，不影響 TCP 服務本身。
func (s *Server) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("監聽 %s 失敗: %w", addr, err)
	}
	slog.Info("Direct Server 啟動", "addr", ln.Addr())

	// 啟動 mDNS 廣播（失敗不影響 TCP 服務）
	port := ln.Addr().(*net.TCPAddr).Port
	shutdown, mdnsErr := StartMDNS(s.cfg.Hostname, port, s.cfg.Token)
	if mdnsErr != nil {
		slog.Warn("mDNS 廣播啟動失敗（非致命）", "error", mdnsErr)
	} else {
		defer shutdown()
	}

	return s.ServeListener(ctx, ln)
}

// ServeListener 使用已建立的 listener 啟動服務。
// 將 listener 建立與服務迴圈分離，方便測試時注入 in-memory listener。
func (s *Server) ServeListener(ctx context.Context, ln net.Listener) error {
	// 當 context 被取消時關閉 listener，使 Accept() 跳出阻塞
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// context 已取消時視為正常退出
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept 失敗: %w", err)
		}
		// 每個 Client 連線在獨立 goroutine 中處理，支援並行存取
		go s.handleConn(ctx, conn)
	}
}

// handleConn 處理單一 Client TCP 連線的完整生命週期。
// 流程：讀取 JSON Request → Token 驗證 → 依 action 分派到 handleList 或 handleConnect。
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// 解析 Client 送來的 JSON Request（每條連線只有一筆）
	reader := bufio.NewReader(conn)
	var req Request
	if err := json.NewDecoder(reader).Decode(&req); err != nil {
		s.writeResponse(conn, Response{OK: false, Error: "無效的請求格式"})
		return
	}

	// Token 驗證：若 Server 有設定 token，則 Client 必須提供相同值
	if s.cfg.Token != "" && req.Token != s.cfg.Token {
		s.writeResponse(conn, Response{OK: false, Error: "認證失敗"})
		return
	}

	switch req.Action {
	case "list":
		s.handleList(conn)
	case "connect":
		s.handleConnect(ctx, conn, req)
	case "connect-server":
		s.handleConnectServer(ctx, conn)
	case "connect-service":
		s.handleConnectService(ctx, conn, req)
	default:
		s.writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("未知的 action: %s", req.Action)})
	}
}

// handleList 回傳目前 Agent 上所有 Android 設備的狀態清單。
// 資料來源為 DeviceTable，由 ADB track-devices 持續更新。
func (s *Server) handleList(conn net.Conn) {
	devices := s.cfg.DeviceTable.List()
	infos := make([]DeviceInfo, len(devices))
	for i, d := range devices {
		infos[i] = DeviceInfo{
			Serial:   d.Serial,
			State:    d.State,
			Lock:     d.Lock,
			LockedBy: d.LockedBy,
		}
	}
	s.writeResponse(conn, Response{OK: true, Hostname: s.cfg.Hostname, Devices: infos})
}

// handleConnect 處理 ADB 設備連線請求，完整流程：
//
//  1. 驗證 serial 參數
//  2. 以 clientID 鎖定設備（互斥，同一設備同時只允許一位使用者）
//  3. 撥號連線到 ADB 設備（預設 port 5555）
//  4. 回傳成功 Response
//  5. 進入雙向位元組轉發（Client ↔ ADB 設備），直到任一端斷線或 ctx 取消
//  6. 函式返回時透過 defer 自動解鎖設備
//
// clientID 的設計選擇：使用 conn.RemoteAddr()（IP:Port）作為唯一識別。
// 在 Direct 模式下，Client 不需要登入或註冊，因此無法取得穩定的使用者 ID。
// RemoteAddr 在同一條 TCP 連線的生命週期內是唯一的，足以作為鎖定的 owner 標記。
// 這與 Signal Server 模式中使用 WebSocket session ID 作為 clientID 的概念對應。
func (s *Server) handleConnect(ctx context.Context, conn net.Conn, req Request) {
	if req.Serial == "" {
		s.writeResponse(conn, Response{OK: false, Error: "serial 為必填"})
		return
	}

	// 使用 conn.RemoteAddr()（如 "192.168.1.5:49832"）作為 client ID，
	// 無需額外的身份識別機制，連線斷開後自然不再佔用
	clientID := conn.RemoteAddr().String()

	// 步驟 1：嘗試鎖定設備，防止多人同時操作同一台 Android 設備
	if !s.cfg.DeviceTable.Lock(req.Serial, clientID) {
		s.writeResponse(conn, Response{OK: false, Error: "設備不可用或已被鎖定"})
		return
	}

	// 步驟 2：defer 確保無論後續流程成功或失敗，函式返回時都會解鎖設備
	defer s.cfg.DeviceTable.Unlock(req.Serial, clientID)

	// 步驟 3：撥號連線到實際的 ADB 設備
	adbConn, err := s.cfg.DialDevice(req.Serial, 5555)
	if err != nil {
		s.writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("連線 ADB 設備失敗: %v", err)})
		return
	}
	defer adbConn.Close()

	// 步驟 4：回傳成功，此後同一條 TCP 連線將轉為 raw bytes 轉發
	s.writeResponse(conn, Response{OK: true})

	slog.Info("Direct 轉發開始", "serial", req.Serial, "client", clientID)

	// 步驟 5：雙向轉發 — 兩個 goroutine 分別處理上行與下行方向
	// 使用 buffered channel（容量 2）確保兩個 goroutine 都能寫入而不阻塞
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(adbConn, conn) // Client → ADB 設備（上行）
		errc <- err
	}()
	go func() {
		_, err := io.Copy(conn, adbConn) // ADB 設備 → Client（下行）
		errc <- err
	}()

	// 等待任一方向結束或 context 取消
	select {
	case err := <-errc:
		if err != nil {
			slog.Debug("Direct 轉發結束", "serial", req.Serial, "error", err)
		}
	case <-ctx.Done():
	}

	slog.Info("Direct 轉發已停止", "serial", req.Serial, "client", clientID)
	// 函式返回時，defer 會依序：解鎖設備 → 關閉 ADB 連線 → 關閉 Client 連線
}

// handleConnectServer 橋接 Client TCP 連線到本機 ADB server。
// 回傳成功 Response 後，TCP 連線轉為 ADB server 原生協定的 raw bytes 轉發。
// Client 可直接發送任何 ADB server 命令（如 host:devices、host:transport:<serial> 等）。
func (s *Server) handleConnectServer(ctx context.Context, conn net.Conn) {
	adbAddr := s.cfg.ADBAddr
	if adbAddr == "" {
		adbAddr = "127.0.0.1:5037"
	}

	adbConn, err := net.Dial("tcp", adbAddr)
	if err != nil {
		s.writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("連線 ADB server 失敗: %v", err)})
		return
	}
	defer adbConn.Close()

	s.writeResponse(conn, Response{OK: true})

	slog.Debug("connect-server 轉發開始", "adbAddr", adbAddr, "client", conn.RemoteAddr())

	errc := make(chan error, 2)
	go func() { _, err := io.Copy(adbConn, conn); errc <- err }()
	go func() { _, err := io.Copy(conn, adbConn); errc <- err }()

	select {
	case <-errc:
	case <-ctx.Done():
	}
}

// handleConnectService 橋接 Client TCP 連線到指定設備的指定服務。
// 流程：連線 ADB server → host:transport:<serial> → service → 回傳成功 → 雙向轉發。
// 用於 deviceBridge 的 OPEN 命令，每個 ADB transport stream 對應一條 TCP 連線。
func (s *Server) handleConnectService(ctx context.Context, conn net.Conn, req Request) {
	if req.Serial == "" {
		s.writeResponse(conn, Response{OK: false, Error: "serial 為必填"})
		return
	}
	if req.Service == "" {
		s.writeResponse(conn, Response{OK: false, Error: "service 為必填"})
		return
	}

	adbAddr := s.cfg.ADBAddr
	if adbAddr == "" {
		adbAddr = "127.0.0.1:5037"
	}

	dialer := adb.NewDialer(adbAddr)
	adbConn, err := dialer.DialService(req.Serial, req.Service)
	if err != nil {
		s.writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("連線服務失敗: %v", err)})
		return
	}
	defer adbConn.Close()

	s.writeResponse(conn, Response{OK: true})

	slog.Debug("connect-service 轉發開始", "serial", req.Serial, "service", req.Service, "client", conn.RemoteAddr())

	errc := make(chan error, 2)
	go func() { _, err := io.Copy(adbConn, conn); errc <- err }()
	go func() { _, err := io.Copy(conn, adbConn); errc <- err }()

	select {
	case <-errc:
	case <-ctx.Done():
	}
}

// writeResponse 將 JSON 回應寫入 TCP 連線。
// 使用 json.Encoder 自動在尾部加上換行符，作為 JSON 訊息的分隔。
func (s *Server) writeResponse(conn net.Conn, resp Response) {
	json.NewEncoder(conn).Encode(resp)
}
