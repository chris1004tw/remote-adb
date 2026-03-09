# 開發者指南

## 環境需求

| 項目 | 版本 | 說明 |
|------|------|------|
| Go | >= 1.22 | 編譯和開發所需 |
| ADB | 最新版 | 僅 Agent 端需要（Android Platform Tools） |
| golangci-lint | >= 1.55 | 程式碼品質檢查（可選） |
| Git | >= 2.0 | 版本控制 |

## 快速開始

### 取得原始碼

```bash
git clone https://github.com/chris1004tw/remote-adb.git
cd remote-adb
```

### 建置全部元件

```bash
# 建置所有執行檔
go build ./cmd/...

# 或分別建置
go build -o radb-signal ./cmd/radb-signal
go build -o radb-agent ./cmd/radb-agent
go build -o radb ./cmd/radb
```

### 交叉編譯

```bash
# Windows
GOOS=windows GOARCH=amd64 go build -o radb-agent.exe ./cmd/radb-agent

# Linux
GOOS=linux GOARCH=amd64 go build -o radb-agent ./cmd/radb-agent

# macOS (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o radb-agent ./cmd/radb-agent

# macOS (Intel)
GOOS=darwin GOARCH=amd64 go build -o radb-agent ./cmd/radb-agent
```

## 執行測試

```bash
# 執行所有測試
go test ./...

# 含 race detector
go test -race ./...

# 顯示詳細輸出
go test -v ./...

# 執行特定 package 的測試
go test -v ./internal/adb/...
go test -v ./pkg/protocol/...
```

## 程式碼品質

### Lint

```bash
# 安裝 golangci-lint
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# 執行 lint
golangci-lint run
```

### 格式化

```bash
# 格式化程式碼
gofmt -w .

# 或使用
go fmt ./...
```

## 專案結構

```
remote-adb/
├── cmd/
│   ├── radb/                   # 本機客戶端 (CLI + Daemon)
│   │   └── main.go
│   ├── radb-agent/             # 遠端代理端 (Host PC)
│   │   └── main.go
│   └── radb-signal/            # 信令伺服器
│       └── main.go
│
├── internal/                   # 私有 package（不可被外部 import）
│   ├── adb/                    # ADB 協定通訊與設備鎖定狀態表
│   ├── cli/                    # 終端機互動式選單（bubbletea）
│   ├── daemon/                 # 本機背景服務、Port 路由表、IPC
│   ├── proxy/                  # 透明 TCP 代理與 16KB 切片轉發
│   ├── signal/                 # WebSocket 信令交換與動態主機管理
│   └── webrtc/                 # pion/webrtc PeerConnection 與通道管理
│
├── pkg/                        # 公開 package（可被外部 import）
│   └── protocol/               # 共用的 Signaling JSON 與 IPC 格式定義
│
├── configs/                    # 設定檔範例 (.env)
├── docs/                       # 詳細文件
├── go.mod
├── go.sum
├── Makefile
├── LICENSE
└── README.md
```

## 開發原則

### BDD (Behavior-Driven Development)
- 每項功能先寫測試，再撰寫實作
- 測試命名使用 `Test<功能>_<場景>` 格式
- 範例：`TestDeviceTable_ConcurrentLock`、`TestTracker_Reconnect`

### DRY (Don't Repeat Yourself)
- 共用型別定義集中在 `pkg/protocol/`
- 避免跨 package 複製程式碼

### YAGNI (You Aren't Gonna Need It)
- 不引入不需要的框架（如 viper、cobra）
- 設定優先使用環境變數，夠用就好

### KISS (Keep It Simple, Stupid)
- 優先選擇最簡單的解法
- 避免過度設計和不必要的抽象層

### 型別安全
- 所有函式與方法加上 type hints
- 使用明確的介面定義模組邊界

## 提交規範

- Commit message 使用繁體中文
- 依模組分別提交（如 `feat(adb): 新增設備追蹤器`）
- 提交前確保 `go test -race ./...` 和 `golangci-lint run` 通過

## Makefile 常用指令

```makefile
.PHONY: build test lint clean

build:
	go build ./cmd/...

test:
	go test -race ./...

lint:
	golangci-lint run

clean:
	rm -f radb radb-agent radb-signal
	rm -f radb.exe radb-agent.exe radb-signal.exe

all: lint test build
```
