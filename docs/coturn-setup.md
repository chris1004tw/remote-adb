# coturn TURN Server 架設指南

當 Agent 與 Client 雙方都在**對稱型 NAT**（Symmetric NAT）後方時，STUN 無法完成穿透，需要 TURN server 作為中繼。[coturn](https://github.com/coturn/coturn) 是最廣泛使用的開源 TURN/STUN 實作。

> 判斷是否需要 TURN，請參閱 [設定指南 — STUN/TURN 說明](configuration.md#stunturn-設定說明)。

---

## 安裝

### Ubuntu / Debian

```bash
sudo apt update && sudo apt install coturn
```

### CentOS / RHEL

```bash
sudo yum install coturn
```

### Docker

```bash
docker run -d --network=host coturn/coturn \
  -n --realm=your-domain.com \
  --user=radb:your-secret-password \
  --external-ip=YOUR_PUBLIC_IP/YOUR_PRIVATE_IP
```

---

## 設定

編輯 `/etc/turnserver.conf`：

```ini
# === 監聽 ===
listening-ip=0.0.0.0
listening-port=3478

# TLS 端口（生產環境建議啟用）
# tls-listening-port=5349
# cert=/etc/ssl/certs/turn.pem
# pkey=/etc/ssl/private/turn.key

# === 外部 IP（NAT 後方必填）===
# 格式：公網IP/內網IP
external-ip=YOUR_PUBLIC_IP/YOUR_PRIVATE_IP

# === 認證 ===
realm=your-domain.com

# 方式一：長期憑證（簡單，適合測試與小規模使用）
lt-cred-mech
user=radb:your-secret-password

# 方式二：HMAC 臨時憑證（更安全，適合生產環境）
# use-auth-secret
# static-auth-secret=YOUR_LONG_RANDOM_SECRET

# === Relay ===
min-port=49152
max-port=65535

# === 日誌 ===
log-file=/var/log/turnserver.log

# === 安全性 ===
no-multicast-peers
```

### 重要參數說明

| 參數 | 說明 |
|------|------|
| `external-ip` | **必填**。NAT 後方不設定會導致 relay candidate 無法使用 |
| `lt-cred-mech` | 長期憑證模式，帳密固定寫在設定檔中 |
| `use-auth-secret` | HMAC 臨時憑證模式，搭配 `static-auth-secret` 動態產生短期帳密 |
| `min-port` / `max-port` | relay 使用的 UDP 端口範圍，防火牆需放行 |

### 安全性注意事項

coturn 預設會允許 relay 到任何 IP，包括私有網段。如果 TURN server 部署在公網，**建議封鎖私有網段**以防止內網探測攻擊：

```ini
denied-peer-ip=10.0.0.0-10.255.255.255
denied-peer-ip=172.16.0.0-172.31.255.255
denied-peer-ip=192.168.0.0-192.168.255.255
```

> **例外**：如果 radb-agent 與 TURN server 在同一內網，需將該網段從 `denied-peer-ip` 中移除，否則中繼流量無法到達 Agent。

---

## 啟動

```bash
# 啟用 coturn 服務（Ubuntu 需先設定 /etc/default/coturn 中的 TURNSERVER_ENABLED=1）
sudo systemctl enable coturn
sudo systemctl start coturn

# 檢查狀態
sudo systemctl status coturn
```

---

## 防火牆設定

```bash
# STUN/TURN 主端口
sudo ufw allow 3478/tcp
sudo ufw allow 3478/udp

# TLS 端口（如有啟用）
sudo ufw allow 5349/tcp
sudo ufw allow 5349/udp

# Relay UDP 端口範圍
sudo ufw allow 49152:65535/udp
```

若使用雲端服務商（AWS / GCP / Azure），需在安全群組中同步放行上述端口。

---

## 驗證

### 使用 turnutils_uclient

coturn 自帶的測試工具：

```bash
turnutils_uclient -u radb -w your-secret-password YOUR_PUBLIC_IP
```

### 使用 Trickle ICE 網頁測試

開啟 [Trickle ICE](https://webrtc.github.io/samples/src/content/peerconnection/trickle-ice/)，輸入：

- **STUN or TURN URI**：`turn:YOUR_PUBLIC_IP:3478`
- **TURN username**：`radb`
- **TURN password**：`your-secret-password`

點選 Gather candidates，若出現 `relay` 類型的 candidate 即代表 TURN server 正常運作。

---

## 整合到 radb

設定 radb-agent 和 radb daemon 的 TURN 參數：

```bash
# Agent
radb-agent \
  --signal ws://signal.example.com:8080 \
  --turn turn:YOUR_PUBLIC_IP:3478 \
  --turn-user radb \
  --turn-pass your-secret-password

# Client Daemon
radb daemon \
  --signal ws://signal.example.com:8080 \
  --turn turn:YOUR_PUBLIC_IP:3478 \
  --turn-user radb \
  --turn-pass your-secret-password
```

或透過環境變數：

```bash
export RADB_TURN_URL=turn:YOUR_PUBLIC_IP:3478
export RADB_TURN_USER=radb
export RADB_TURN_PASS=your-secret-password
```

---

## 常見問題

**Q: 沒有出現 relay candidate？**
A: 檢查 `external-ip` 是否正確設定，以及防火牆是否放行 3478 和 relay 端口範圍。

**Q: TURN 連線建立但傳輸很慢？**
A: TURN 中繼所有流量都經過伺服器，受限於伺服器頻寬。考慮使用更高頻寬的主機，或在可行時讓連線走 STUN 直連。

**Q: 如何支援多人同時使用？**
A: coturn 支援多使用者，可在設定檔中加入多組 `user=name:password`，或使用 `use-auth-secret` 模式由應用程式動態產生臨時帳密。

**Q: 生產環境需要 TLS 嗎？**
A: 建議啟用。設定 `tls-listening-port=5349` 並配置 SSL 憑證，使用 `turns:` URI scheme 連線，可防止中間人攻擊。此時 radb 的 TURN URL 改為 `turns:YOUR_PUBLIC_IP:5349`。
