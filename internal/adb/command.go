// command.go 提供 ADB wire protocol 的命令發送與狀態讀取函式。
//
// ADB protocol 使用簡單的文字格式：
//   - 發送：4 位 hex 長度前綴 + ASCII 命令字串（如 "000chost:version"）
//   - 回應：4 byte 狀態碼 "OKAY" 或 "FAIL"（FAIL 後接 4 位 hex 長度 + 錯誤訊息）
//
// 這些函式由 adb 套件內部（Dialer、Tracker）和外部套件（bridge）共用，
// 避免重複實作 ADB wire format 編解碼邏輯。
package adb

import (
	"fmt"
	"io"
)

// SendCommand 發送 ADB protocol 格式的命令。
// 將命令字串編碼為 4 位 hex 長度前綴 + 命令內容後寫入 w。
// 例如 "host:version"（長度 12）→ "000chost:version"。
//
// 參數：
//   - w: 任意 io.Writer（可為 net.Conn、bytes.Buffer 等）
//   - cmd: ADB 命令字串
//
// 回傳：寫入失敗時回傳 error
func SendCommand(w io.Writer, cmd string) error {
	msg := fmt.Sprintf("%04x%s", len(cmd), cmd)
	_, err := io.WriteString(w, msg)
	return err
}

// ReadStatus 讀取 ADB server 的 4-byte 狀態回應。
// 預期為 "OKAY"（成功）或 "FAIL"（失敗）。
// 收到 "FAIL" 時會繼續讀取 4 位 hex 長度 + 錯誤訊息，
// 回傳包含完整錯誤內容的 error。
//
// 參數：
//   - r: 任意 io.Reader（可為 net.Conn、bytes.Buffer 等）
//
// 回傳：
//   - "OKAY" → nil
//   - "FAIL" → 包含 FAIL 訊息的 error
//   - 其他 → 包含未預期狀態碼的 error
//   - 讀取失敗 → 包裝後的 I/O error
func ReadStatus(r io.Reader) error {
	status := make([]byte, 4)
	if _, err := io.ReadFull(r, status); err != nil {
		return fmt.Errorf("read ADB response status: %w", err)
	}

	switch string(status) {
	case "OKAY":
		return nil
	case "FAIL":
		// 讀取錯誤訊息長度 + 內容
		lenHex := make([]byte, 4)
		if _, err := io.ReadFull(r, lenHex); err != nil {
			return fmt.Errorf("ADB FAIL (cannot read error message)")
		}
		length, err := parseHexLength(lenHex)
		if err != nil {
			return fmt.Errorf("ADB FAIL (cannot parse error length)")
		}
		msg := make([]byte, length)
		if _, err := io.ReadFull(r, msg); err != nil {
			return fmt.Errorf("ADB FAIL (cannot read error content)")
		}
		return fmt.Errorf("ADB FAIL: %s", string(msg))
	default:
		return fmt.Errorf("unexpected ADB response: %s", string(status))
	}
}
