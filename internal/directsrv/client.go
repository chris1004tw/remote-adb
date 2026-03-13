package directsrv

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// DialService 連線到 Direct Server 並發送指定 action。
// 成功後回傳的 conn 已處於 raw bytes 模式，可直接用於 io.Copy 橋接。
//
// action 可為：
//   - "list" — 查詢設備清單（回傳 Response 後連線即關閉）
//   - "connect" — 連線到指定設備的 port 5555
//   - "connect-server" — 連線到 ADB server
//   - "connect-service" — 連線到指定設備的指定服務
//
// serial 僅在 "connect" / "connect-server" / "connect-service" 時使用，
// service 僅在 "connect-service" 時使用。
//
// 回傳值：
//   - io.ReadWriteCloser：底層 TCP 連線，呼叫端負責關閉
//   - error：連線失敗、請求編碼失敗、回應解碼失敗或伺服器回報錯誤時回傳
func DialService(addr, token, action, serial, service string) (io.ReadWriteCloser, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial direct server: %w", err)
	}

	req := Request{
		Action:  action,
		Serial:  serial,
		Service: service,
		Token:   token,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send request: %w", err)
	}

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("read response: %w", err)
	}
	conn.SetDeadline(time.Time{})

	if !resp.OK {
		conn.Close()
		return nil, fmt.Errorf("%s", resp.Error)
	}

	return conn, nil
}

// QueryDevices 查詢遠端 Agent 的設備清單。
// 封裝 TCP 連線 + JSON 編解碼 + OK 檢查，回傳完整 Response（含 Hostname、Devices）。
// 呼叫端依需求決定錯誤處理策略（os.Exit / return error / return nil）。
//
// 回傳值：
//   - *Response：查詢成功時回傳完整 Response
//   - error：連線、請求發送、回應讀取或伺服器端錯誤時回傳
func QueryDevices(addr, token string) (*Response, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if err := json.NewEncoder(conn).Encode(Request{Action: "list", Token: token}); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	if !resp.OK {
		return nil, fmt.Errorf("server: %s", resp.Error)
	}

	return &resp, nil
}
