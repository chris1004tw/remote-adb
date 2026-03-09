package protocol_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/chris1004tw/remote-adb/pkg/protocol"
)

func TestMessageTypeConstants_NoDuplicates(t *testing.T) {
	types := []protocol.MessageType{
		protocol.MsgTypeAuth, protocol.MsgTypeAuthAck,
		protocol.MsgTypeRegister, protocol.MsgTypeUnregister,
		protocol.MsgTypeHostList, protocol.MsgTypeHostListResp,
		protocol.MsgTypeDeviceUpdate,
		protocol.MsgTypeLockReq, protocol.MsgTypeLockResp,
		protocol.MsgTypeUnlockReq, protocol.MsgTypeUnlockResp,
		protocol.MsgTypeOffer, protocol.MsgTypeAnswer, protocol.MsgTypeCandidate,
		protocol.MsgTypeError,
	}

	seen := make(map[protocol.MessageType]bool)
	for _, mt := range types {
		if seen[mt] {
			t.Errorf("MessageType 重複: %q", mt)
		}
		seen[mt] = true
	}
}

func TestNewEnvelope_ContainsTimestampAndHostname(t *testing.T) {
	before := time.Now().UTC()
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeAuth,
		"test-host",
		"source-1",
		"target-1",
		protocol.AuthPayload{Token: "secret", Role: protocol.RoleAgent},
	)
	after := time.Now().UTC()

	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	if env.Type != protocol.MsgTypeAuth {
		t.Errorf("Type = %q, 預期 %q", env.Type, protocol.MsgTypeAuth)
	}
	if env.Hostname != "test-host" {
		t.Errorf("Hostname = %q, 預期 %q", env.Hostname, "test-host")
	}
	if env.SourceID != "source-1" {
		t.Errorf("SourceID = %q, 預期 %q", env.SourceID, "source-1")
	}
	if env.TargetID != "target-1" {
		t.Errorf("TargetID = %q, 預期 %q", env.TargetID, "target-1")
	}
	if env.Timestamp.Before(before) || env.Timestamp.After(after) {
		t.Errorf("Timestamp %v 不在預期範圍 [%v, %v]", env.Timestamp, before, after)
	}
}

func TestEnvelope_MarshalJSON_RoundTrip(t *testing.T) {
	original, err := protocol.NewEnvelope(
		protocol.MsgTypeAuth,
		"my-pc",
		"client-001",
		"",
		protocol.AuthPayload{Token: "my-token", Role: protocol.RoleClient},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal 失敗: %v", err)
	}

	var decoded protocol.Envelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal 失敗: %v", err)
	}

	if decoded.Type != original.Type {
		t.Errorf("Type = %q, 預期 %q", decoded.Type, original.Type)
	}
	if decoded.Hostname != original.Hostname {
		t.Errorf("Hostname = %q, 預期 %q", decoded.Hostname, original.Hostname)
	}
	if decoded.SourceID != original.SourceID {
		t.Errorf("SourceID = %q, 預期 %q", decoded.SourceID, original.SourceID)
	}

	// TargetID 為空時應省略
	if decoded.TargetID != "" {
		t.Errorf("TargetID = %q, 預期為空", decoded.TargetID)
	}
}

func TestEnvelope_DecodePayload_AuthPayload(t *testing.T) {
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeAuth,
		"host",
		"src",
		"",
		protocol.AuthPayload{Token: "abc123", Role: protocol.RoleAgent},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var payload protocol.AuthPayload
	if err := env.DecodePayload(&payload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}

	if payload.Token != "abc123" {
		t.Errorf("Token = %q, 預期 %q", payload.Token, "abc123")
	}
	if payload.Role != protocol.RoleAgent {
		t.Errorf("Role = %q, 預期 %q", payload.Role, protocol.RoleAgent)
	}
}

func TestEnvelope_DecodePayload_AuthAckPayload(t *testing.T) {
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeAuthAck,
		"host",
		"signal",
		"client-1",
		protocol.AuthAckPayload{Success: true, AssignID: "agent-xyz"},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var payload protocol.AuthAckPayload
	if err := env.DecodePayload(&payload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}

	if !payload.Success {
		t.Error("Success = false, 預期 true")
	}
	if payload.AssignID != "agent-xyz" {
		t.Errorf("AssignID = %q, 預期 %q", payload.AssignID, "agent-xyz")
	}
}

