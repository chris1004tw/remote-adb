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
