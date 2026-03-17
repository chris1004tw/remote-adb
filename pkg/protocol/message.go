// Package protocol 定義 radb 系統中所有信令訊息的共用格式。
// 包含 Envelope 外層封裝與各種 Payload 型別，
// 供 Signal Server、Agent、Client 三端共同使用。
//
// # Envelope + Payload 兩層結構設計
//
// 本套件採用「外層信封 + 內層酬載」的兩層 JSON 結構：
//
//   - Envelope：固定欄位（type、timestamp、hostname、source_id、target_id、payload），
//     所有訊息共用，路由層只需解析 Envelope 即可決定轉發目標。
//   - Payload：使用 json.RawMessage 延遲解析，各 MessageType 對應不同的 struct。
//     接收端先讀取 Envelope.Type 判斷訊息類型，再將 Payload 反序列化為對應的型別。
//
// 這種設計的好處：
//   - Signal Server 做路由轉發時不需要解析每種 Payload，降低耦合
//   - 新增訊息類型時只需加 Payload struct，不影響路由邏輯
//   - 減少不必要的序列化/反序列化成本
//
// # 錯誤碼分類規則
//
// 錯誤碼採用類似 HTTP 的數字分層：
//   - 4xxx（應用層錯誤）：由使用者操作或業務邏輯觸發的可預期錯誤
//     （如設備已鎖定、設備不存在、認證失敗等）
//   - 5xxx（內部錯誤）：伺服器端非預期的系統錯誤
//     （如資料庫異常、內部狀態不一致等）
package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// MessageType 列舉所有信令訊息類型。
// 每種類型都有對應的 Payload struct，接收端根據 Type 決定如何反序列化 Payload。
type MessageType string

const (
	// --- 認證階段 ---
	MsgTypeAuth    MessageType = "auth"     // Agent/Client → Signal Server：連線後首先發送，攜帶 PSK token 與角色
	MsgTypeAuthAck MessageType = "auth_ack" // Signal Server → Agent/Client：認證結果，成功時回傳分配的 session ID

	// --- Host 註冊（Agent 專用）---
	MsgTypeRegister   MessageType = "register"   // Agent → Signal Server：註冊自身主機資訊與設備列表
	MsgTypeUnregister MessageType = "unregister"  // Agent → Signal Server：主動取消註冊（斷線時 Server 也會自動清理）

	// --- 主機與設備查詢 ---
	MsgTypeHostList     MessageType = "host_list"      // Client → Signal Server：請求所有已註冊的 Agent 主機清單
	MsgTypeHostListResp MessageType = "host_list_resp"  // Signal Server → Client：回傳主機清單（含各主機的設備列表）
	MsgTypeDeviceUpdate MessageType = "device_update"   // Agent → Signal Server：設備狀態變更時主動推送更新

	// --- 設備鎖定（互斥存取）---
	MsgTypeLockReq    MessageType = "lock_req"    // Client → Signal Server → Agent：請求鎖定指定設備
	MsgTypeLockResp   MessageType = "lock_resp"   // Agent → Signal Server → Client：鎖定結果
	MsgTypeUnlockReq  MessageType = "unlock_req"  // Client → Signal Server → Agent：請求解鎖指定設備
	MsgTypeUnlockResp MessageType = "unlock_resp"  // Agent → Signal Server → Client：解鎖結果

	// --- WebRTC 信令交換 ---
	MsgTypeOffer     MessageType = "offer"     // Client → Signal Server → Agent：WebRTC SDP Offer
	MsgTypeAnswer    MessageType = "answer"    // Agent → Signal Server → Client：WebRTC SDP Answer
	MsgTypeCandidate MessageType = "candidate" // 雙向：ICE Candidate 交換（Client ↔ Signal Server ↔ Agent）

	// --- 錯誤通知 ---
	MsgTypeError MessageType = "error" // Signal Server → Agent/Client：操作失敗時的錯誤回應
)

// DeviceState 表示 Android 設備的硬體連線狀態。
// 對應 ADB track-devices 回報的狀態字串。
type DeviceState string

const (
	// DeviceStateOnline 表示設備已連線且可操作。
	// 值為 "device"（而非 "online"），與 ADB 協定回傳的狀態字串一致。
	DeviceStateOnline DeviceState = "device"
	// DeviceStateOffline 表示設備已斷線或無回應。
	DeviceStateOffline DeviceState = "offline"
)

// LockState 表示設備的鎖定狀態，用於實現設備的互斥存取。
// 同一時間，一台 Android 設備只允許被一位使用者（Client）鎖定並操作。
type LockState string

const (
	// LockAvailable 表示設備未被任何人鎖定，可供 Client 請求連線。
	LockAvailable LockState = "available"
	// LockLocked 表示設備已被某位 Client 鎖定，其他人無法操作直到解鎖。
	LockLocked LockState = "locked"
)

