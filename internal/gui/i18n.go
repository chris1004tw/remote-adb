// i18n.go 實作 GUI 的多語系支援機制。
//
// 使用 atomic.Pointer 儲存當前語言的 Messages 結構，
// Gio immediate-mode 下一幀自動生效，不需重啟。
//
// 支援語言：繁體中文（zh-TW）、英文（en）。
// 語言偵測優先順序：設定檔 → 系統語系自動偵測 → 預設繁中。
//
// 相關文件：.claude/CLAUDE.md「專案概述 — GUI」
package gui

import "sync/atomic"

// --- 語言代碼常數 ---

const (
	LangAuto = "" // 自動偵測
	LangZhTW = "zh-TW"
	LangEN   = "en"
)

// --- Messages 結構 ---

// Messages 是所有 UI 可見文字的集合。
// 依元件分組為巢狀結構，方便維護與查詢。
type Messages struct {
	App      appMsg
	Common   commonMsg
	Pair     pairMsg
	LAN      lanMsg
	Signal   signalMsg
	Settings settingsMsg
}

// appMsg 是應用程式層級的文字（視窗標題、分頁名稱）。
type appMsg struct {
	WindowTitle string // 主視窗標題
	TabPair     string // 簡易連線分頁
	TabLAN      string // 區網直連分頁
	TabSignal   string // Relay 伺服器分頁
	GearTooltip string // 齒輪按鈕 tooltip
}

// commonMsg 是跨分頁共用的文字。
type commonMsg struct {
	Controller        string // 主控端
	Agent             string // 被控端
	StatusPrefix      string // "狀態: "
	DevicesFmt        string // "設備 (%d):"
	Stopped           string // "已停止"
	Disconnected      string // "未連線"
	CheckingADB       string // "檢查 ADB..."
	ADBErrorFmt       string // "ADB 錯誤: %v"
	ErrorFmt          string // "錯誤: %v"
	RunningFmt        string // "運行中（port %d）"
	StartServer       string // "啟動伺服器"
	StopServer        string // "停止伺服器"
	Connect           string // "連線"
	DisconnectBtn     string // "中斷連線"
	TokenLabel        string // "Token:"
	TokenHintOptional string // "（可選）"
	TokenHintPSK      string // "PSK 認證 Token"
	Connecting        string // "連線中..."
	RelayNotice       string // TURN 中繼通知訊息
}

