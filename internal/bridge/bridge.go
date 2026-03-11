// bridge 套件提供 ADB bridge 的核心邏輯，讓 CLI 和 GUI 共用。
//
// 主要功能：
//   - ADB device transport 多工橋接（deviceBridge）
//   - ADB forward 命令攔截與管理（ForwardManager）
//   - 被控端 DataChannel 處理（ServerHandler）
//   - Control channel 設備推送/接收（DevicePushLoop / ControlReadLoop）
//   - SDP 緊湊編碼（compactSDP）
//   - 主控端 proxy session 生命週期管理（ProxySession）
//
// 設計原則：本套件不依賴任何 GUI 框架（如 Gio），
// 透過 callback 和 interface 與上層（GUI / CLI）互動。
package bridge

import (
	"context"
	"io"
	"time"
)

// DeviceInfo 表示一台遠端 ADB 設備的資訊。
// 用於 control channel 協定和 ForwardManager 的設備清單。
type DeviceInfo struct {
	Serial   string `json:"serial"`            // 設備序號（如 "emulator-5554"）
	State    string `json:"state"`             // 設備狀態："device"、"offline"、"no device"
	Features string `json:"features,omitempty"` // 逗號分隔的 feature 清單（如 "shell_v2,cmd,stat_v2"）
}

// OpenChannelFunc 是開啟命名 channel 的函式類型。
// 用於抽象化 DataChannel（WebRTC P2P）或 TCP 連線（LAN 直連），
// 讓 ADB bridge 不依賴特定傳輸層實作。
//
// label 格式：
//   - "control" — 控制通道（設備清單推送）
//   - "adb-server/{id}" — ADB server 協定轉發
//   - "adb-stream/{id}/{serial}/{service}" — 設備服務串流
//   - "adb-fwd/{id}/{serial}/{remoteSpec}" — forward 轉發
type OpenChannelFunc func(label string) (io.ReadWriteCloser, error)

// DeviceProvider 提供遠端設備清單與等待機制。
// GUI 的 pairTab/lanTab 和 CLI 的 connect 命令各自實作。
type DeviceProvider interface {
	// GetDevice 回傳第一個在線設備的 serial 和 features。
	// 若目前無設備，等待 timeout 後回傳空字串。
	// 回傳值：serial 為空字串表示逾時或 context 取消。
	GetDevice(ctx context.Context, timeout time.Duration) (serial, features string)

	// OnlineDevices 回傳目前所有在線設備（State=="device"）的清單。
	OnlineDevices() []DeviceInfo

	// ResolveSerial 將 requested serial 映射為遠端真實裝置 serial。
	// 映射邏輯：完全匹配 > 單設備自動映射 > 失敗。
	ResolveSerial(requested string) (string, bool)
}

// ReverseForwardManager 管理 reverse forward listeners。
// 被 deviceBridge 的 handleReverseOPEN 呼叫。
// 傳入 nil 表示不支援 reverse forward（如 LAN 直連模式）。
type ReverseForwardManager interface {
	// KillReverseForward 移除指定 remoteSpec 的 reverse forward。
	KillReverseForward(spec string) bool

	// KillReverseForwardAll 移除所有 reverse forward listeners。
	KillReverseForwardAll()

	// ListReverseForwards 回傳 reverse forward 清單（ADB 格式字串）。
	ListReverseForwards() string
}
