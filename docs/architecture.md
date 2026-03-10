# 系統架構設計

## 架構概覽 (Host-Based Routing & Local Daemon)

本專案採用「依照電腦主機轉發」的架構，單一主機可集中管理多支設備。包含三個核心元件：

### 1. Signaling Server (`radb server`)

- 負責 `Host ID` 註冊、WebRTC SDP/ICE 信令交換。
- 採用 **純記憶體動態主機管理 (In-Memory Ephemeral State)**：不持久化儲存歷史紀錄，僅依賴 WebSocket 即時狀態維護可用主機清單。Agent 斷線即刻剔除，確保開發端選單乾淨無干擾。

### 2. Remote Agent (`radb agent`)

- 部署於掛載測試手機的遠端 Host PC。
- **單一執行檔免安裝部署 (Single Binary Drop-and-Run)：** 借助 Go 語言靜態編譯特性，編譯為獨立 `.exe` 或 ELF 檔，隨插即用。

### 3. Local Client (`radb`)

- 部署於開發者本機，分為背景 Daemon 與前端互動式 CLI 兩部分，負責管理本機 Port 與 WebRTC 通道的精準映射。

---

## 核心通訊與安全機制

### WebRTC DataChannel 多工 (Multiplexing)

兩端僅需維持一條 PeerConnection，內部透過動態開啟多條 DataChannel 來隔離不同手機的 TCP 流量，大幅降低連線開銷。預設採用 DTLS 強加密。

**DataChannel Label 命名慣例**：`adb/<serial>/<session_id>`，例如 `adb/ABCD1234/sess-001`。Agent 端可從 label 直接解析出目標設備序號。

### 信令交換格式 (Signaling JSON)

所有 WebSocket 信令使用統一的 Envelope 封裝格式，強制包含：
- `timestamp` -- 防重放攻擊
- `hostname` -- 自動抓取，提升辨識度
- `source_id` / `target_id` -- 精準路由

### 身分驗證 (Token Auth)

初期透過環境變數 `RADB_TOKEN` 傳入靜態的 Pre-Shared Key (PSK) 進行基礎防護，保留未來擴充動態 Token 白名單的能力。

---

## 信令協定格式

### Envelope（所有訊息共用外殼）

```json
{
  "type": "<message_type>",
  "timestamp": "2026-03-09T12:00:00Z",
  "hostname": "chris-dev-pc",
  "source_id": "client-abc123",
  "target_id": "agent-xyz789",
  "payload": { ... }
}
```

### Message Types

| 類別 | Type | 方向 | 說明 |
|------|------|------|------|
| 認證 | `auth` | Client/Agent → Server | 傳送 PSK token 與角色 |
| 認證 | `auth_ack` | Server → Client/Agent | 認證結果與分配的 ID |
| 主機 | `register` | Agent → Server | Host 註冊（含設備列表） |
| 主機 | `unregister` | Agent → Server | Host 取消註冊 |
| 查詢 | `host_list` | Client → Server | 請求可用主機列表 |
| 查詢 | `host_list_resp` | Server → Client | 回傳主機列表 |
| 設備 | `device_update` | Agent → Server → Client | 設備狀態即時更新 |
| 鎖定 | `lock_req` | Client → Server → Agent | 請求鎖定設備 |
| 鎖定 | `lock_resp` | Agent → Server → Client | 鎖定結果 |
| 解鎖 | `unlock_req` | Client → Server → Agent | 請求解鎖設備 |
| 解鎖 | `unlock_resp` | Agent → Server → Client | 解鎖結果 |
| WebRTC | `offer` | Client → Server → Agent | SDP Offer |
| WebRTC | `answer` | Agent → Server → Client | SDP Answer |
| WebRTC | `candidate` | 雙向 | ICE Candidate |
| 錯誤 | `error` | 任意方向 | 錯誤通知 |

### 錯誤碼定義

| 錯誤碼 | 含義 |
|--------|------|
| 4001 | 設備已被鎖定 |
| 4002 | 設備不存在 |
| 4003 | 主機不存在 |
| 4004 | 認證失敗 |
| 4005 | 信令轉發失敗（目標離線） |
| 5001 | 內部伺服器錯誤 |

---

## 模組依賴關係

```
pkg/protocol（零依賴，所有模組共用）
    ↓
internal/signal     internal/adb     internal/webrtc（可平行開發）
    ↓                   ↓                 ↓
                internal/proxy
                    ↓
                internal/daemon
                    ↓
                internal/cli     internal/gui（adb + webrtc + proxy 整合）
                    ↓                 ↓
          cmd/radb（統一入口）
```

---

## 技術選型

| 項目 | 選擇 | 理由 |
|------|------|------|
| WebSocket | `coder/websocket` | 原生 context.Context 支援、並行寫入安全、持續維護 |
| WebRTC | `pion/webrtc` v4 | Go 生態唯一成熟的純 Go 實作，支援 DataChannel detach 模式 |
| CLI | `charmbracelet/bubbletea` + `lipgloss` | Elm Architecture 天然適合即時更新 UI |
| 日誌 | `log/slog`（Go 1.21+ 內建） | 結構化日誌、不需外部依賴 |
| 設定 | 環境變數 | 符合 YAGNI，不引入 viper |
| Daemon IPC | Named Pipe (Win) / Unix Socket (Linux/macOS) | build tag 分離實作 |