// pairMsg 是「簡易連線」分頁的文字。
type pairMsg struct {
	// 按鈕
	GenerateOffer     string // "產生邀請碼"
	GenerateOfferFast string // "立即產生邀請碼（有無法連接的風險）"
	ClearBtn          string // "清除邀請碼 / 回應碼"
	DisconnectBtn     string // "結束連線"
	ReconnectADB      string // "重新添加遠端 ADB 設備到本機"
	RefreshDevices    string // "重新偵測被控端 ADB 設備"

	// 標籤 / Hint
	OfferOutLabel  string // "邀請碼（已複製到剪貼簿，僅限使用一次）:"
	AnswerInLabel  string // "回應碼（貼入後自動連線）:"
	AnswerInHint   string // "貼入對方給的回應碼"
	OfferInLabel   string // "邀請碼（貼入後自動處理）:"
	OfferInHint    string // "貼入對方給的邀請碼"
	AnswerOutLabel string // "回應碼（已複製到剪貼簿，僅限使用一次）:"
	RemoteHost     string // "遠端主機: "
	RemoteDevFmt   string // "遠端設備 (%d):"
	LatencyFmt     string // "延遲: %d ms"

	// 狀態
	StatusNotStarted      string // "未開始"
	StatusGenerating      string // "正在產生邀請碼..."
	StatusPreparingTURN   string // "正在準備網路中繼..."
	StatusCreatingPC      string // "正在建立連線..."
	StatusCreatingOffer   string // "正在尋找最佳連線路徑，請稍候..."
	StatusEncodingOffer   string // "正在產生邀請碼..."
	StatusOfferReady      string // "邀請碼已產生（已複製到剪貼簿）"
	StatusPleaseGenerate  string // "請先產生邀請碼"
	StatusPleaseAnswer    string // "請貼入對方的回應碼"
	StatusPleaseOffer     string // "請貼入對方的邀請碼"
	StatusProcessing      string // "正在處理邀請碼..."
	StatusDecodingOffer   string // "正在解析邀請碼..."
	StatusCreatingAnswer  string // "正在尋找最佳連線路徑，請稍候..."
	StatusEncodingAnswer  string // "正在產生回應碼..."
	StatusAnswerReady     string // "回應碼已產生（已複製到剪貼簿）"
	StatusP2PConnecting   string // "正在嘗試與對方建立連線..."
	StatusP2PConnected      string // "點對點直連，已連線"
	StatusRelayConnected    string // "透過中繼伺服器已連線"
	StatusP2PDisconnected   string // "點對點直連，已斷線"
	StatusP2PProxyFmt       string // "點對點直連，已連線，ADB Proxy: 127.0.0.1:%d"
	StatusP2PDevicesProxy   string // "點對點直連，已連線，遠端 %d 個設備（ADB Proxy: 127.0.0.1:%d）"
	StatusP2PWaiting        string // "點對點直連，已連線，被控端無設備"
	StatusRelayWaiting      string // "透過中繼伺服器，已連線，被控端無設備"
	StatusP2PDevicesFmt     string // "點對點直連，已連線，%d 個設備"
	StatusRelayDevicesFmt   string // "透過中繼伺服器，已連線，%d 個設備"
	StatusControlClosed   string // "control channel 已關閉"

	// 錯誤（格式字串，用 fmt.Sprintf 填入）
	ErrCreatePCFmt      string // "建立 PeerConnection 失敗: %v"
	ErrCreateCtrlChFmt  string // "建立 control channel 失敗: %v"
	ErrCreateOfferFmt   string // "建立 Offer 失敗: %v"
	ErrEncodeOfferFmt   string // "編碼邀請碼失敗: %v"
	ErrInvalidAnswerFmt string // "無效回應碼: %v"
	ErrHandleAnswerFmt  string // "處理回應碼失敗: %v"
	ErrProxyListenerFmt string // "建立 proxy listener 失敗: %v"
	ErrInvalidOfferFmt  string // "無效邀請碼: %v"
	ErrHandleOfferFmt   string // "處理 Offer 失敗: %v"
	ErrEncodeAnswerFmt  string // "編碼回應碼失敗: %v"

	// 警告
	WarnTURNUnavailable string // "Cloudflare 中繼服務無法使用，若網路環境受限可能無法連線"
}

// lanMsg 是「區網直連」分頁的文字。
type lanMsg struct {
	// 按鈕
	ScanLAN  string // "掃描 LAN"
	Scanning string // "掃描中..."

	// 標籤
	AgentAddr      string // "Agent 地址:"
	AgentsFoundFmt string // "發現 %d 個 Agent:"
	ProxyDevFmt    string // "ADB Proxy: 127.0.0.1:%d（%d 個設備）:"

	// 狀態
	StatusDisconnected    string // "已中斷"
	StatusPleaseAddr      string // "請填入 Agent 地址"
	StatusQuerying        string // "查詢設備中..."
	StatusNoDevices       string // "Agent 上沒有可用設備"
	StatusConnectedFmt    string // "已連線，ADB Proxy: 127.0.0.1:%d"
	StatusConnectedDevFmt string // "已連線，%d 個設備"

	// 錯誤
	ErrProxyFmt        string // "建立 proxy 失敗: %v"
	ErrConnectFmt      string // "連線失敗: %v"
	ErrSendFmt         string // "發送失敗: %v"
	ErrReadFmt         string // "讀取失敗: %v"
	ErrQueryFmt        string // "查詢失敗: %s"
	ErrDialAgentFmt    string // "連線 Agent 失敗: %v"
	ErrSendRequestFmt  string // "發送請求失敗: %v"
	ErrReadResponseFmt string // "讀取回應失敗: %v"
}

