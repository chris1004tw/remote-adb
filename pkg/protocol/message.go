// Package protocol 定義 radb 系統中所有信令訊息的共用格式。
// 包含 Envelope 外層封裝與各種 Payload 型別，
// 供 Signal Server、Agent、Client 三端共同使用。
package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// MessageType 列舉所有信令訊息類型。
type MessageType string

const (
	// 認證
	MsgTypeAuth    MessageType = "auth"
	MsgTypeAuthAck MessageType = "auth_ack"

	// Host 註冊
	MsgTypeRegister   MessageType = "register"
	MsgTypeUnregister MessageType = "unregister"

	// 主機與設備查詢
	MsgTypeHostList     MessageType = "host_list"
	MsgTypeHostListResp MessageType = "host_list_resp"
	MsgTypeDeviceUpdate MessageType = "device_update"

	// 設備鎖定
	MsgTypeLockReq    MessageType = "lock_req"
	MsgTypeLockResp   MessageType = "lock_resp"
	MsgTypeUnlockReq  MessageType = "unlock_req"
	MsgTypeUnlockResp MessageType = "unlock_resp"

	// WebRTC 信令
	MsgTypeOffer     MessageType = "offer"
	MsgTypeAnswer    MessageType = "answer"
	MsgTypeCandidate MessageType = "candidate"

	// 錯誤
	MsgTypeError MessageType = "error"
)

// DeviceState 表示設備的硬體狀態。
type DeviceState string

const (
	DeviceStateOnline  DeviceState = "device"
	DeviceStateOffline DeviceState = "offline"
)

// LockState 表示設備的鎖定狀態。
type LockState string

const (
	LockAvailable LockState = "available"
	LockLocked    LockState = "locked"
)

// Role 表示連線端的角色。
type Role string

const (
	RoleAgent  Role = "agent"
	RoleClient Role = "client"
)

// --- 錯誤碼 ---

const (
	ErrCodeDeviceLocked    = 4001
	ErrCodeDeviceNotFound  = 4002
	ErrCodeHostNotFound    = 4003
	ErrCodeAuthFailed      = 4004
	ErrCodeTargetOffline   = 4005
	ErrCodeInternalError   = 5001
)

// --- Envelope ---

// Envelope 是所有信令訊息的外層封裝。
type Envelope struct {
	Type      MessageType     `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Hostname  string          `json:"hostname"`
	SourceID  string          `json:"source_id"`
	TargetID  string          `json:"target_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

// NewEnvelope 建立一個含有當前時間戳的 Envelope。
func NewEnvelope(msgType MessageType, hostname, sourceID, targetID string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("序列化 payload 失敗: %w", err)
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

// DecodePayload 將 Envelope 的 Payload 反序列化到指定的目標型別。
func (e *Envelope) DecodePayload(target any) error {
	return json.Unmarshal(e.Payload, target)
}

// --- 認證 ---

// AuthPayload 是 auth 訊息的 payload。
type AuthPayload struct {
	Token string `json:"token"`
	Role  Role   `json:"role"`
}

// AuthAckPayload 是 auth_ack 訊息的 payload。
type AuthAckPayload struct {
	Success  bool   `json:"success"`
	AssignID string `json:"assign_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// --- 主機與設備 ---

// HostInfo 描述一台已註冊的遠端主機及其設備列表。
type HostInfo struct {
	HostID   string       `json:"host_id"`
	Hostname string       `json:"hostname"`
	Devices  []DeviceInfo `json:"devices"`
}

// DeviceInfo 描述單一 Android 設備的狀態。
type DeviceInfo struct {
	Serial   string      `json:"serial"`
	State    DeviceState `json:"state"`
	Lock     LockState   `json:"lock"`
	LockedBy string      `json:"locked_by,omitempty"`
}

// RegisterPayload 是 register 訊息的 payload（Agent → Signal）。
type RegisterPayload struct {
	HostID   string       `json:"host_id"`
	Hostname string       `json:"hostname"`
	Devices  []DeviceInfo `json:"devices"`
}

// HostListRespPayload 是 host_list_resp 訊息的 payload。
type HostListRespPayload struct {
	Hosts []HostInfo `json:"hosts"`
}

// DeviceUpdatePayload 是 device_update 訊息的 payload。
type DeviceUpdatePayload struct {
	HostID  string       `json:"host_id"`
	Devices []DeviceInfo `json:"devices"`
}

// --- 設備鎖定 ---

// LockReqPayload 是 lock_req 訊息的 payload。
type LockReqPayload struct {
	HostID string `json:"host_id"`
	Serial string `json:"serial"`
}

// LockRespPayload 是 lock_resp 訊息的 payload。
type LockRespPayload struct {
	Success bool   `json:"success"`
	Serial  string `json:"serial"`
	Reason  string `json:"reason,omitempty"`
}

// UnlockReqPayload 是 unlock_req 訊息的 payload。
type UnlockReqPayload struct {
	HostID string `json:"host_id"`
	Serial string `json:"serial"`
}

// UnlockRespPayload 是 unlock_resp 訊息的 payload。
type UnlockRespPayload struct {
	Success bool   `json:"success"`
	Serial  string `json:"serial"`
	Reason  string `json:"reason,omitempty"`
}

// --- WebRTC 信令 ---

// SDPPayload 是 offer/answer 訊息的 payload。
type SDPPayload struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"`
}

// CandidatePayload 是 candidate 訊息的 payload。
type CandidatePayload struct {
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdp_mid"`
	SDPMLineIndex int    `json:"sdp_mline_index"`
}

// --- 錯誤 ---

// ErrorPayload 是 error 訊息的 payload。
type ErrorPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