func TestEnvelope_DecodePayload_RegisterPayload(t *testing.T) {
	devices := []protocol.DeviceInfo{
		{Serial: "ABCD1234", State: protocol.DeviceStateOnline, Lock: protocol.LockAvailable},
		{Serial: "EFGH5678", State: protocol.DeviceStateOffline, Lock: protocol.LockAvailable},
	}
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeRegister,
		"lab-pc",
		"agent-1",
		"",
		protocol.RegisterPayload{HostID: "agent-1", Hostname: "lab-pc", Devices: devices},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var payload protocol.RegisterPayload
	if err := env.DecodePayload(&payload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}

	if payload.HostID != "agent-1" {
		t.Errorf("HostID = %q, 預期 %q", payload.HostID, "agent-1")
	}
	if len(payload.Devices) != 2 {
		t.Fatalf("Devices 數量 = %d, 預期 2", len(payload.Devices))
	}
	if payload.Devices[0].Serial != "ABCD1234" {
		t.Errorf("Devices[0].Serial = %q, 預期 %q", payload.Devices[0].Serial, "ABCD1234")
	}
	if payload.Devices[0].State != protocol.DeviceStateOnline {
		t.Errorf("Devices[0].State = %q, 預期 %q", payload.Devices[0].State, protocol.DeviceStateOnline)
	}
	if payload.Devices[1].State != protocol.DeviceStateOffline {
		t.Errorf("Devices[1].State = %q, 預期 %q", payload.Devices[1].State, protocol.DeviceStateOffline)
	}
}

func TestEnvelope_DecodePayload_HostListRespPayload(t *testing.T) {
	hosts := []protocol.HostInfo{
		{
			HostID:   "agent-1",
			Hostname: "lab-pc-01",
			Devices: []protocol.DeviceInfo{
				{Serial: "DEV001", State: protocol.DeviceStateOnline, Lock: protocol.LockLocked, LockedBy: "client-1"},
			},
		},
	}
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeHostListResp,
		"signal",
		"signal",
		"client-1",
		protocol.HostListRespPayload{Hosts: hosts},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var payload protocol.HostListRespPayload
	if err := env.DecodePayload(&payload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}

	if len(payload.Hosts) != 1 {
		t.Fatalf("Hosts 數量 = %d, 預期 1", len(payload.Hosts))
	}
	if payload.Hosts[0].Devices[0].Lock != protocol.LockLocked {
		t.Errorf("Lock = %q, 預期 %q", payload.Hosts[0].Devices[0].Lock, protocol.LockLocked)
	}
	if payload.Hosts[0].Devices[0].LockedBy != "client-1" {
		t.Errorf("LockedBy = %q, 預期 %q", payload.Hosts[0].Devices[0].LockedBy, "client-1")
	}
}

func TestEnvelope_DecodePayload_LockReqResp(t *testing.T) {
	// Lock Request
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeLockReq,
		"dev-pc",
		"client-1",
		"agent-1",
		protocol.LockReqPayload{HostID: "agent-1", Serial: "DEV001"},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var reqPayload protocol.LockReqPayload
	if err := env.DecodePayload(&reqPayload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}
	if reqPayload.Serial != "DEV001" {
		t.Errorf("Serial = %q, 預期 %q", reqPayload.Serial, "DEV001")
	}

	// Lock Response
	env2, err := protocol.NewEnvelope(
		protocol.MsgTypeLockResp,
		"lab-pc",
		"agent-1",
		"client-1",
		protocol.LockRespPayload{Success: true, Serial: "DEV001"},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var respPayload protocol.LockRespPayload
	if err := env2.DecodePayload(&respPayload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}
	if !respPayload.Success {
		t.Error("Success = false, 預期 true")
	}
}

func TestEnvelope_DecodePayload_SDPPayload(t *testing.T) {
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeOffer,
		"dev-pc",
		"client-1",
		"agent-1",
		protocol.SDPPayload{SDP: "v=0\r\n...", Type: "offer"},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var payload protocol.SDPPayload
	if err := env.DecodePayload(&payload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}
	if payload.Type != "offer" {
		t.Errorf("Type = %q, 預期 %q", payload.Type, "offer")
	}
}

func TestEnvelope_DecodePayload_CandidatePayload(t *testing.T) {
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeCandidate,
		"dev-pc",
		"client-1",
		"agent-1",
		protocol.CandidatePayload{
			Candidate:     "candidate:1 1 udp 2130706431 192.168.1.1 50000 typ host",
			SDPMid:        "0",
			SDPMLineIndex: 0,
		},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var payload protocol.CandidatePayload
	if err := env.DecodePayload(&payload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}
	if payload.SDPMid != "0" {
		t.Errorf("SDPMid = %q, 預期 %q", payload.SDPMid, "0")
	}
}

func TestEnvelope_DecodePayload_ErrorPayload(t *testing.T) {
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeError,
		"signal",
		"signal",
		"client-1",
		protocol.ErrorPayload{Code: protocol.ErrCodeDeviceLocked, Message: "設備已被鎖定"},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var payload protocol.ErrorPayload
	if err := env.DecodePayload(&payload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}
	if payload.Code != protocol.ErrCodeDeviceLocked {
		t.Errorf("Code = %d, 預期 %d", payload.Code, protocol.ErrCodeDeviceLocked)
	}
	if payload.Message != "設備已被鎖定" {
		t.Errorf("Message = %q, 預期 %q", payload.Message, "設備已被鎖定")
	}
}

func TestEnvelope_JSON_TargetIDOmitEmpty(t *testing.T) {
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeHostList,
		"dev-pc",
		"client-1",
		"",
		nil,
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal 失敗: %v", err)
	}

	// 確認 JSON 中不包含 target_id
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map 失敗: %v", err)
	}
	if _, exists := raw["target_id"]; exists {
		t.Error("空的 target_id 應該被省略，但 JSON 中仍然存在")
	}
}

