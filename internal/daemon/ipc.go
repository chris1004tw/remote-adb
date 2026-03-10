// ipc.go 定義 CLI 與 Daemon 之間的 IPC 通訊格式。
//
// IPC 採用 JSON-over-TCP/UnixSocket 的一問一答模式：
// CLI 發送一個 IPCCommand → Daemon 回傳一個 IPCResponse → 連線關閉。
//
// 支援的 Action 清單：
//
//	| Action   | Payload 格式       | Response Data 格式   | 說明                     |
//	|----------|-------------------|---------------------|--------------------------|
//	| "list"   | （無）              | []Binding           | 列出所有綁定關係           |
//	| "status" | （無）              | StatusInfo           | 查詢 Daemon 連線狀態       |
//	| "hosts"  | （無）              | []protocol.HostInfo  | 查詢遠端主機與設備列表     |
//	| "bind"   | BindRequest        | BindResult           | 綁定遠端設備到本機 port    |
//	| "unbind" | UnbindRequest      | null                 | 解除指定 port 的綁定       |
package daemon

import "encoding/json"

// IPCCommand 是 CLI 透過 IPC 發送給 Daemon 的指令。
// Action 指定操作類型，Payload 攜帶操作所需的參數（部分 Action 不需要 Payload）。
type IPCCommand struct {
	Action  string          `json:"action"`           // 操作類型："bind", "unbind", "list", "status", "hosts"
	Payload json.RawMessage `json:"payload,omitempty"` // 操作參數（JSON 延遲解析，由各 handler 自行 Unmarshal）
}

// IPCResponse 是 Daemon 回傳給 CLI 的統一回應格式。
// 成功時 Success=true 且 Data 包含結果；失敗時 Success=false 且 Error 包含錯誤訊息。
type IPCResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`  // 成功時的回應資料（JSON）
	Error   string          `json:"error,omitempty"` // 失敗時的錯誤訊息
}

// SuccessResponse 建立成功的 IPC 回應。
func SuccessResponse(data any) IPCResponse {
	raw, _ := json.Marshal(data)
	return IPCResponse{Success: true, Data: raw}
}

// ErrorResponse 建立失敗的 IPC 回應。
func ErrorResponse(msg string) IPCResponse {
	return IPCResponse{Success: false, Error: msg}
}

// StatusInfo 是 "status" 指令的回應資料，反映 Daemon 目前的運作狀態。
type StatusInfo struct {
	Connected bool   `json:"connected"`          // 是否已連線到 Signal Server
	ConnID    string `json:"conn_id,omitempty"`   // Server 分配的連線 ID
	ServerURL string `json:"server_url"`          // 連線的 Server URL
	BindCount int    `json:"bind_count"`          // 目前綁定的設備數量
}

// BindRequest 是 "bind" 指令的 payload，指定要綁定的遠端設備。
type BindRequest struct {
	HostID string `json:"host_id"` // 遠端 Agent 的 host ID
	Serial string `json:"serial"`  // 要綁定的 Android 設備序號
}

// BindResult 是 "bind" 指令成功後的回應，告知 CLI 分配到的本機 port。
type BindResult struct {
	LocalPort int    `json:"local_port"` // 分配到的本機 TCP port
	Serial    string `json:"serial"`     // 已綁定的設備序號
}

// UnbindRequest 是 "unbind" 指令的 payload，指定要解除綁定的本機 port。
type UnbindRequest struct {
	LocalPort int `json:"local_port"` // 要解除綁定的本機 port
}