// signalMsg 是「Relay 伺服器」分頁的文字。
type signalMsg struct {
	// 按鈕 / 標籤
	Server        string // "伺服器"
	StartAgent    string // "啟動被控端"
	StopAgent     string // "停止被控端"
	ConnectServer string // "連線到伺服器"
	HostnameLabel string // "主機名稱:"
	HostnameHint  string // "自動偵測"
	HostsFmt      string // "主機 (%d):"
	HostDevFmt    string // "%d 設備"
	Locked        string // "(已鎖定)"
	BindingsFmt   string // "已綁定 (%d):"
	BindLabel     string // "[Bind]"
	UnbindLabel   string // "[解綁]"

	// 狀態
	StatusPleaseToken    string // "請輸入 Token"
	StatusPleaseURLToken string // "請輸入 Server URL 和 Token"
	StatusRunning        string // "運行中"
	StatusConnected      string // "已連線"
	StatusDisconnected   string // "已斷線"
	StatusBindOKFmt         string // "綁定成功 127.0.0.1:%d → %s"
	StatusBindFailFmt       string // "綁定失敗: %s"
	StatusBindDecodeFailFmt string // "解碼綁定結果失敗: %v"
	StatusUnbindOKFmt       string // "已解綁 port %d"
	StatusUnbindFailFmt     string // "解綁失敗: %s"

	// 錯誤
	ErrIPCFmt      string // "建立 IPC 失敗: %v"
	ErrDaemonFmt   string // "Daemon 錯誤: %v"
	ErrIPCNotReady string // "IPC 未就緒"
}

// settingsMsg 是設定面板的文字。
type settingsMsg struct {
	Title              string // "設定"
	ConnectionSection  string // "連線設定"
	SaveBtn            string // "儲存設定"
	CloseBtn           string // "關閉"
	AboutSection       string // "關於"
	CurrentVerFmt      string // "目前版本：%s"
	LatestVerFmt       string // "最新版本：%s"
	CheckUpdate        string // "檢查更新"
	Checking           string // "檢查中..."
	Updating           string // "更新中..."
	UpdateNow          string // "立即更新"
	BannerNewVerFmt    string // "新版本 %s 可用"
	BannerDismiss      string // "稍後再說"
	ConnModeLabel      string // "連線方式"
	ConnModeDirectFirst string // "直連優先（預設）"
	ConnModeDirectOnly string // "僅直連"
	ConnModeRelayOnly  string // "僅中繼"
	STUNLabel          string // "NAT 探測伺服器"
	CustomStun         string // "自訂"
	CustomStunOption   string // "自訂..."
	TURNModeLabel      string // "中繼伺服器"
	TURNModeCloudflare string // "Cloudflare（免費）"
	TURNModeCustom     string // "自訂"
	TURNLabel          string // "中繼位址"
	TURNHint           string // "turn:your.server.com:3478"
	TURNUserLabel      string // "中繼帳號"
	TURNPassLabel      string // "中繼密碼"
	LanguageLabel      string // "語言"
	LanguageAuto       string // "自動"

	// 更新狀態
	StatusChecking       string // "正在檢查更新..."
	StatusCheckFailFmt   string // "檢查失敗：%v"
	StatusUpdateAvailFmt string // "有新版本可用：%s → %s"
	StatusUpToDate       string // "已是最新版本"
	StatusDownloading    string // "正在下載更新..."
	StatusUpdateFailFmt      string // "更新失敗：%v"
	StatusUpdatedFmt         string // "已更新至 %s，正在重新啟動..."
	StatusRestartFailFmt     string // "重啟失敗：%v"
	StatusUpdatePendingRestart string // "更新已下載，結束連線後將自動重啟"
}

// --- 全域語言狀態 ---

var currentMessages atomic.Pointer[Messages]

