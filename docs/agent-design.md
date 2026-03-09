# 遠端被控端設計 (Agent Device Management)

`Remote Agent` 扮演遠端主機的資源管理員，嚴格管控設備存取權限並防止指令衝突。

包含以下四大機制：

---

## 1. 即時硬體監聽 (ADB Server Protocol)

- 不依賴 `adb devices` 輪詢
- 直連本地 `127.0.0.1:5037`，使用 `host:track-devices` 協定指令
- 建立長連線即時接收硬體插拔與狀態變更事件
- ADB protocol 格式：4 位 hex 長度前綴 + payload（`<hex4><serial>\t<state>\n`）
- 斷線時使用指數退避重連策略（初始 1s，最大 30s）

## 2. 執行緒安全的狀態表 (Thread-Safe Map)

- 維護以設備序號 (Serial Number) 為鍵值的狀態表
- 記錄硬體狀態 (`device`/`offline`) 與鎖定狀態 (`Available`/`Locked`)
- 實作 Mutex 互斥鎖避免 Race Condition
- Lock/Unlock 操作需攜帶 clientID，確保只有持鎖者可解鎖

## 3. 異常斷線防呆機制 (Graceful Teardown)

- 深度綁定 WebRTC DataChannel 的 OnClose 事件
- 綁定 PeerConnection 的 OnConnectionStateChange
- 偵測到開發端異常斷線時，強制切斷對應的 ADB TCP Socket
- 自動釋放設備鎖定，防止資源永久卡死
- 額外 TTL 計時器（60 秒無心跳強制釋放），作為最後防線

## 4. ADB TCP 轉發流程

- 收到 Client 的 lock_req 並確認設備可用後，鎖定設備
- 等待 WebRTC DataChannel 建立（label 格式：`adb/<serial>/<session_id>`）
- 從 label 解析目標設備序號
- 使用 ADB protocol: `host:transport:<serial>` 切換到目標設備
- 接著發送 `tcp:5555`（或指定 port）建立 TCP tunnel
- 啟動雙向 data pump：DataChannel <-> ADB TCP socket

---

## 核心介面

```go
// Tracker 即時追蹤 ADB server 上的設備變化
type Tracker interface {
    Track(ctx context.Context) (<-chan []DeviceEvent, error)
    Close() error
}

// DeviceTable 執行緒安全的設備狀態表
type DeviceTable interface {
    Update(devices []DeviceEvent)
    List() []DeviceInfo
    Lock(serial string, clientID string) bool
    Unlock(serial string, clientID string) bool
    UnlockAll(clientID string)
}
```