// Role 表示 WebSocket 連線端的角色，在認證（auth）階段由連線方自行聲明。
// Signal Server 根據角色決定訊息的路由邏輯：
//   - Agent 的訊息會被轉發給對應的 Client
//   - Client 的訊息會被轉發給對應的 Agent
type Role string

const (
	// RoleAgent 為遠端代理端，部署在掛載 Android 設備的主機上。
	RoleAgent Role = "agent"
	// RoleClient 為開發者端，透過 CLI 或 GUI 操作遠端設備。
	RoleClient Role = "client"
)

// --- 錯誤碼 ---
//
// 4xxx 系列：應用層錯誤（可預期，由使用者操作或業務邏輯觸發）
// 5xxx 系列：內部錯誤（非預期，表示伺服器端發生異常）

const (
	ErrCodeDeviceLocked   = 4001 // 設備已被其他 Client 鎖定
	ErrCodeDeviceNotFound = 4002 // 指定的設備序號不存在
	ErrCodeHostNotFound   = 4003 // 指定的 Agent 主機不存在或已離線
	ErrCodeAuthFailed     = 4004 // PSK token 驗證失敗
	ErrCodeTargetOffline  = 4005 // 訊息轉發目標（Agent 或 Client）已離線
	ErrCodeInternalError  = 5001 // 伺服器內部非預期錯誤
)

// --- Envelope ---

// Envelope 是所有信令訊息的外層封裝（信封），包含路由與元資料欄位。
// Signal Server 僅需解析 Envelope 層即可完成訊息路由，無需理解 Payload 內容。
// Payload 欄位使用 json.RawMessage，在需要時才由接收端延遲反序列化為對應的型別。
type Envelope struct {
	Type      MessageType     `json:"type"`               // 訊息類型，決定 Payload 的反序列化目標型別
	Timestamp time.Time       `json:"timestamp"`           // 訊息建立時間（UTC）
	Hostname  string          `json:"hostname"`            // 發送方的主機名稱
	SourceID  string          `json:"source_id"`           // 發送方的 session ID（由 Signal Server 在 auth_ack 時分配）
	TargetID  string          `json:"target_id,omitempty"` // 接收方的 session ID（點對點訊息時填寫）
	Payload   json.RawMessage `json:"payload"`             // 延遲解析的酬載，使用 DecodePayload() 反序列化
}

// NewEnvelope 建立一個含有當前 UTC 時間戳的 Envelope。
// payload 參數接受任意型別，會先序列化為 json.RawMessage 存入 Envelope.Payload。
func NewEnvelope(msgType MessageType, hostname, sourceID, targetID string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("failed to marshal payload: %w", err)
	}
	return Envelope{
		Type:      msgType,
		Timestamp: time.Now().UTC(),
		Hostname:  hostname,
		SourceID:  sourceID,
		TargetID:  targetID,
		Payload:   raw,
	}, nil
}

// DecodePayload 將 Envelope 的 Payload（json.RawMessage）反序列化到指定的目標型別。
// 使用方式：先檢查 Envelope.Type，再呼叫 DecodePayload 傳入對應的 Payload struct 指標。
// 例如：若 Type == MsgTypeAuth，則傳入 &AuthPayload{} 作為 target。
func (e *Envelope) DecodePayload(target any) error {
	return json.Unmarshal(e.Payload, target)
}

// --- 認證 ---

// AuthPayload 是 auth 訊息的 payload（Agent/Client → Signal Server）。
// 連線建立後的第一筆訊息，攜帶 PSK token 進行身份驗證，並聲明自身角色。
type AuthPayload struct {
	Token string `json:"token"` // 預共享密鑰（Pre-Shared Key），須與 Signal Server 設定一致
	Role  Role   `json:"role"`  // 連線角色：agent 或 client
}

// AuthAckPayload 是 auth_ack 訊息的 payload（Signal Server → Agent/Client）。
// 認證成功時分配唯一的 session ID，後續所有訊息以此 ID 作為路由標識。
type AuthAckPayload struct {
	Success  bool   `json:"success"`              // 認證是否成功
	AssignID string `json:"assign_id,omitempty"`  // 成功時分配的 session ID
	Reason   string `json:"reason,omitempty"`     // 失敗時的原因說明
}

// --- 主機與設備 ---

// HostInfo 描述一台已註冊的遠端主機及其設備列表。
// 由 Signal Server 在回應 host_list_resp 時組裝。
type HostInfo struct {
	HostID   string       `json:"host_id"`  // Agent 的 session ID
	Hostname string       `json:"hostname"` // Agent 主機的顯示名稱
	Devices  []DeviceInfo `json:"devices"`  // 該主機上的所有 Android 設備
}