func TestEnvelope_DecodePayload_UnlockReqResp(t *testing.T) {
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeUnlockReq,
		"dev-pc",
		"client-1",
		"agent-1",
		protocol.UnlockReqPayload{HostID: "agent-1", Serial: "DEV001"},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var reqPayload protocol.UnlockReqPayload
	if err := env.DecodePayload(&reqPayload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}
	if reqPayload.Serial != "DEV001" {
		t.Errorf("Serial = %q, 預期 %q", reqPayload.Serial, "DEV001")
	}

	env2, err := protocol.NewEnvelope(
		protocol.MsgTypeUnlockResp,
		"lab-pc",
		"agent-1",
		"client-1",
		protocol.UnlockRespPayload{Success: false, Serial: "DEV001", Reason: "設備未被鎖定"},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var respPayload protocol.UnlockRespPayload
	if err := env2.DecodePayload(&respPayload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}
	if respPayload.Success {
		t.Error("Success = true, 預期 false")
	}
	if respPayload.Reason != "設備未被鎖定" {
		t.Errorf("Reason = %q, 預期 %q", respPayload.Reason, "設備未被鎖定")
	}
}

func TestEnvelope_DecodePayload_DeviceUpdatePayload(t *testing.T) {
	env, err := protocol.NewEnvelope(
		protocol.MsgTypeDeviceUpdate,
		"lab-pc",
		"agent-1",
		"",
		protocol.DeviceUpdatePayload{
			HostID: "agent-1",
			Devices: []protocol.DeviceInfo{
				{Serial: "AAA", State: protocol.DeviceStateOnline, Lock: protocol.LockAvailable},
				{Serial: "BBB", State: protocol.DeviceStateOnline, Lock: protocol.LockLocked, LockedBy: "client-2"},
			},
		},
	)
	if err != nil {
		t.Fatalf("NewEnvelope 失敗: %v", err)
	}

	var payload protocol.DeviceUpdatePayload
	if err := env.DecodePayload(&payload); err != nil {
		t.Fatalf("DecodePayload 失敗: %v", err)
	}

	if payload.HostID != "agent-1" {
		t.Errorf("HostID = %q, 預期 %q", payload.HostID, "agent-1")
	}
	if len(payload.Devices) != 2 {
		t.Fatalf("Devices 數量 = %d, 預期 2", len(payload.Devices))
	}
	if payload.Devices[1].LockedBy != "client-2" {
		t.Errorf("Devices[1].LockedBy = %q, 預期 %q", payload.Devices[1].LockedBy, "client-2")
	}
}

func TestErrorCodes_AreDistinct(t *testing.T) {
	codes := []int{
		protocol.ErrCodeDeviceLocked,
		protocol.ErrCodeDeviceNotFound,
		protocol.ErrCodeHostNotFound,
		protocol.ErrCodeAuthFailed,
		protocol.ErrCodeTargetOffline,
		protocol.ErrCodeInternalError,
	}

	seen := make(map[int]bool)
	for _, code := range codes {
		if seen[code] {
			t.Errorf("錯誤碼重複: %d", code)
		}
		seen[code] = true
	}
}
