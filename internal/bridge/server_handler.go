// server_handler.go 實作被控端的 DataChannel 連線處理。
//
// 被控端（伺服器端）收到客戶端建立的 DataChannel 後，根據 label 前綴
// 分派到對應的處理函式。支援三種 DataChannel 類型：
//   - adb-server/{id}：轉發到本機 ADB server（ADB 協定命令）
//   - adb-stream/{id}/{serial}/{service}：設備 transport 串流
//   - adb-fwd/{id}/{serial}/{remoteSpec}：forward 連線到設備服務
//
// 設計原則：不依賴任何 GUI 框架，透過 ADBAddr 欄位指定本機 ADB server 位址。
package bridge

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"

	"github.com/chris1004tw/remote-adb/internal/adb"
)

// ServerHandler 處理被控端收到的 DataChannel 連線。
// 根據 DataChannel label 前綴分派到對應的處理函式。
type ServerHandler struct {
	ADBAddr string // 本機 ADB server 位址（如 "127.0.0.1:5037"）
}

// HandleChannel 根據 label 前綴分派 DataChannel 到對應處理函式。
// 回傳 true 表示已處理（或已啟動 goroutine 處理），false 表示未知 label。
//
// label 前綴對應：
//   - "adb-server/{id}" → HandleADBServerConn
//   - "adb-stream/{id}/{serial}/{service}" → HandleADBStreamConn
//   - "adb-fwd/{id}/{serial}/{remoteSpec}" → HandleADBForwardConn
func (h *ServerHandler) HandleChannel(ctx context.Context, label string, rwc io.ReadWriteCloser) bool {
	parts := strings.SplitN(label, "/", 4)
	if len(parts) < 2 {
		return false
	}
	switch parts[0] {
	case "adb-server":
		go h.HandleADBServerConn(ctx, rwc)
		return true
	case "adb-stream":
		if len(parts) < 4 {
			rwc.Close()
			return true
		}
		go h.HandleADBStreamConn(ctx, rwc, parts[2], parts[3])
		return true
	case "adb-fwd":
		if len(parts) < 4 {
			rwc.Close()
			return true
		}
		go HandleADBForwardConn(ctx, rwc, h.ADBAddr, parts[2], parts[3])
		return true
	}
	return false
}

// HandleADBServerConn 將客戶端的 DataChannel 轉發到本機 ADB server。
// 建立到 ADBAddr 的 TCP 連線後，雙向橋接 DataChannel 與 ADB server。
//
// 參數：
//   - ctx：用於取消雙向複製
//   - rwc：DataChannel 的 ReadWriteCloser
func (h *ServerHandler) HandleADBServerConn(ctx context.Context, rwc io.ReadWriteCloser) {
	defer rwc.Close()

	conn, err := net.Dial("tcp", h.ADBAddr)
	if err != nil {
		slog.Debug("failed to connect local ADB server", "error", err)
		return
	}
	defer conn.Close()

	BiCopy(ctx, rwc, conn)
}

// HandleADBStreamConn 處理 device transport 的單一串流（被控端）。
// 收到客戶端建立的 adb-stream DataChannel 後：
//  1. 連線本機 ADB server
//  2. 發送 host:transport:<serial> 切換到目標設備
//  3. 發送 service 命令（如 shell:ls、sync: 等）
//  4. 通知客戶端就緒（寫入 1 byte: 1=成功, 0=失敗）
//  5. 雙向橋接 DataChannel ↔ ADB server（BiCopy）
//
// 參數：
//   - ctx：用於取消雙向複製
//   - rwc：DataChannel 的 ReadWriteCloser
//   - serial：目標設備序號
//   - service：要連線的服務（如 "shell:ls"、"localabstract:scrcpy"）
func (h *ServerHandler) HandleADBStreamConn(ctx context.Context, rwc io.ReadWriteCloser, serial, service string) {
	defer rwc.Close()

	slog.Debug("stream: start handling", "serial", serial, "service", service)

	conn, err := adb.NewDialer(h.ADBAddr).DialServiceWithRetry(ctx, serial, service)
	if err != nil {
		slog.Debug("stream: DialService failed", "serial", serial, "service", service, "error", err)
		rwc.Write([]byte{0})
		return
	}
	defer conn.Close()

	// 通知客戶端就緒
	if n, err := rwc.Write([]byte{1}); err != nil {
		slog.Debug("stream: failed to write ready signal", "serial", serial, "service", service, "error", err, "n", n)
		return
	}

	slog.Debug("stream established", "serial", serial, "service", service)

	// 雙向轉發（BiCopy 結束時關閉雙方，避免死鎖）
	BiCopy(ctx, rwc, conn)
}
