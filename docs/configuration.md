# 設定指南

radb 的三個元件各有獨立的設定方式，統一透過環境變數和 CLI flag 配置。

## 通用設定

### 環境變數

| 環境變數 | 必填 | 預設值 | 說明 |
|---------|------|--------|------|
| `RADB_TOKEN` | 是 | — | Pre-Shared Key (PSK) 驗證 Token，三個元件需設定相同值 |

## Signal Server (`radb-signal`)

### CLI Flags

| Flag | 環境變數 | 預設值 | 說明 |
|------|---------|--------|------|
| `--port` | `RADB_SIGNAL_PORT` | `8080` | HTTP/WebSocket 監聽埠 |
| `--host` | `RADB_SIGNAL_HOST` | `0.0.0.0` | 監聽地址 |

### 啟動範例

```bash
# 最簡啟動
RADB_TOKEN=my-secret radb-signal

# 指定 port
radb-signal --port 9090 --token my-secret
```

## Remote Agent (`radb-agent`)

### CLI Flags

| Flag | 環境變數 | 預設值 | 說明 |
|------|---------|--------|------|
| `--signal` | `RADB_SIGNAL_URL` | `ws://localhost:8080` | Signal Server WebSocket 位址 |
| `--token` | `RADB_TOKEN` | — | PSK Token |
| `--host-id` | `RADB_HOST_ID` | (自動偵測主機名稱) | 註冊在 Signal Server 上的主機識別名稱 |
| `--adb-port` | `RADB_ADB_PORT` | `5037` | 本機 ADB server 埠 |
| `--stun` | `RADB_STUN_URLS` | `stun:stun.l.google.com:19302` | STUN Server URL（多個以逗號分隔） |
| `--turn` | `RADB_TURN_URL` | — | TURN Server URL |
| `--turn-user` | `RADB_TURN_USER` | — | TURN 使用者名稱 |
| `--turn-pass` | `RADB_TURN_PASS` | — | TURN 密碼 |

### 啟動範例

```bash
# 基本啟動
RADB_TOKEN=my-secret radb-agent --signal ws://signal.example.com:8080

# 指定 Host ID + TURN
radb-agent \
  --signal ws://signal.example.com:8080 \
  --token my-secret \
  --host-id lab-pc-01 \
  --turn turn:turn.example.com:3478 \
  --turn-user admin \
  --turn-pass password
```

## Local Client (`radb`)

### Daemon Flags

| Flag | 環境變數 | 預設值 | 說明 |
|------|---------|--------|------|
| `--signal` | `RADB_SIGNAL_URL` | `ws://localhost:8080` | Signal Server WebSocket 位址 |
| `--token` | `RADB_TOKEN` | — | PSK Token |
| `--port-start` | `RADB_PORT_START` | `15555` | 自動分配的起始 Port |
| `--stun` | `RADB_STUN_URLS` | `stun:stun.l.google.com:19302` | STUN Server URL |
| `--turn` | `RADB_TURN_URL` | — | TURN Server URL |
| `--turn-user` | `RADB_TURN_USER` | — | TURN 使用者名稱 |
| `--turn-pass` | `RADB_TURN_PASS` | — | TURN 密碼 |

### CLI 子命令

| 子命令 | 說明 |
|--------|------|
| `radb daemon` | 啟動背景 Daemon 服務 |
| `radb bind` | 互動式選擇主機與設備，鎖定並建立代理 |
| `radb unbind <port>` | 釋放指定 port 的設備綁定 |
| `radb list` | 列出所有已綁定的設備及對應 port |
| `radb status` | 查詢 daemon 與連線狀態 |

### 使用範例

```bash
# 啟動 daemon
RADB_TOKEN=my-secret radb daemon --signal ws://signal.example.com:8080

# 互動式綁定設備（另開終端）
radb bind

# 查看已綁定設備
radb list

# 釋放設備
radb unbind 15555
```

## STUN/TURN 設定說明

### STUN (Session Traversal Utilities for NAT)

STUN 用於 NAT 穿透，讓兩端可以發現自己的公網 IP 和 Port。預設使用 Google 的公開 STUN server。

- 在大多數家用/辦公室網路環境（錐形 NAT）下，僅需 STUN 即可建立 P2P 連線
- **免費且無頻寬限制**（STUN 只在連線建立階段使用）

### TURN (Traversal Using Relays around NAT)

當兩端都在**對稱型 NAT**後方時，STUN 無法穿透，此時需要 TURN server 作為中繼。

- TURN 會中繼所有流量，**有頻寬成本**
- 建議自建 TURN server（可使用 `pion/turn` 或 `coturn`）
- 若不設定 TURN，在對稱型 NAT 環境下將無法連線

### 判斷是否需要 TURN

| 網路環境 | 需要 TURN？ |
|---------|------------|
| 兩端在同一區域網路 | 不需要 |
| 一端有公網 IP | 不需要 |
| 兩端都在家用路由器後方 | 通常不需要（錐形 NAT） |
| 企業防火牆 / 對稱型 NAT | **需要** |
| 行動網路 (4G/5G) | 可能需要 |

## .env 範例檔

```bash
# configs/.env.example

# 通用
RADB_TOKEN=change-me-to-a-strong-secret

# Signal Server
RADB_SIGNAL_PORT=8080

# Agent / Client
RADB_SIGNAL_URL=ws://signal.example.com:8080
RADB_STUN_URLS=stun:stun.l.google.com:19302,stun:stun1.l.google.com:19302

# TURN（僅在對稱型 NAT 環境需要）
# RADB_TURN_URL=turn:turn.example.com:3478
# RADB_TURN_USER=admin
# RADB_TURN_PASS=password

# Agent 專用
# RADB_HOST_ID=lab-pc-01
# RADB_ADB_PORT=5037

# Client 專用
# RADB_PORT_START=15555
```
