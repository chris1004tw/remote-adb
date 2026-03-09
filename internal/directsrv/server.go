// Package directsrv 實作 Agent 端的 TCP 直連服務。
// 提供設備列表查詢和 ADB 轉發功能，不需要 Signal Server。
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
type Request struct {
	Action string `json:"action"`           // "list" 或 "connect"
	Serial string `json:"serial,omitempty"` // connect 時必填
	Token  string `json:"token,omitempty"`  // 可選的認證 token
}

// Response 是 Direct Server 回傳給 Client 的 JSON 回應。
type Response struct {
	OK       bool         `json:"ok"`
	Error    string       `json:"error,omitempty"`
	Hostname string       `json:"hostname,omitempty"`
	Devices  []DeviceInfo `json:"devices,omitempty"`
}

// DeviceInfo 是回應中的設備資訊。
type DeviceInfo struct {
	Serial   string `json:"serial"`
	State    string `json:"state"`
	Lock     string `json:"lock"`
	LockedBy string `json:"locked_by,omitempty"`
}

// Config 是 Direct Server 的設定。
type Config struct {
	DeviceTable *adb.DeviceTable
	DialDevice  func(serial string, port int) (net.Conn, error) // ADB 設備撥號函式
	Hostname    string
	Token       string // 空字串 = 不驗證
}

// Server 是 Agent 端的 TCP 直連服務。
type Server struct {
	cfg Config
}

// New 建立一個新的 Direct Server。
func New(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// Serve 在指定地址啟動 TCP server，並自動啟動 mDNS 廣播。
// 阻塞直到 ctx 取消。
func (s *Server) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("監聽 %s 失敗: %w", addr, err)
	}
	slog.Info("Direct Server 啟動", "addr", ln.Addr())

	// 啟動 mDNS 廣播（失敗不影響 TCP 服務）
	port := ln.Addr().(*net.TCPAddr).Port
	shutdown, mdnsErr := StartMDNS(s.cfg.Hostname, port)
	if mdnsErr != nil {
		slog.Warn("mDNS 廣播啟動失敗（非致命）", "error", mdnsErr)
	} else {
		defer shutdown()
	}

	return s.ServeListener(ctx, ln)
}

// ServeListener 使用已建立的 listener 啟動服務。供測試使用。
func (s *Server) ServeListener(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept 失敗: %w", err)
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	var req Request
	if err := json.NewDecoder(reader).Decode(&req); err != nil {
		s.writeResponse(conn, Response{OK: false, Error: "無效的請求格式"})
		return
	}

	// Token 驗證
	if s.cfg.Token != "" && req.Token != s.cfg.Token {
		s.writeResponse(conn, Response{OK: false, Error: "認證失敗"})
		return
	}

	switch req.Action {
	case "list":
		s.handleList(conn)
	case "connect":
		s.handleConnect(ctx, conn, req)
	default:
		s.writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("未知的 action: %s", req.Action)})
	}
}

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

func (s *Server) handleConnect(ctx context.Context, conn net.Conn, req Request) {
	if req.Serial == "" {
		s.writeResponse(conn, Response{OK: false, Error: "serial 為必填"})
		return
	}

	// 使用 remote addr 作為 client ID
	clientID := conn.RemoteAddr().String()

	// 鎖定設備
	if !s.cfg.DeviceTable.Lock(req.Serial, clientID) {
		s.writeResponse(conn, Response{OK: false, Error: "設備不可用或已被鎖定"})
		return
	}

	// 確保斷線時解鎖
	defer s.cfg.DeviceTable.Unlock(req.Serial, clientID)

	// 連線到 ADB 設備
	adbConn, err := s.cfg.DialDevice(req.Serial, 5555)
	if err != nil {
		s.writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("連線 ADB 設備失敗: %v", err)})
		return
	}
	defer adbConn.Close()

	// 回應成功
	s.writeResponse(conn, Response{OK: true})

	slog.Info("Direct 轉發開始", "serial", req.Serial, "client", clientID)

	// 雙向轉發
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(adbConn, conn)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(conn, adbConn)
		errc <- err
	}()

	select {
	case err := <-errc:
		if err != nil {
			slog.Debug("Direct 轉發結束", "serial", req.Serial, "error", err)
		}
	case <-ctx.Done():
	}

	slog.Info("Direct 轉發已停止", "serial", req.Serial, "client", clientID)
}

func (s *Server) writeResponse(conn net.Conn, resp Response) {
	json.NewEncoder(conn).Encode(resp)
}
