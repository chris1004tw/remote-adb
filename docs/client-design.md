# 本機開發端設計 (Client UX & Proxy)

為了提供極致的開發者體驗，本機端採用 Daemon 與互動式 CLI 分離的架構。

## 1. 互動式命令列介面 (Interactive CLI)

- 使用 charmbracelet/bubbletea 框架（Elm Architecture）
- 捨棄繁瑣的 flag 輸入
- `radb bind`：互動式選單列出「即時可用主機」→「設備清單 (含鎖定狀態)」→ 一鍵鎖定
- `radb list`：顯示所有已鎖定設備及對應本機 port
- `radb unbind`：釋放指定設備
- `radb status`：查詢當前 daemon 與連線狀態
- CLI 透過 IPC（Named Pipe / Unix Socket）與 Daemon 通訊

## 2. 自動 Port 分配與一對一映射

- `radb daemon` 接管本機 Port 生命週期
- 預設由 `15555` 起算，自動偵測空閒 Port 並遞增分配
- 可透過 `--port-start` flag 指定起始 port
- 內部路由表確保「一個本地 Port 絕對獨立對應一隻遠端手機」
- Port 在設備解鎖後自動回收

## 3. 透明 TCP 代理與切片轉發 (Chunking Proxy)

- 實作雙向資料泵浦 (Data Pump)
- 讀取 TCP 時採用 16KB 固定大小 Buffer (Chunking)
- 避免單次發送超過 SCTP MTU 限制
- 確保大檔案傳輸（`adb push` 100MB+）穩定不中斷
- 使用 pion/webrtc 的 DataChannel detach 模式取得 io.ReadWriteCloser
- 直接做 stream I/O，避開 message-based 限制
- **單連線替換設計**：ADB device transport 是單連線多工協定，同一時間只能有一條 TCP 連線使用共用的 DataChannel。新連線到達時，舊連線會被關閉並等待其寫入器完全結束，才啟動新連線，確保 channel 讀寫不會交錯

## 4. Daemon 架構

- 背景常駐服務，管理所有 WebRTC 連線與 TCP 代理
- IPC server 接收 CLI 指令
- 維護 Binding Table：本機 port <-> 遠端設備的映射
- Graceful shutdown：關閉時逐一釋放所有設備鎖定、關閉 PeerConnection
- Windows 使用 Named Pipe (`\\.\pipe\radb-daemon`)
- Linux/macOS 使用 Unix Domain Socket

## 核心介面

```go
// Proxy 管理單一設備的 TCP 代理
type Proxy interface {
    Start(ctx context.Context, listenPort int, channel io.ReadWriteCloser) error
    Stop() error
    Port() int
}

// PortAllocator 管理 port 分配
type PortAllocator interface {
    Allocate() (int, error)
    Release(port int)
}

// Daemon 本機背景服務
type Daemon interface {
    Start(ctx context.Context) error
    Stop() error
}
```
