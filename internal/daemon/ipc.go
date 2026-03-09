package daemon

import "encoding/json"

// IPCCommand 是 CLI 透過 IPC 發送給 Daemon 的指令。
type IPCCommand struct {
	Action  string          `json:"action"` // "bind", "unbind", "list", "status", "hosts"
	Payload json.RawMessage `json:"payload,omitempty"`
}

// IPCResponse 是 Daemon 回傳給 CLI 的回應。
type IPCResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
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

// StatusInfo 是 status 指令的回應資料。
type StatusInfo struct {
	Connected bool   `json:"connected"`
	ConnID    string `json:"conn_id,omitempty"`
	ServerURL string `json:"server_url"`
	BindCount int    `json:"bind_count"`
}

// BindRequest 是 bind 指令的 payload。
type BindRequest struct {
	HostID string `json:"host_id"`
	Serial string `json:"serial"`
}

// BindResult 是 bind 指令的回應。
type BindResult struct {
	LocalPort int    `json:"local_port"`
	Serial    string `json:"serial"`
}

// UnbindRequest 是 unbind 指令的 payload。
type UnbindRequest struct {
	LocalPort int `json:"local_port"`
}