// msg 回傳當前語言的 Messages。
// 由 UI 程式碼在每一幀呼叫，atomic 讀取保證執行緒安全。
func msg() *Messages {
	return currentMessages.Load()
}

// SetLanguage 切換介面語言。
// lang 為 LangZhTW 或 LangEN；空字串觸發自動偵測。
func SetLanguage(lang string) {
	if lang == LangAuto {
		lang = detectSystemLanguage()
	}
	switch lang {
	case LangEN:
		currentMessages.Store(&messagesEN)
	default:
		currentMessages.Store(&messagesZhTW)
	}
}

// currentLangCode 回傳目前生效的語言代碼。
// 用於設定面板顯示當前語言。
func currentLangCode() string {
	m := currentMessages.Load()
	if m == &messagesEN {
		return LangEN
	}
	return LangZhTW
}

func init() {
	currentMessages.Store(&messagesZhTW)
}

// --- 繁體中文 ---

var messagesZhTW = Messages{
	App: appMsg{
		WindowTitle: "遠端 ADB 工具",
		TabPair:     "P2P 直連",
		TabLAN:      "區網直連",
		TabSignal:   "Relay 伺服器",
		GearTooltip: "設定",
	},
	Common: commonMsg{
		Controller:        "主控端",
		Agent:             "被控端",
		StatusPrefix:      "狀態: ",
		DevicesFmt:        "設備 (%d):",
		Stopped:           "已停止",
		Disconnected:      "未連線",
		CheckingADB:       "檢查 ADB...",
		ADBErrorFmt:       "ADB 錯誤: %v",
		ErrorFmt:          "錯誤: %v",
		RunningFmt:        "運行中（port %d）",
		StartServer:       "啟動伺服器",
		StopServer:        "停止伺服器",
		Connect:           "連線",
		DisconnectBtn:     "中斷連線",
		TokenLabel:        "Token:",
		TokenHintOptional: "（可選）",
		TokenHintPSK:      "PSK 認證 Token",
		Connecting:        "連線中...",
		RelayNotice:       "因主控或被控端的網路環境受限，目前透過 Cloudflare 中繼伺服器連線",
	},
	Pair: pairMsg{
		GenerateOffer:     "產生邀請碼",
		GenerateOfferFast: "立即產生邀請碼（有無法連接的風險）",
		ClearBtn:          "清除邀請碼 / 回應碼",
		DisconnectBtn:     "結束連線",
		ReconnectADB:      "重新添加遠端 ADB 設備到本機",
		RefreshDevices:    "重新偵測被控端 ADB 設備",

		OfferOutLabel:  "邀請碼（已複製到剪貼簿，僅限使用一次）:",
		AnswerInLabel:  "回應碼（貼入後自動連線）:",
		AnswerInHint:   "貼入對方給的回應碼",
		OfferInLabel:   "邀請碼（貼入後自動處理）:",
		OfferInHint:    "貼入對方給的邀請碼",
		AnswerOutLabel: "回應碼（已複製到剪貼簿，僅限使用一次）:",
		RemoteHost:     "遠端主機: ",
		RemoteDevFmt:   "遠端設備 (%d):",
		LatencyFmt:     "延遲: %d ms",

		StatusNotStarted:      "未開始",
		StatusGenerating:      "正在產生邀請碼...",
		StatusPreparingTURN:   "正在準備網路中繼...",
		StatusCreatingPC:      "正在建立連線...",
		StatusCreatingOffer:   "正在尋找最佳連線路徑，請稍候...",
		StatusEncodingOffer:   "正在產生邀請碼...",
		StatusOfferReady:      "邀請碼已產生（已複製到剪貼簿）",
		StatusPleaseGenerate:  "請先產生邀請碼",
		StatusPleaseAnswer:    "請貼入對方的回應碼",
		StatusPleaseOffer:     "請貼入對方的邀請碼",
		StatusProcessing:      "正在處理邀請碼...",
		StatusDecodingOffer:   "正在解析邀請碼...",
		StatusCreatingAnswer:  "正在尋找最佳連線路徑，請稍候...",
		StatusEncodingAnswer:  "正在產生回應碼...",
		StatusAnswerReady:     "回應碼已產生（已複製到剪貼簿）",
		StatusP2PConnecting:   "正在嘗試與對方建立連線...",
		StatusP2PConnected:    "點對點直連，已連線",
		StatusRelayConnected:  "透過中繼伺服器，已連線",
		StatusP2PDisconnected: "點對點直連，已斷線",
		StatusP2PProxyFmt:     "點對點直連，已連線，ADB Proxy: 127.0.0.1:%d",
		StatusP2PDevicesProxy: "點對點直連，已連線，遠端 %d 個設備（ADB Proxy: 127.0.0.1:%d）",
		StatusP2PWaiting:      "點對點直連，已連線，被控端無設備",
		StatusRelayWaiting:    "透過中繼伺服器，已連線，被控端無設備",
		StatusP2PDevicesFmt:   "點對點直連，已連線，%d 個設備",
		StatusRelayDevicesFmt: "透過中繼伺服器，已連線，%d 個設備",
		StatusControlClosed:   "control channel 已關閉",

		ErrCreatePCFmt:      "建立 PeerConnection 失敗: %v",
		ErrCreateCtrlChFmt:  "建立 control channel 失敗: %v",
		ErrCreateOfferFmt:   "建立 Offer 失敗: %v",
		ErrEncodeOfferFmt:   "編碼邀請碼失敗: %v",
		ErrInvalidAnswerFmt: "無效回應碼: %v",
		ErrHandleAnswerFmt:  "處理回應碼失敗: %v",
		ErrProxyListenerFmt: "建立 proxy listener 失敗: %v",
		ErrInvalidOfferFmt:  "無效邀請碼: %v",
		ErrHandleOfferFmt:   "處理 Offer 失敗: %v",
		ErrEncodeAnswerFmt:  "編碼回應碼失敗: %v",

		WarnTURNUnavailable: "Cloudflare 中繼服務無法使用，若網路環境受限可能無法連線",
	},
	LAN: lanMsg{
		ScanLAN:  "掃描 LAN",
		Scanning: "掃描中...",

		AgentAddr:      "Agent 地址:",
		AgentsFoundFmt: "發現 %d 個 Agent:",
		ProxyDevFmt:    "ADB Proxy: 127.0.0.1:%d（%d 個設備）:",

		StatusDisconnected:    "已中斷",
		StatusPleaseAddr:      "請填入 Agent 地址",
		StatusQuerying:        "查詢設備中...",
		StatusNoDevices:       "Agent 上沒有可用設備",
		StatusConnectedFmt:    "已連線，ADB Proxy: 127.0.0.1:%d",
		StatusConnectedDevFmt: "已連線，%d 個設備",

		ErrProxyFmt:        "建立 proxy 失敗: %v",
		ErrConnectFmt:      "連線失敗: %v",
		ErrSendFmt:         "發送失敗: %v",
		ErrReadFmt:         "讀取失敗: %v",
		ErrQueryFmt:        "查詢失敗: %s",
		ErrDialAgentFmt:    "連線 Agent 失敗: %v",
		ErrSendRequestFmt:  "發送請求失敗: %v",
		ErrReadResponseFmt: "讀取回應失敗: %v",
	},
	Signal: signalMsg{
		Server:        "伺服器",
		StartAgent:    "啟動被控端",
		StopAgent:     "停止被控端",
		ConnectServer: "連線到伺服器",
		HostnameLabel: "主機名稱:",
		HostnameHint:  "自動偵測",
		HostsFmt:      "主機 (%d):",
		HostDevFmt:    "%d 設備",
		Locked:        "(已鎖定)",
		BindingsFmt:   "已綁定 (%d):",
		BindLabel:     "[Bind]",
		UnbindLabel:   "[解綁]",

		StatusPleaseToken:    "請輸入 Token",
		StatusPleaseURLToken: "請輸入 Server URL 和 Token",
		StatusRunning:        "運行中",
		StatusConnected:      "已連線",
		StatusDisconnected:   "已斷線",
		StatusBindOKFmt:         "綁定成功 127.0.0.1:%d → %s",
		StatusBindFailFmt:       "綁定失敗: %s",
		StatusBindDecodeFailFmt: "解碼綁定結果失敗: %v",
		StatusUnbindOKFmt:       "已解綁 port %d",
		StatusUnbindFailFmt:     "解綁失敗: %s",

		ErrIPCFmt:      "建立 IPC 失敗: %v",
		ErrDaemonFmt:   "Daemon 錯誤: %v",
		ErrIPCNotReady: "IPC 未就緒",
	},
	Settings: settingsMsg{
		Title:              "設定",
		ConnectionSection:  "連線設定",
		SaveBtn:            "儲存設定",
		CloseBtn:           "關閉",
		AboutSection:       "關於",
		CurrentVerFmt:      "目前版本：%s",
		LatestVerFmt:       "最新版本：%s",
		CheckUpdate:        "檢查更新",
		Checking:           "檢查中...",
		Updating:           "更新中...",
		UpdateNow:          "立即更新",
		BannerNewVerFmt:    "新版本 %s 可用",
		BannerDismiss:      "稍後再說",
		ConnModeLabel:       "連線方式",
		ConnModeDirectFirst: "直連優先（預設）",
		ConnModeDirectOnly: "僅直連",
		ConnModeRelayOnly:  "僅中繼",
		STUNLabel:          "NAT 探測伺服器",
		CustomStun:         "自訂",
		CustomStunOption:   "自訂...",
		TURNModeLabel:      "中繼伺服器",
		TURNModeCloudflare: "Cloudflare（免費）",
		TURNModeCustom:     "自訂...",
		TURNLabel:          "中繼位址",
		TURNHint:           "turn:your.server.com:3478",
		TURNUserLabel:      "中繼帳號",
		TURNPassLabel:      "中繼密碼",
		LanguageLabel:      "語言",
		LanguageAuto:       "自動",

		StatusChecking:       "正在檢查更新...",
		StatusCheckFailFmt:   "檢查失敗：%v",
		StatusUpdateAvailFmt: "有新版本可用：%s → %s",
		StatusUpToDate:       "已是最新版本",
		StatusDownloading:    "正在下載更新...",
		StatusUpdateFailFmt:  "更新失敗：%v",
		StatusUpdatedFmt:     "已更新至 %s，正在重新啟動...",
		StatusRestartFailFmt:       "重啟失敗：%v",
		StatusUpdatePendingRestart: "更新已下載，結束連線後將自動重啟",
	},
}

