// forward_protocol.go 實作 ADB forward 相關的協定解析與回應工具函式。
// 這些函式皆為無狀態的純函式，不依賴 ForwardManager。
package bridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/chris1004tw/remote-adb/internal/adb"
)

// FwdListener 追蹤一個 adb forward 的本機 TCP listener。
// 每當有連線進來，會建立 DataChannel 轉發到遠端設備的指定服務。
type FwdListener struct {
	ln         net.Listener
	serial     string
	localSpec  string
	remoteSpec string
	cancel     context.CancelFunc
}

// FwdCmd 表示解析後的 ADB forward 命令。
// 例如 `adb forward tcp:27183 localabstract:scrcpy` 解析為：
// Serial=""（未指定），LocalSpec="tcp:27183"，RemoteSpec="localabstract:scrcpy"。
type FwdCmd struct {
	Serial     string // 目標設備序號（可能為空，由 ResolveSerial 映射）
	LocalSpec  string // 本機 spec (e.g., "tcp:27183")
	RemoteSpec string // 遠端 spec (e.g., "localabstract:scrcpy")
}

// --- ADB Server 協定輔助函式 ---
// ADB server 使用文字協定：每個命令/回應以 4 字元 hex 長度前綴 + 內容。
// 例如發送 "host:version" → "000chost:version"。
// 回應以 "OKAY" 或 "FAIL" + 4 字元 hex 長度 + 錯誤訊息。
// 命令發送與狀態讀取統一使用 adb.SendCommand / adb.ReadStatus。

// WriteADBOkay 寫入 ADB OKAY 回應。
func WriteADBOkay(w io.Writer) error {
	_, err := w.Write([]byte("OKAY"))
	return err
}

// WriteADBFail 寫入 ADB FAIL + 訊息。
func WriteADBFail(w io.Writer, msg string) error {
	resp := fmt.Sprintf("FAIL%04x%s", len(msg), msg)
	_, err := w.Write([]byte(resp))
	return err
}

// QueryDeviceFeatures 透過 ADB server 協定查詢指定設備的 feature 清單。
// 回傳逗號分隔的 feature 字串（如 "shell_v2,cmd,stat_v2,..."），
// 用於 CNXN 回應的 banner，讓遠端 adb client 知道設備支援哪些功能。
// 連線與讀寫皆有 5 秒逾時保護，避免 ADB server 無回應時無限阻塞。
func QueryDeviceFeatures(adbAddr, serial string) (string, error) {
	conn, err := net.DialTimeout("tcp", adbAddr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	cmd := fmt.Sprintf("host-serial:%s:features", serial)
	if err := adb.SendCommand(conn, cmd); err != nil {
		return "", err
	}
	if err := adb.ReadStatus(conn); err != nil {
		return "", err
	}

	// 讀取 hex-length-prefixed 回應
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseInt(string(lenBuf[:]), 16, 32)
	if err != nil {
		return "", err
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(conn, data); err != nil {
		return "", err
	}
	return string(data), nil
}

// QueryDeviceModel 透過 ADB server 查詢指定設備的機型名稱。
// 使用 host:transport + shell:getprop ro.product.model 取得使用者可讀的設備名稱
// （如 "Pixel 10 Pro XL"），用於 GUI 設備列表顯示。
// 查詢失敗時回傳空字串（不影響核心功能）。
func QueryDeviceModel(adbAddr, serial string) string {
	conn, err := net.DialTimeout("tcp", adbAddr, 3*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// 切換到指定設備的 transport
	if err := adb.SendCommand(conn, fmt.Sprintf("host:transport:%s", serial)); err != nil {
		return ""
	}
	if err := adb.ReadStatus(conn); err != nil {
		return ""
	}

	// 開啟 shell 服務查詢 model
	if err := adb.SendCommand(conn, "shell:getprop ro.product.model"); err != nil {
		return ""
	}
	if err := adb.ReadStatus(conn); err != nil {
		return ""
	}

	// 讀取回應（機型名稱，通常一行）
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	return strings.TrimSpace(string(buf[:n]))
}

// --- Forward 命令解析 ---
// 以下函式解析 ADB server 協定中的 forward 相關命令。
// ADB 的 forward 命令有多種格式（帶/不帶 serial、帶/不帶 norebind），
// 這些解析器統一處理各種變體。

// ParseForwardCmd 解析 ADB forward 命令為 FwdCmd 結構。
// 支援格式：host:forward:、host:forward:norebind:、host-serial:<serial>:forward: 等。
// LocalSpec 和 RemoteSpec 以分號（;）分隔。
func ParseForwardCmd(cmd string) *FwdCmd {
	var rest, serial string

	switch {
	case strings.HasPrefix(cmd, "host-serial:"):
		after := cmd[len("host-serial:"):]
		idx := strings.Index(after, ":forward:")
		if idx < 0 {
			return nil
		}
		serial = after[:idx]
		rest = after[idx+len(":forward:"):]
	case strings.HasPrefix(cmd, "host:forward:"):
		rest = cmd[len("host:forward:"):]
	default:
		return nil
	}

	rest = strings.TrimPrefix(rest, "norebind:")

	parts := strings.SplitN(rest, ";", 2)
	if len(parts) != 2 {
		return nil
	}

	return &FwdCmd{Serial: serial, LocalSpec: parts[0], RemoteSpec: parts[1]}
}

// ParseKillForwardCmd 解析 killforward 命令，回傳 localSpec。
func ParseKillForwardCmd(cmd string) (string, bool) {
	switch {
	case strings.HasPrefix(cmd, "host-serial:"):
		after := cmd[len("host-serial:"):]
		idx := strings.Index(after, ":killforward:")
		if idx < 0 {
			return "", false
		}
		return after[idx+len(":killforward:"):], true
	case strings.HasPrefix(cmd, "host:killforward:"):
		return cmd[len("host:killforward:"):], true
	}
	return "", false
}

// IsKillForwardAll 判斷是否為 killforward-all 命令。
func IsKillForwardAll(cmd string) bool {
	if cmd == "host:killforward-all" {
		return true
	}
	return strings.HasPrefix(cmd, "host-serial:") && strings.HasSuffix(cmd, ":killforward-all")
}

// IsListForward 判斷是否為 list-forward 命令。
func IsListForward(cmd string) bool {
	if cmd == "host:list-forward" {
		return true
	}
	return strings.HasPrefix(cmd, "host-serial:") && strings.HasSuffix(cmd, ":list-forward")
}

// ParseLocalSpec 解析 forward 的 local spec（如 "tcp:27183"），回傳 port 數值。
// 目前僅支援 tcp: 格式，不支援 localabstract: 等其他 spec。
func ParseLocalSpec(spec string) (int, error) {
	if !strings.HasPrefix(spec, "tcp:") {
		return 0, fmt.Errorf("unsupported local spec: %s", spec)
	}
	port, err := strconv.Atoi(spec[4:])
	if err != nil {
		return 0, fmt.Errorf("invalid port: %s", spec)
	}
	return port, nil
}
