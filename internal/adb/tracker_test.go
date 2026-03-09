package adb_test

import (
	"testing"

	"github.com/chris1004tw/remote-adb/internal/adb"
)

func TestParseDeviceList_SingleDevice(t *testing.T) {
	payload := "ABCD1234\tdevice\n"
	devices := adb.ParseDeviceList(payload)
	if len(devices) != 1 {
		t.Fatalf("設備數量 = %d, 預期 1", len(devices))
	}
	if devices[0].Serial != "ABCD1234" {
		t.Errorf("Serial = %q, 預期 %q", devices[0].Serial, "ABCD1234")
	}
	if devices[0].State != "device" {
		t.Errorf("State = %q, 預期 %q", devices[0].State, "device")
	}
}

func TestParseDeviceList_MultipleDevices(t *testing.T) {
	payload := "DEV001\tdevice\nDEV002\toffline\nDEV003\tunauthorized\n"
	devices := adb.ParseDeviceList(payload)
	if len(devices) != 3 {
		t.Fatalf("設備數量 = %d, 預期 3", len(devices))
	}
	if devices[1].State != "offline" {
		t.Errorf("devices[1].State = %q, 預期 %q", devices[1].State, "offline")
	}
	if devices[2].State != "unauthorized" {
		t.Errorf("devices[2].State = %q, 預期 %q", devices[2].State, "unauthorized")
	}
}

func TestParseDeviceList_EmptyPayload(t *testing.T) {
	devices := adb.ParseDeviceList("")
	if len(devices) != 0 {
		t.Errorf("空 payload 應回傳空列表，但得到 %d 個設備", len(devices))
	}
}

func TestParseDeviceList_IgnoresInvalidLines(t *testing.T) {
	payload := "DEV001\tdevice\ninvalid-line\n\nDEV002\toffline\n"
	devices := adb.ParseDeviceList(payload)
	if len(devices) != 2 {
		t.Errorf("設備數量 = %d, 預期 2（忽略無效行）", len(devices))
	}
}

func TestParseDeviceList_WithExtraWhitespace(t *testing.T) {
	payload := "  DEV001\tdevice  \n"
	devices := adb.ParseDeviceList(payload)
	if len(devices) != 1 {
		t.Fatalf("設備數量 = %d, 預期 1", len(devices))
	}
	// 注意 trimming 行為
	if devices[0].Serial != "DEV001" {
		t.Errorf("Serial = %q, 預期 %q", devices[0].Serial, "DEV001")
	}
}