// DeviceInfo 描述單一 Android 設備的狀態，同時用於 Signal Server 模式與 Direct 模式。
type DeviceInfo struct {
	Serial   string      `json:"serial"`              // 設備序號（如 "ABC123"、"emulator-5554"）
	State    DeviceState `json:"state"`               // 硬體連線狀態
	Lock     LockState   `json:"lock"`                // 鎖定狀態
	LockedBy string      `json:"locked_by,omitempty"` // 鎖定者的 session ID 或 client ID
}

// RegisterPayload 是 register 訊息的 payload（Agent → Signal Server）。
// Agent 啟動後向 Signal Server 註冊自身資訊，Server 據此維護全域主機表。
type RegisterPayload struct {
	HostID   string       `json:"host_id"`  // Agent 的 session ID（auth_ack 時取得）
	Hostname string       `json:"hostname"` // Agent 主機名稱
	Devices  []DeviceInfo `json:"devices"`  // 初始設備清單
}

// HostListRespPayload 是 host_list_resp 訊息的 payload（Signal Server → Client）。
// 回傳所有已註冊的 Agent 主機與其設備列表，供 Client 選擇要連線的設備。
type HostListRespPayload struct {
	Hosts []HostInfo `json:"hosts"`
}

// DeviceUpdatePayload 是 device_update 訊息的 payload（Agent → Signal Server）。
// 當 Agent 偵測到設備插拔或狀態變更時主動推送，Server 據此更新全域主機表並通知 Client。
type DeviceUpdatePayload struct {
	HostID  string       `json:"host_id"`  // 發送更新的 Agent session ID
	Devices []DeviceInfo `json:"devices"`  // 更新後的完整設備清單
}

// --- 設備鎖定 ---
//
// 鎖定機制確保同一台 Android 設備同時只被一位 Client 使用，
// 避免多人同時操作導致 ADB 協定衝突。

// LockReqPayload 是 lock_req 訊息的 payload（Client → Signal Server → Agent）。
// Client 在建立 WebRTC 連線前，先請求鎖定目標設備。
type LockReqPayload struct {
	HostID string `json:"host_id"` // 目標 Agent 的 session ID
	Serial string `json:"serial"`  // 要鎖定的設備序號
}

// LockRespPayload 是 lock_resp 訊息的 payload（Agent → Signal Server → Client）。
type LockRespPayload struct {
	Success bool   `json:"success"`          // 鎖定是否成功
	Serial  string `json:"serial"`           // 對應的設備序號
	Reason  string `json:"reason,omitempty"` // 失敗原因（如 "已被其他使用者鎖定"）
}

// UnlockReqPayload 是 unlock_req 訊息的 payload（Client → Signal Server → Agent）。
// Client 使用完畢或斷線時，釋放設備鎖定。
type UnlockReqPayload struct {
	HostID string `json:"host_id"` // 目標 Agent 的 session ID
	Serial string `json:"serial"`  // 要解鎖的設備序號
}

// UnlockRespPayload 是 unlock_resp 訊息的 payload（Agent → Signal Server → Client）。
type UnlockRespPayload struct {
	Success bool   `json:"success"`          // 解鎖是否成功
	Serial  string `json:"serial"`           // 對應的設備序號
	Reason  string `json:"reason,omitempty"` // 失敗原因（如 "非鎖定者無法解鎖"）
}

// --- WebRTC 信令 ---
//
// WebRTC 連線建立需要交換 SDP（Session Description Protocol）和 ICE Candidate。
// 這些訊息透過 Signal Server 在 Client 與 Agent 之間中繼。

// SDPPayload 是 offer/answer 訊息的 payload，攜帶 WebRTC SDP 描述。
// Client 發送 offer，Agent 回傳 answer，完成 WebRTC 協商。
type SDPPayload struct {
	SDP  string `json:"sdp"`  // SDP 內容（Base64 或純文字，視 WebRTC 實作而定）
	Type string `json:"type"` // "offer" 或 "answer"
}

// CandidatePayload 是 candidate 訊息的 payload，攜帶 ICE Candidate 資訊。
// ICE Candidate 描述了一條可能的網路路徑（本地、STUN 反射、TURN 中繼），
// 雙方交換後由 WebRTC 引擎選擇最佳路徑建立 P2P 連線。
type CandidatePayload struct {
	Candidate     string `json:"candidate"`       // ICE Candidate 字串（SDP 格式）
	SDPMid        string `json:"sdp_mid"`         // 對應的 SDP media stream ID
	SDPMLineIndex int    `json:"sdp_mline_index"` // 對應的 SDP media line 索引
}

// --- 錯誤 ---

// ErrorPayload 是 error 訊息的 payload（Signal Server → Agent/Client）。
// 當 Server 無法處理請求時回傳此 payload，Code 欄位使用上方定義的 ErrCode 常數。
type ErrorPayload struct {
	Code    int    `json:"code"`    // 錯誤碼：4xxx 為應用層錯誤，5xxx 為內部錯誤
	Message string `json:"message"` // 人類可讀的錯誤描述
}
