// control.go 實作 P2P control channel 的通訊協定和共用邏輯。
//
// P2P 連線建立後，雙方透過 label="control" 的 DataChannel 交換 JSON 訊息：
//   - hello：被控端發送主機名稱
//   - devices：被控端定期推送設備清單（serial、state、features）
//
// 本檔案提供兩個核心函式：
//   - DevicePushLoop：被控端使用，追蹤本機 ADB 設備並推送給客戶端
//   - ControlReadLoop：客戶端使用，持續讀取 control channel 訊息
//
// 設計原則：不依賴任何 GUI 框架，透過 callback 與上層（GUI/CLI）互動。
package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/buildinfo"
)

// keepaliveInterval 是 control channel keepalive ping 的發送間隔。
// 防止 NAT mapping 因 SCTP idle 過久而失效，導致後續 DataChannel 建立逾時。
const keepaliveInterval = 30 * time.Second

// CtrlMessage 是 control channel 的 JSON 訊息格式。
// Type 可為 "hello"/"devices"（被控端→主控端）或 "refresh"（主控端→被控端）。
type CtrlMessage struct {
	Type     string       `json:"type"`               // "hello"、"devices"、"ping"、"refresh"
	Hostname string       `json:"hostname,omitempty"` // 遠端主機名稱（hello 訊息）
	Devices  []DeviceInfo `json:"devices,omitempty"`  // 設備清單（devices 訊息）
}

// SendCtrlRefresh 向被控端發送 refresh 請求，觸發重新推送設備清單。
// controlCh 須為 control DataChannel 的 Writer 端。
func SendCtrlRefresh(w io.Writer) error {
	return json.NewEncoder(w).Encode(CtrlMessage{Type: "refresh"})
}

// DevicePushLoop 追蹤本機 ADB 設備清單並透過 control channel 推送給客戶端。
// 使用 ADB tracker 的事件驅動模式（而非輪詢），設備增減時即時推送。
// 對每個在線設備額外查詢 features（如 shell_v2, cmd 等），讓客戶端的 CNXN 回應
// 能攜帶真實 features，避免 adb 功能不相容。
//
// 參數：
//   - ctx：用於取消追蹤迴圈
//   - controlCh：control DataChannel 的 ReadWriteCloser
//   - adbAddr：本機 ADB server 位址（如 "127.0.0.1:5037"）
//   - onUpdate：設備清單變更時呼叫（可用於 GUI 更新或 CLI 輸出），可為 nil
func DevicePushLoop(ctx context.Context, controlCh io.ReadWriteCloser, adbAddr string, onUpdate func([]DeviceInfo)) {
	tracker := adb.NewTracker(adbAddr)
	deviceCh := tracker.Track(ctx)
	table := adb.NewDeviceTable()
	enc := json.NewEncoder(controlCh)
	// features/model 快取：以設備 serial 為 key，避免每次設備事件都重新查詢 ADB server。
	// 設備離線時自動清除對應快取條目，確保重新上線時取得最新資訊。
	featuresCache := make(map[string]string)
	modelCache := make(map[string]string)

	// 先發送主機名稱
	if err := enc.Encode(CtrlMessage{Type: "hello", Hostname: buildinfo.Hostname()}); err != nil {
		slog.Debug("failed to send hello", "error", err)
		return
	}

	// 讀取主控端發來的訊息（如 refresh 請求）
	refreshCh := make(chan struct{}, 1)
	go func() {
		dec := json.NewDecoder(controlCh)
		for {
			var msg CtrlMessage
			if err := dec.Decode(&msg); err != nil {
				return
			}
			if msg.Type == "refresh" {
				slog.Debug("received device refresh request from client")
				select {
				case refreshCh <- struct{}{}:
				default: // 已有 pending refresh，不重複排隊
				}
			}
		}
	}()

	// buildDevices 根據 DeviceTable 當前狀態建構設備清單（含 features 快取查詢）。
	buildDevices := func() []DeviceInfo {
		devs := table.List()

		// 清除已離線設備的 features/model 快取
		online := make(map[string]bool, len(devs))
		for _, d := range devs {
			online[d.Serial] = true
		}
		for serial := range featuresCache {
			if !online[serial] {
				delete(featuresCache, serial)
				delete(modelCache, serial)
			}
		}

		devices := make([]DeviceInfo, len(devs))
		for i, d := range devs {
			devices[i] = DeviceInfo{Serial: d.Serial, State: d.State}
			if d.State == "device" {
				// features 查詢（含快取）
				if feat, ok := featuresCache[d.Serial]; ok {
					devices[i].Features = feat
				} else if feat, err := QueryDeviceFeatures(adbAddr, d.Serial); err == nil {
					featuresCache[d.Serial] = feat
					devices[i].Features = feat
				}
				// model 查詢（含快取）
				if model, ok := modelCache[d.Serial]; ok {
					devices[i].Model = model
				} else if model := QueryDeviceModel(adbAddr, d.Serial); model != "" {
					modelCache[d.Serial] = model
					devices[i].Model = model
				}
			}
		}
		return devices
	}

	// pushDevices 將設備清單推送給主控端並通知上層。
	pushDevices := func(devices []DeviceInfo) bool {
		if err := enc.Encode(CtrlMessage{Type: "devices", Devices: devices}); err != nil {
			slog.Debug("control channel write failed", "error", err)
			return false
		}
		if onUpdate != nil {
			onUpdate(devices)
		}
		return true
	}

	// keepalive ticker：定期送 ping 訊息，防止 SCTP idle 導致 NAT mapping 失效
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if err := enc.Encode(CtrlMessage{Type: "ping"}); err != nil {
				slog.Debug("control channel keepalive failed", "error", err)
				return
			}
		case <-refreshCh:
			// 主控端要求重新查詢：重建設備清單並推送
			if !pushDevices(buildDevices()) {
				return
			}
		case events, ok := <-deviceCh:
			if !ok {
				return
			}
			table.Update(events)
			if !pushDevices(buildDevices()) {
				return
			}
		}
	}
}

// ControlReadLoop 持續讀取 control channel 的 JSON 訊息。
// 每收到一則訊息就呼叫 onMessage，讓上層（GUI/CLI）決定如何處理。
//
// 參數：
//   - ctx：用於偵測取消（提前結束時不記錄錯誤）
//   - controlCh：control DataChannel 的 ReadWriteCloser
//   - onMessage：收到訊息時呼叫的 callback
//
// 回傳值：
//   - 正常結束（ctx 取消或 channel 關閉）時回傳 nil
//   - 讀取失敗時回傳 error（上層可據此更新 UI 狀態）
func ControlReadLoop(ctx context.Context, controlCh io.ReadWriteCloser, onMessage func(CtrlMessage)) error {
	dec := json.NewDecoder(controlCh)
	for {
		var msg CtrlMessage
		if err := dec.Decode(&msg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Debug("control channel read ended", "error", err)
			return err
		}
		onMessage(msg)
	}
}