// --- English ---

var messagesEN = Messages{
	App: appMsg{
		WindowTitle: "Remote ADB Tool",
		TabPair:     "P2P Direct",
		TabLAN:      "LAN Direct",
		TabSignal:   "Relay Server",
		GearTooltip: "Settings",
	},
	Common: commonMsg{
		Controller:        "Controller",
		Agent:             "Agent",
		StatusPrefix:      "Status: ",
		DevicesFmt:        "Devices (%d):",
		Stopped:           "Stopped",
		Disconnected:      "Not connected",
		CheckingADB:       "Checking ADB...",
		ADBErrorFmt:       "ADB error: %v",
		ErrorFmt:          "Error: %v",
		RunningFmt:        "Running (port %d)",
		StartServer:       "Start Server",
		StopServer:        "Stop Server",
		Connect:           "Connect",
		DisconnectBtn:     "Disconnect",
		TokenLabel:        "Token:",
		TokenHintOptional: "(optional)",
		TokenHintPSK:      "PSK auth token",
		Connecting:        "Connecting...",
		RelayNotice:       "Due to network restrictions, the connection is routed through a Cloudflare relay server",
	},
	Pair: pairMsg{
		GenerateOffer:     "Generate Invite Code",
		GenerateOfferFast: "Generate Now (risk of connection failure)",
		ClearBtn:          "Clear Codes",
		DisconnectBtn:     "Disconnect",
		ReconnectADB:      "Reconnect Remote ADB Devices",
		RefreshDevices:    "Re-detect Remote ADB Devices",

		OfferOutLabel:  "Invite code (copied to clipboard, single use):",
		AnswerInLabel:  "Response code (auto-connect on paste):",
		AnswerInHint:   "Paste the response code from the other side",
		OfferInLabel:   "Invite code (auto-process on paste):",
		OfferInHint:    "Paste the invite code from the other side",
		AnswerOutLabel: "Response code (copied to clipboard, single use):",
		RemoteHost:     "Remote host: ",
		RemoteDevFmt:   "Remote devices (%d):",
		LatencyFmt:     "Latency: %d ms",

		StatusNotStarted:      "Not started",
		StatusGenerating:      "Generating invite code...",
		StatusPreparingTURN:   "Preparing network relay...",
		StatusCreatingPC:      "Establishing connection...",
		StatusCreatingOffer:   "Finding the best connection path, please wait...",
		StatusEncodingOffer:   "Generating invite code...",
		StatusOfferReady:      "Invite code generated (copied to clipboard)",
		StatusPleaseGenerate:  "Please generate an invite code first",
		StatusPleaseAnswer:    "Please paste the response code",
		StatusPleaseOffer:     "Please paste the invite code",
		StatusProcessing:      "Processing invite code...",
		StatusDecodingOffer:   "Decoding invite code...",
		StatusCreatingAnswer:  "Finding the best connection path, please wait...",
		StatusEncodingAnswer:  "Generating response code...",
		StatusAnswerReady:     "Response code generated (copied to clipboard)",
		StatusP2PConnecting:   "Connecting to remote peer...",
		StatusP2PConnected:    "P2P connected",
		StatusRelayConnected:  "Connected via relay server",
		StatusP2PDisconnected: "P2P disconnected",
		StatusP2PProxyFmt:     "P2P connected, ADB Proxy: 127.0.0.1:%d",
		StatusP2PDevicesProxy: "P2P connected, %d remote device(s) (ADB Proxy: 127.0.0.1:%d)",
		StatusP2PWaiting:      "P2P connected, no devices on agent",
		StatusRelayWaiting:    "Connected via relay, no devices on agent",
		StatusP2PDevicesFmt:   "P2P connected, %d device(s)",
		StatusRelayDevicesFmt: "Connected via relay, %d device(s)",
		StatusControlClosed:   "Control channel closed",

		ErrCreatePCFmt:      "Failed to create PeerConnection: %v",
		ErrCreateCtrlChFmt:  "Failed to create control channel: %v",
		ErrCreateOfferFmt:   "Failed to create Offer: %v",
		ErrEncodeOfferFmt:   "Failed to encode invite code: %v",
		ErrInvalidAnswerFmt: "Invalid response code: %v",
		ErrHandleAnswerFmt:  "Failed to process response code: %v",
		ErrProxyListenerFmt: "Failed to create proxy listener: %v",
		ErrInvalidOfferFmt:  "Invalid invite code: %v",
		ErrHandleOfferFmt:   "Failed to process Offer: %v",
		ErrEncodeAnswerFmt:  "Failed to encode response code: %v",

		WarnTURNUnavailable: "Cloudflare relay unavailable, connection may fail in restricted networks",
	},
	LAN: lanMsg{
		ScanLAN:  "Scan LAN",
		Scanning: "Scanning...",

		AgentAddr:      "Agent address:",
		AgentsFoundFmt: "Found %d Agent(s):",
		ProxyDevFmt:    "ADB Proxy: 127.0.0.1:%d (%d device(s)):",

		StatusDisconnected:    "Disconnected",
		StatusPleaseAddr:      "Please enter Agent address",
		StatusQuerying:        "Querying devices...",
		StatusNoDevices:       "No available devices on Agent",
		StatusConnectedFmt:    "Connected, ADB Proxy: 127.0.0.1:%d",
		StatusConnectedDevFmt: "Connected, %d device(s)",

		ErrProxyFmt:        "Failed to create proxy: %v",
		ErrConnectFmt:      "Connection failed: %v",
		ErrSendFmt:         "Send failed: %v",
		ErrReadFmt:         "Read failed: %v",
		ErrQueryFmt:        "Query failed: %s",
		ErrDialAgentFmt:    "Failed to connect Agent: %v",
		ErrSendRequestFmt:  "Failed to send request: %v",
		ErrReadResponseFmt: "Failed to read response: %v",
	},
	Signal: signalMsg{
		Server:        "Server",
		StartAgent:    "Start Agent",
		StopAgent:     "Stop Agent",
		ConnectServer: "Connect to Server",
		HostnameLabel: "Hostname:",
		HostnameHint:  "Auto-detect",
		HostsFmt:      "Hosts (%d):",
		HostDevFmt:    "%d device(s)",
		Locked:        "(locked)",
		BindingsFmt:   "Bindings (%d):",
		BindLabel:     "[Bind]",
		UnbindLabel:   "[Unbind]",

		StatusPleaseToken:    "Please enter Token",
		StatusPleaseURLToken: "Please enter Server URL and Token",
		StatusRunning:        "Running",
		StatusConnected:      "Connected",
		StatusDisconnected:   "Disconnected",
		StatusBindOKFmt:         "Bound 127.0.0.1:%d → %s",
		StatusBindFailFmt:       "Bind failed: %s",
		StatusBindDecodeFailFmt: "Failed to decode bind result: %v",
		StatusUnbindOKFmt:       "Unbound port %d",
		StatusUnbindFailFmt:     "Unbind failed: %s",

		ErrIPCFmt:      "Failed to create IPC: %v",
		ErrDaemonFmt:   "Daemon error: %v",
		ErrIPCNotReady: "IPC not ready",
	},
	Settings: settingsMsg{
		Title:              "Settings",
		ConnectionSection:  "Connection",
		SaveBtn:            "Save Settings",
		CloseBtn:           "Close",
		AboutSection:       "About",
		CurrentVerFmt:      "Current version: %s",
		LatestVerFmt:       "Latest version: %s",
		CheckUpdate:        "Check for Updates",
		Checking:           "Checking...",
		Updating:           "Updating...",
		UpdateNow:          "Update Now",
		BannerNewVerFmt:    "Version %s available",
		BannerDismiss:      "Later",
		ConnModeLabel:       "Connection Mode",
		ConnModeDirectFirst: "Direct First (Default)",
		ConnModeDirectOnly: "Direct Only",
		ConnModeRelayOnly:  "Relay Only",
		STUNLabel:          "STUN Server",
		CustomStun:         "Custom",
		CustomStunOption:   "Custom...",
		TURNModeLabel:      "TURN Server",
		TURNModeCloudflare: "Cloudflare (Free)",
		TURNModeCustom:     "Custom...",
		TURNLabel:          "TURN Address",
		TURNHint:           "turn:your.server.com:3478",
		TURNUserLabel:      "TURN User",
		TURNPassLabel:      "TURN Password",
		LanguageLabel:      "Language",
		LanguageAuto:       "Auto",

		StatusChecking:       "Checking for updates...",
		StatusCheckFailFmt:   "Check failed: %v",
		StatusUpdateAvailFmt: "Update available: %s → %s",
		StatusUpToDate:       "Already up to date",
		StatusDownloading:    "Downloading update...",
		StatusUpdateFailFmt:  "Update failed: %v",
		StatusUpdatedFmt:     "Updated to %s, restarting...",
		StatusRestartFailFmt:       "Restart failed: %v",
		StatusUpdatePendingRestart: "Update downloaded, will restart after connections are closed",
	},
}
