// tab_pair.go 實作「簡易連線」分頁的 GUI 與邏輯。
//
// 本分頁透過手動交換 SDP token（邀請碼/回應碼）建立 WebRTC P2P 連線，
// 不需要中央伺服器。適合跨 NAT 的開發場景。
//
// # SDP 緊湊格式（compactSDP）
//
// 完整的 WebRTC SDP 包含大量樣板行（v=, o=, s=, m= 等固定內容），
// 實際需要交換的只有 ice-ufrag、ice-pwd、fingerprint、setup role 和 candidates。
// compactSDP 只保留這些必要欄位，配合二進位序列化 + deflate 壓縮 + base64 編碼，
// 將 token 長度壓縮到約 100-200 字元，方便使用者手動複製貼上。
//
// # ADB Transport 多工
//
// 客戶端（主控端）建立 ADB server proxy，接受 `adb connect` 的 device transport 連線。
// 每個 adb 服務（shell, push, pull 等）會建立一條獨立的 DataChannel
// （label=adb-stream/{id}/{serial}/{service}），
// 由 deviceBridge（見 adb_transport.go）管理 OPEN/OKAY/WRTE/CLSE 的訊息流控。
//
// # control channel
//
// P2P 連線建立後，雙方透過 label="control" 的 DataChannel 交換 JSON 訊息：
//   - hello：被控端發送主機名稱
//   - devices：被控端定期推送設備清單（serial、state、features）
package gui

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/io/clipboard"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/chris1004tw/remote-adb/internal/adb"
	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

// --- control channel 協定（DataChannel label="control"）---

// ctrlMessage 是 control channel 的 JSON 訊息。
// Type 可為 "hello"（攜帶主機名稱）或 "devices"（攜帶設備清單）。
type ctrlMessage struct {
	Type     string       `json:"type"`
	Hostname string       `json:"hostname,omitempty"`
	Devices  []ctrlDevice `json:"devices,omitempty"`
}

// ctrlDevice 是設備資訊。
type ctrlDevice struct {
	Serial   string `json:"serial"`
	State    string `json:"state"`
	Features string `json:"features,omitempty"` // 逗號分隔的 feature 清單（如 shell_v2,cmd,...）
}

// --- SDP 緊湊格式 ---
// 設計理由：WebRTC SDP 原始格式約 500-1000 字元，其中大部分是固定樣板。
// 透過 compactSDP 只保留可變欄位，再以二進位 + deflate + base64 編碼，
// 最終 token 長度壓縮到約 100-200 字元，使用者可以透過即時通訊軟體手動傳送。

// compactSDP 只保留 WebRTC data-channel SDP 的必要欄位。
// 完整 SDP 的樣板行（v=, o=, s=, m=application 等）可從預設值重建。
type compactSDP struct {
	U string   `json:"u"` // ice-ufrag
	P string   `json:"p"` // ice-pwd
	F string   `json:"f"` // fingerprint hash（hex，無冒號）
	S string   `json:"s"` // setup role（actpass/active/passive）
	C []string `json:"c"` // candidates，格式: proto,ip,port,priority,type[,raddr,rport]
}

// sdpToCompact 從完整 SDP 提取必要欄位。
func sdpToCompact(sdp string) compactSDP {
	var c compactSDP
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "a=ice-ufrag:"):
			c.U = strings.TrimPrefix(line, "a=ice-ufrag:")
		case strings.HasPrefix(line, "a=ice-pwd:"):
			c.P = strings.TrimPrefix(line, "a=ice-pwd:")
		case strings.HasPrefix(line, "a=fingerprint:sha-256 "):
			hash := strings.TrimPrefix(line, "a=fingerprint:sha-256 ")
			c.F = strings.ReplaceAll(hash, ":", "")
		case strings.HasPrefix(line, "a=setup:"):
			c.S = strings.TrimPrefix(line, "a=setup:")
		case strings.HasPrefix(line, "a=candidate:"):
			if cc := parseCandidate(line); cc != "" {
				c.C = append(c.C, cc)
			}
		}
	}
	c.C = filterCandidates(c.C)
	return c
}

// filterCandidates 過濾無用的 ICE candidate，減少 token 長度。
//
// 過濾規則：
//  1. 移除 loopback IP（127.x.x.x、::1）— 遠端連線無用
//  2. 移除 IPv6 link-local（fe80:: 開頭）— 跨 NAT 無用
//  3. srflx 同公網 IP + 同協定去重，保留最高 priority —
//     多張網卡映射到相同公網 IP 時只需保留一個 srflx candidate
//
// 設計意圖：減少手動複製 token 的長度，從 ~375 字元降至接近 200 字元。
func filterCandidates(candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}

	// 第一輪：過濾 loopback 和 link-local
	var filtered []string
	for _, c := range candidates {
		parts := strings.Split(c, ",")
		if len(parts) < 5 {
			continue
		}
		ip := net.ParseIP(parts[1])
		if ip == nil {
			continue
		}
		if ip.IsLoopback() {
			continue
		}
		if ip.IsLinkLocalUnicast() {
			continue
		}
		filtered = append(filtered, c)
	}

	// 第二輪：srflx 同公網 IP + 同協定去重，保留最高 priority
	// key = "proto,ip"（srflx 的公網 IP），value = 該 key 中最高 priority 的索引
	type srflxEntry struct {
		idx      int
		priority uint64
	}
	bestSrflx := make(map[string]srflxEntry)

	for i, c := range filtered {
		parts := strings.Split(c, ",")
		if len(parts) < 5 || parts[4] != "srflx" {
			continue
		}
		key := parts[0] + "," + parts[1] // proto,ip（公網 IP）
		pri, _ := strconv.ParseUint(parts[3], 10, 64)
		if existing, ok := bestSrflx[key]; !ok || pri > existing.priority {
			bestSrflx[key] = srflxEntry{idx: i, priority: pri}
		}
	}

	// 建立保留的 srflx 索引集合
	keepIdx := make(map[int]bool)
	for _, entry := range bestSrflx {
		keepIdx[entry.idx] = true
	}

	var result []string
	for i, c := range filtered {
		parts := strings.Split(c, ",")
		if len(parts) >= 5 && parts[4] == "srflx" {
			if keepIdx[i] {
				result = append(result, c)
			}
		} else {
			result = append(result, c)
		}
	}
	return result
}

// parseCandidate 將 SDP candidate 行轉為緊湊字串。
// 輸入: a=candidate:1 1 udp 2130706431 192.168.1.100 54321 typ host
// 輸出: udp,192.168.1.100,54321,2130706431,host
func parseCandidate(line string) string {
	// a=candidate:foundation component proto priority ip port typ type [raddr addr rport port]
	parts := strings.Fields(strings.TrimPrefix(line, "a=candidate:"))
	if len(parts) < 8 {
		return ""
	}
	proto := parts[2]    // udp/tcp
	priority := parts[3] // uint32
	ip := parts[4]
	port := parts[5]
	typ := parts[7] // host/srflx/prflx/relay

	result := fmt.Sprintf("%s,%s,%s,%s,%s", proto, ip, port, priority, typ)

	// 解析 raddr/rport（如果有）
	for i := 8; i < len(parts)-1; i++ {
		if parts[i] == "raddr" {
			result += "," + parts[i+1]
		}
		if parts[i] == "rport" {
			result += "," + parts[i+1]
		}
	}
	return result
}

// compactToSDP 從緊湊格式重建合法的 data-channel SDP。
func compactToSDP(c compactSDP) string {
	// 重新插入 fingerprint 冒號
	fp := insertColons(c.F)

	var b strings.Builder
	b.WriteString("v=0\r\n")
	b.WriteString("o=- 0 0 IN IP4 0.0.0.0\r\n")
	b.WriteString("s=-\r\n")
	b.WriteString("t=0 0\r\n")
	b.WriteString("a=group:BUNDLE 0\r\n")
	b.WriteString("a=msid-semantic:WMS\r\n")
	b.WriteString("m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n")
	b.WriteString("c=IN IP4 0.0.0.0\r\n")
	b.WriteString("a=ice-ufrag:" + c.U + "\r\n")
	b.WriteString("a=ice-pwd:" + c.P + "\r\n")
	b.WriteString("a=fingerprint:sha-256 " + fp + "\r\n")
	b.WriteString("a=setup:" + c.S + "\r\n")
	b.WriteString("a=mid:0\r\n")
	b.WriteString("a=sctp-port:5000\r\n")
	b.WriteString("a=max-message-size:262144\r\n")

	for i, cc := range c.C {
		b.WriteString(rebuildCandidate(i+1, cc) + "\r\n")
	}

	return b.String()
}

// insertColons 將連續的 hex 字串每 2 字元插入冒號。
// "AABBCCDD" → "AA:BB:CC:DD"
func insertColons(hex string) string {
	var parts []string
	for i := 0; i+2 <= len(hex); i += 2 {
		parts = append(parts, hex[i:i+2])
	}
	return strings.Join(parts, ":")
}

// rebuildCandidate 將緊湊 candidate 字串重建為 SDP candidate 行。
func rebuildCandidate(idx int, compact string) string {
	parts := strings.Split(compact, ",")
	if len(parts) < 5 {
		return ""
	}
	proto := parts[0]
	ip := parts[1]
	port := parts[2]
	priority := parts[3]
	typ := parts[4]

	line := fmt.Sprintf("a=candidate:%d 1 %s %s %s %s typ %s", idx, proto, priority, ip, port, typ)

	if len(parts) >= 7 {
		line += " raddr " + parts[5] + " rport " + parts[6]
	}
	return line
}

// --- 二進位序列化 ---
// 將 compactSDP 結構體編碼為緊湊的二進位格式，避免 JSON key 名稱和引號的開銷。
// 搭配 deflate 壓縮後再 base64 編碼，產生最終的 token 字串。
//
// 格式：
//   [1B ufragLen][ufrag][1B pwdLen][pwd]
//   [32B fingerprint raw bytes]
//   [1B setup enum: 0=actpass,1=active,2=passive]
//   [1B candidateCount]
//     每個 candidate:
//       [1B proto: 0=udp,1=tcp]
//       [1B ipLen: 4=IPv4, 16=IPv6]
//       [4/16B IP]
//       [2B port BE]
//       [4B priority BE]
//       [1B typ: 0=host,1=srflx,2=prflx,3=relay]
//       [1B hasRaddr: 0/1]
//       若 hasRaddr:
//         [1B raddrLen: 4/16]
//         [4/16B raddr]
//         [2B rport BE]

var setupToEnum = map[string]byte{"actpass": 0, "active": 1, "passive": 2}
var enumToSetup = [...]string{"actpass", "active", "passive"}

var typToEnum = map[string]byte{"host": 0, "srflx": 1, "prflx": 2, "relay": 3}
var enumToTyp = [...]string{"host", "srflx", "prflx", "relay"}

// marshalBinary 將 compactSDP 編碼為二進位格式。
func marshalBinary(c compactSDP) ([]byte, error) {
	var buf bytes.Buffer

	// ufrag
	buf.WriteByte(byte(len(c.U)))
	buf.WriteString(c.U)

	// pwd
	buf.WriteByte(byte(len(c.P)))
	buf.WriteString(c.P)

	// fingerprint: hex → raw 32 bytes
	fpBytes, err := hex.DecodeString(c.F)
	if err != nil {
		return nil, fmt.Errorf("fingerprint hex 解碼失敗: %w", err)
	}
	buf.Write(fpBytes)

	// setup
	s, ok := setupToEnum[c.S]
	if !ok {
		return nil, fmt.Errorf("未知 setup 值: %q", c.S)
	}
	buf.WriteByte(s)

	// candidates
	buf.WriteByte(byte(len(c.C)))
	for _, cc := range c.C {
		if err := marshalCandidate(&buf, cc); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// marshalCandidate 將一條 candidate 字串編碼為二進位。
func marshalCandidate(buf *bytes.Buffer, compact string) error {
	parts := strings.Split(compact, ",")
	if len(parts) < 5 {
		return fmt.Errorf("candidate 格式錯誤: %q", compact)
	}

	// proto
	if parts[0] == "udp" {
		buf.WriteByte(0)
	} else {
		buf.WriteByte(1)
	}

	// IP
	ip := net.ParseIP(parts[1])
	if ip == nil {
		return fmt.Errorf("無效 IP: %q", parts[1])
	}
	if v4 := ip.To4(); v4 != nil {
		buf.WriteByte(4)
		buf.Write(v4)
	} else {
		buf.WriteByte(16)
		buf.Write(ip.To16())
	}

	// port
	port, err := strconv.ParseUint(parts[2], 10, 16)
	if err != nil {
		return fmt.Errorf("無效 port: %w", err)
	}
	binary.Write(buf, binary.BigEndian, uint16(port))

	// priority
	pri, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil {
		return fmt.Errorf("無效 priority: %w", err)
	}
	binary.Write(buf, binary.BigEndian, uint32(pri))

	// type
	t, ok := typToEnum[parts[4]]
	if !ok {
		return fmt.Errorf("未知 candidate type: %q", parts[4])
	}
	buf.WriteByte(t)

	// raddr/rport
	if len(parts) >= 7 {
		buf.WriteByte(1)
		rip := net.ParseIP(parts[5])
		if rip == nil {
			return fmt.Errorf("無效 raddr: %q", parts[5])
		}
		if v4 := rip.To4(); v4 != nil {
			buf.WriteByte(4)
			buf.Write(v4)
		} else {
			buf.WriteByte(16)
			buf.Write(rip.To16())
		}
		rport, err := strconv.ParseUint(parts[6], 10, 16)
		if err != nil {
			return fmt.Errorf("無效 rport: %w", err)
		}
		binary.Write(buf, binary.BigEndian, uint16(rport))
	} else {
		buf.WriteByte(0)
	}

	return nil
}

// unmarshalBinary 將二進位資料解碼為 compactSDP。
func unmarshalBinary(data []byte) (compactSDP, error) {
	r := bytes.NewReader(data)
	var c compactSDP

	// ufrag
	uLen, err := r.ReadByte()
	if err != nil {
		return c, fmt.Errorf("讀取 ufrag 長度失敗: %w", err)
	}
	uBuf := make([]byte, uLen)
	if _, err := io.ReadFull(r, uBuf); err != nil {
		return c, fmt.Errorf("讀取 ufrag 失敗: %w", err)
	}
	c.U = string(uBuf)

	// pwd
	pLen, err := r.ReadByte()
	if err != nil {
		return c, fmt.Errorf("讀取 pwd 長度失敗: %w", err)
	}
	pBuf := make([]byte, pLen)
	if _, err := io.ReadFull(r, pBuf); err != nil {
		return c, fmt.Errorf("讀取 pwd 失敗: %w", err)
	}
	c.P = string(pBuf)

	// fingerprint: 32 bytes → uppercase hex
	fpBuf := make([]byte, 32)
	if _, err := io.ReadFull(r, fpBuf); err != nil {
		return c, fmt.Errorf("讀取 fingerprint 失敗: %w", err)
	}
	c.F = strings.ToUpper(hex.EncodeToString(fpBuf))

	// setup
	sEnum, err := r.ReadByte()
	if err != nil {
		return c, fmt.Errorf("讀取 setup 失敗: %w", err)
	}
	if int(sEnum) >= len(enumToSetup) {
		return c, fmt.Errorf("未知 setup enum: %d", sEnum)
	}
	c.S = enumToSetup[sEnum]

	// candidates
	cCount, err := r.ReadByte()
	if err != nil {
		return c, fmt.Errorf("讀取 candidate 數量失敗: %w", err)
	}
	c.C = make([]string, cCount)
	for i := range c.C {
		cc, err := unmarshalCandidate(r)
		if err != nil {
			return c, fmt.Errorf("解碼 candidate[%d] 失敗: %w", i, err)
		}
		c.C[i] = cc
	}

	return c, nil
}

// unmarshalCandidate 從 reader 解碼一條 candidate。
func unmarshalCandidate(r *bytes.Reader) (string, error) {
	// proto
	protoByte, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	proto := "udp"
	if protoByte == 1 {
		proto = "tcp"
	}

	// IP
	ipStr, err := readIP(r)
	if err != nil {
		return "", fmt.Errorf("讀取 IP 失敗: %w", err)
	}

	// port
	var port uint16
	if err := binary.Read(r, binary.BigEndian, &port); err != nil {
		return "", fmt.Errorf("讀取 port 失敗: %w", err)
	}

	// priority
	var priority uint32
	if err := binary.Read(r, binary.BigEndian, &priority); err != nil {
		return "", fmt.Errorf("讀取 priority 失敗: %w", err)
	}

	// type
	typByte, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	if int(typByte) >= len(enumToTyp) {
		return "", fmt.Errorf("未知 candidate type enum: %d", typByte)
	}
	typ := enumToTyp[typByte]

	result := fmt.Sprintf("%s,%s,%d,%d,%s", proto, ipStr, port, priority, typ)

	// hasRaddr
	hasRaddr, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	if hasRaddr == 1 {
		raddrStr, err := readIP(r)
		if err != nil {
			return "", fmt.Errorf("讀取 raddr 失敗: %w", err)
		}
		var rport uint16
		if err := binary.Read(r, binary.BigEndian, &rport); err != nil {
			return "", fmt.Errorf("讀取 rport 失敗: %w", err)
		}
		result += fmt.Sprintf(",%s,%d", raddrStr, rport)
	}

	return result, nil
}

// readIP 從 reader 讀取 IP 位址（1B 長度 + 4/16B 資料）。
func readIP(r *bytes.Reader) (string, error) {
	ipLen, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	ipBuf := make([]byte, ipLen)
	if _, err := io.ReadFull(r, ipBuf); err != nil {
		return "", err
	}
	return net.IP(ipBuf).String(), nil
}

// encodeToken 將 compactSDP 編碼為可傳輸的 token 字串。
// 流程：compactSDP → 二進位序列化 → deflate 壓縮（BestCompression）→ base64url 編碼。
// 使用 RawURLEncoding（無 padding）進一步減少字元數。
func encodeToken(c compactSDP) (string, error) {
	data, err := marshalBinary(c)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// decodeToken 是 encodeToken 的逆操作：base64url 解碼 → deflate 解壓 → 二進位反序列化。
func decodeToken(token string) (compactSDP, error) {
	compressed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return compactSDP{}, fmt.Errorf("base64 解碼失敗: %w", err)
	}
	r := flate.NewReader(bytes.NewReader(compressed))
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return compactSDP{}, fmt.Errorf("deflate 解壓失敗: %w", err)
	}
	return unmarshalBinary(data)
}

// pairTab 是「簡易連線」分頁的完整狀態。
// 提供兩個角色：主控模式（客戶端）和被控模式（伺服器）。
//
// 主控模式流程：產生邀請碼 → 傳給對方 → 對方貼回回應碼 → 自動建立 P2P 連線 →
// 啟動 ADB server proxy → 自動 adb connect。
//
// 被控模式流程：貼入邀請碼 → 自動產生回應碼 → 傳給對方 → 等待 P2P 連線 →
// 透過 control channel 推送設備清單。
type pairTab struct {
	window *app.Window
	config *AppConfig // 共用設定（Port、STUN 等），來自設定面板

	// 角色選擇
	clientBtn widget.Clickable
	serverBtn widget.Clickable
	isServer  bool // false=客戶端, true=伺服器

	// --- 客戶端模式 ---
	cliGenOfferBtn     widget.Clickable
	cliOfferOutEditor  widget.Editor // 顯示邀請碼（唯讀）
	cliAnswerInEditor  widget.Editor // 貼入回應碼
	cliApplyBtn        widget.Clickable

	// --- 被控模式 ---
	srvOfferInEditor   widget.Editor // 貼入邀請碼
	srvAnswerOutEditor widget.Editor // 顯示回應碼（唯讀）
	srvProcessedOffer  string        // 上次已處理的 offer，用於自動偵測變更
	cliProcessedAnswer string        // 上次已處理的 answer，用於自動偵測變更

	// --- 共用狀態 ---
	mu        sync.Mutex
	status    string
	connected bool
	cancel    context.CancelFunc
	pm        *webrtc.PeerManager
	controlCh io.ReadWriteCloser // 客戶端持有的 control channel

	// 剪貼簿：產生 token 後自動複製
	pendingClipboard string

	// 伺服器模式：設備清單
	srvDevices []ctrlDevice

	// 客戶端模式
	proxyPort  int          // ADB server proxy 實際 port
	proxyLn    net.Listener // proxy TCP listener
	cliDevices    []ctrlDevice    // 遠端設備清單（control channel 推送）
	deviceReadyCh chan struct{}   // CNXN 等待信號：有設備時 close，無設備時重建

	// Forward 攔截（客戶端模式）
	// scrcpy 等工具會執行 `adb forward tcp:PORT localabstract:scrcpy`，
	// 需要在本機攔截 forward 命令並建立到遠端設備的 DataChannel 轉發。
	fwdMu        sync.Mutex
	fwdListeners map[string]*fwdListener // key = localSpec (e.g., "tcp:27183")

	// 自動 adb connect（客戶端模式）
	autoConnected bool

	// 遠端資訊（客戶端模式，mutex 保護）
	remoteHostname string // 遠端主機名稱（via control channel）
	remoteAddr     string // 遠端 IP:port（via WebRTC stats）

	// 實時延遲（毫秒），atomic 存取
	latencyMs atomic.Int64

	// 結束連線 / 清除按鈕
	disconnectBtn widget.Clickable
	clearBtn      widget.Clickable

	// 捲動清單
	list widget.List
}

// newPairTab 建立並初始化 pairTab，設定各輸入框的預設值。
// 預設顯示主控模式（isServer=false）。
func newPairTab(w *app.Window, cfg *AppConfig) *pairTab {
	t := &pairTab{
		window: w,
		config: cfg,
		status: "未開始",
	}
	// 客戶端模式
	t.cliOfferOutEditor.ReadOnly = true

	// 伺服器模式
	t.srvAnswerOutEditor.ReadOnly = true

	t.list.Axis = layout.Vertical
	return t
}

// layout 繪製分頁內容。使用 widget.List 實現可捲動版面（token 和設備列表可能超出視窗）。
// 已連線時自動切換到精簡 UI（只顯示連線資訊 + 設備清單 + 結束連線按鈕）。
func (t *pairTab) layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	t.mu.Lock()
	isServer := t.isServer
	status := t.status
	connected := t.connected
	t.mu.Unlock()

	// 自動複製到剪貼簿
	t.mu.Lock()
	clip := t.pendingClipboard
	t.pendingClipboard = ""
	t.mu.Unlock()
	if clip != "" {
		gtx.Execute(clipboard.WriteCmd{
			Type: "application/text",
			Data: io.NopCloser(strings.NewReader(clip)),
		})
	}

	// 角色切換
	for t.clientBtn.Clicked(gtx) {
		t.isServer = false
	}
	for t.serverBtn.Clicked(gtx) {
		t.isServer = true
	}

	// 組裝所有 widget（用於可捲動的 List）
	var widgets []layout.Widget

	// 角色選擇列
	widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(th, &t.clientBtn, "主控端")
					if !isServer {
						btn.Background = colorModeActive
					} else {
						btn.Background = colorModeInactive
					}
					return btn.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(th, &t.serverBtn, "被控端")
					if isServer {
						btn.Background = colorModeActive
					} else {
						btn.Background = colorModeInactive
					}
					return btn.Layout(gtx)
				}),
			)
		})
	})

	// 子模式內容
	if isServer {
		for _, child := range t.layoutServerWidgets(gtx, th) {
			widgets = append(widgets, child)
		}
	} else {
		for _, child := range t.layoutClientWidgets(gtx, th) {
			widgets = append(widgets, child)
		}
	}

	// 狀態
	widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
		c := colorPanelHint
		if connected {
			c = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
		}
		return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return statusText(gtx, th, "狀態: "+status, c)
		})
	})

	// 可捲動的清單
	return material.List(th, &t.list).Layout(gtx, len(widgets), func(gtx layout.Context, i int) layout.Dimensions {
		return widgets[i](gtx)
	})
}

// === 主控模式 UI ===

// layoutClientWidgets 繪製主控模式的 UI 元件。
// 根據連線狀態分兩種顯示：
//   - 未連線：STUN 設定、ADB Port、產生邀請碼按鈕、邀請碼/回應碼文字框
//   - 已連線：ADB Proxy 位址 + 延遲、遠端主機資訊、設備列表、結束連線按鈕
//
// 邀請碼產生後會自動複製到剪貼簿；回應碼貼入後自動偵測變更並觸發連線。
func (t *pairTab) layoutClientWidgets(gtx layout.Context, th *material.Theme) []layout.Widget {
	t.mu.Lock()
	devices := append([]ctrlDevice{}, t.cliDevices...)
	proxyPort := t.proxyPort
	connected := t.connected
	hostname := t.remoteHostname
	remoteAddr := t.remoteAddr
	t.mu.Unlock()

	// 已連線：只顯示連線資訊 + 設備清單 + 結束連線按鈕
	if connected {
		for t.disconnectBtn.Clicked(gtx) {
			t.disconnect()
		}

		var widgets []layout.Widget
		latency := t.latencyMs.Load()

		if proxyPort > 0 {
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				proxyText := fmt.Sprintf("ADB Proxy: 127.0.0.1:%d", proxyPort)
				if latency > 0 {
					proxyText += fmt.Sprintf("  (%d ms)", latency)
				}
				lbl := material.Body2(th, proxyText)
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			})
		}

		// 遠端主機資訊
		if hostname != "" || remoteAddr != "" {
			infoText := "遠端主機: "
			if hostname != "" {
				infoText += hostname
			}
			if remoteAddr != "" {
				if hostname != "" {
					infoText += " (" + remoteAddr + ")"
				} else {
					infoText += remoteAddr
				}
			}
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Body2(th, infoText).Layout(gtx)
				})
			})
		}

		if len(devices) > 0 {
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Body2(th, fmt.Sprintf("遠端設備 (%d):", len(devices))).Layout(gtx)
				})
			})
			for _, d := range devices {
				text := fmt.Sprintf("  %s [%s] → 127.0.0.1:%d", d.Serial, d.State, proxyPort)
				state := d.State
				widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(th, text)
						if state == "device" {
							lbl.Color = color.NRGBA{R: 76, G: 175, B: 80, A: 255}
						}
						return lbl.Layout(gtx)
					})
				})
			}
		}

		// 結束連線按鈕
		widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.disconnectBtn, "結束連線")
				btn.Background = color.NRGBA{R: 244, G: 67, B: 54, A: 255}
				return btn.Layout(gtx)
			})
		})

		return widgets
	}

	// 未連線：顯示完整設定 UI
	for t.cliGenOfferBtn.Clicked(gtx) {
		t.clientGenerateOffer()
	}

	// 自動偵測回應碼貼入（offer 已產生時）
	if t.cliOfferOutEditor.Text() != "" {
		currentAnswer := strings.TrimSpace(t.cliAnswerInEditor.Text())
		if currentAnswer != "" && currentAnswer != t.cliProcessedAnswer {
			t.cliProcessedAnswer = currentAnswer
			t.clientApplyAnswer()
		}
	}

	widgets := []layout.Widget{
		// 產生邀請碼按鈕
		func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.cliGenOfferBtn, "產生邀請碼")
				return btn.Layout(gtx)
			})
		},
		// 邀請碼輸出（限高可捲動，已自動複製到剪貼簿）
		func(gtx layout.Context) layout.Dimensions {
			if t.cliOfferOutEditor.Text() == "" {
				return layout.Dimensions{}
			}
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return tokenBox(gtx, th, "邀請碼（已複製到剪貼簿，僅限使用一次）:", &t.cliOfferOutEditor, "", unit.Dp(100))
			})
		},
		// 回應碼輸入（貼入後自動連線）
		func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return tokenBox(gtx, th, "回應碼（貼入後自動連線）:", &t.cliAnswerInEditor, "貼入對方給的回應碼", unit.Dp(80))
			})
		},
	}

	return widgets
}

// === 被控模式 UI ===

// layoutServerWidgets 繪製被控模式的 UI 元件。
// 邀請碼貼入後自動偵測變更並觸發 serverProcessOffer（產生回應碼）。
// 已連線後顯示延遲、設備列表、結束連線按鈕。
func (t *pairTab) layoutServerWidgets(gtx layout.Context, th *material.Theme) []layout.Widget {
	t.mu.Lock()
	devices := append([]ctrlDevice{}, t.srvDevices...)
	connected := t.connected
	t.mu.Unlock()

	// 已連線：只顯示延遲 + 設備清單 + 結束連線按鈕
	if connected {
		for t.disconnectBtn.Clicked(gtx) {
			t.disconnect()
		}

		var widgets []layout.Widget
		latency := t.latencyMs.Load()
		if latency > 0 {
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, fmt.Sprintf("延遲: %d ms", latency))
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			})
		}

		if len(devices) > 0 {
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Body2(th, fmt.Sprintf("設備 (%d):", len(devices))).Layout(gtx)
				})
			})
			for _, d := range devices {
				text := fmt.Sprintf("  %s [%s]", d.Serial, d.State)
				widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return material.Body2(th, text).Layout(gtx)
					})
				})
			}
		}

		// 結束連線按鈕
		widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.disconnectBtn, "結束連線")
				btn.Background = color.NRGBA{R: 244, G: 67, B: 54, A: 255}
				return btn.Layout(gtx)
			})
		})
		return widgets
	}

	// 未連線：完整設定 UI + 自動偵測邀請碼
	currentOffer := strings.TrimSpace(t.srvOfferInEditor.Text())
	if currentOffer != "" && currentOffer != t.srvProcessedOffer {
		t.srvProcessedOffer = currentOffer
		t.serverProcessOffer()
	}

	for t.clearBtn.Clicked(gtx) {
		t.cleanup() // 清理待連線的 PeerManager（如有）
		t.srvOfferInEditor.SetText("")
		t.srvAnswerOutEditor.SetText("")
		t.srvProcessedOffer = ""
		t.mu.Lock()
		t.status = "未開始"
		t.mu.Unlock()
	}

	var widgets []layout.Widget

	// 邀請碼輸入
	widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return tokenBox(gtx, th, "邀請碼（貼入後自動處理）:", &t.srvOfferInEditor, "貼入對方給的邀請碼", unit.Dp(80))
		})
	})
	// 回應碼輸出
	widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
		if t.srvAnswerOutEditor.Text() == "" {
			return layout.Dimensions{}
		}
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return tokenBox(gtx, th, "回應碼（已複製到剪貼簿，僅限使用一次）:", &t.srvAnswerOutEditor, "", unit.Dp(100))
		})
	})

	// 清除按鈕（有內容時才顯示）
	hasContent := t.srvOfferInEditor.Text() != "" || t.srvAnswerOutEditor.Text() != ""
	if hasContent {
		widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &t.clearBtn, "清除邀請碼 / 回應碼")
				btn.Background = colorTabInactive
				return btn.Layout(gtx)
			})
		})
	}

	// 設備列表
	if len(devices) > 0 {
		widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return material.Body2(th, fmt.Sprintf("設備 (%d):", len(devices))).Layout(gtx)
			})
		})
		for _, d := range devices {
			text := fmt.Sprintf("  %s [%s]", d.Serial, d.State)
			widgets = append(widgets, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: unit.Dp(16), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.Body2(th, text).Layout(gtx)
				})
			})
		}
	}

	return widgets
}

// === 客戶端（主控）模式邏輯 ===

// clientGenerateOffer 產生 WebRTC Offer 並編碼為邀請碼 token。
// 流程：建立 PeerConnection → 建立 control DataChannel → 建立 Offer →
// SDP 壓縮編碼 → 顯示在 UI 並自動複製到剪貼簿。
func (t *pairTab) clientGenerateOffer() {
	stunURLs := t.config.STUNServer

	t.mu.Lock()
	t.status = "正在產生邀請碼..."
	t.mu.Unlock()
	t.window.Invalidate()

	go func() {
		iceConfig := parseICEConfig(stunURLs)

		pm, err := webrtc.NewPeerManager(iceConfig)
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("建立 PeerConnection 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		// 建立 control DataChannel
		controlCh, err := pm.OpenChannel("control")
		if err != nil {
			pm.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf("建立 control channel 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		offerSDP, err := pm.CreateOffer()
		if err != nil {
			pm.Close()
			t.mu.Lock()
			t.status = fmt.Sprintf("建立 Offer 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		offerToken, err := encodeToken(sdpToCompact(offerSDP))
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("編碼邀請碼失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		t.mu.Lock()
		t.pm = pm
		t.controlCh = controlCh
		t.pendingClipboard = offerToken
		t.status = "邀請碼已產生（已複製到剪貼簿）"
		t.mu.Unlock()
		t.cliOfferOutEditor.SetText(offerToken)
		t.window.Invalidate()
	}()
}

// clientApplyAnswer 處理對方回傳的回應碼，完成 WebRTC 握手並啟動服務。
// 流程：解碼回應碼 → HandleAnswer → 建立 ADB server proxy listener →
// 啟動 adbServerProxy（每個 TCP 連線建立 DataChannel）→
// 啟動 RTT 輪詢 → 啟動 controlReadLoop（接收設備清單）。
func (t *pairTab) clientApplyAnswer() {
	answerToken := t.cliAnswerInEditor.Text()
	proxyPort := t.config.ProxyPort

	t.mu.Lock()
	pm := t.pm
	controlCh := t.controlCh
	t.mu.Unlock()

	if pm == nil {
		t.mu.Lock()
		t.status = "請先產生邀請碼"
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	if answerToken == "" {
		t.mu.Lock()
		t.status = "請貼入對方的回應碼"
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	go func() {
		answer, err := decodeToken(answerToken)
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("無效回應碼: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		if err := pm.HandleAnswer(compactToSDP(answer)); err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("處理回應碼失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		// 建立 ADB server proxy TCP listener
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("建立 proxy listener 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}
		actualPort := ln.Addr().(*net.TCPAddr).Port

		ctx, cancel := context.WithCancel(context.Background())

		pm.OnDisconnect(func() {
			t.mu.Lock()
			port := t.proxyPort
			wasConnected := t.autoConnected
			t.autoConnected = false
			t.status = "P2P 已斷線"
			t.connected = false
			t.mu.Unlock()
			t.window.Invalidate()

			// 自動 adb disconnect
			if wasConnected && port > 0 {
				go func() {
					dialer := adb.NewDialer("")
					dialer.Disconnect(fmt.Sprintf("127.0.0.1:%d", port))
				}()
			}
		})

		t.mu.Lock()
		t.connected = true
		t.cancel = cancel
		t.proxyPort = actualPort
		t.proxyLn = ln
		t.deviceReadyCh = make(chan struct{}) // CNXN 等待信號：控制通道收到設備時 close
		t.status = fmt.Sprintf("P2P 已連線，ADB Proxy: 127.0.0.1:%d", actualPort)
		t.mu.Unlock()
		t.window.Invalidate()

		// 啟動 ADB server proxy（每個 TCP 連線建立獨立 DataChannel）
		go t.adbServerProxy(ctx, ln, pm)

		// 啟動 RTT 延遲輪詢
		go t.rttPollLoop(ctx, pm)

		// 啟動 control channel 讀取迴圈（僅更新 UI 設備清單）
		t.controlReadLoop(ctx, controlCh)
	}()
}

// adbServerProxy 接受本機 TCP 連線。
// 每個連線由 handleProxyConn 處理：根據前 4 bytes 判斷是 device transport（CNXN）
// 還是 ADB server 協定（hex 長度前綴），再決定轉發策略。
func (t *pairTab) adbServerProxy(ctx context.Context, ln net.Listener, pm *webrtc.PeerManager) {
	var connID atomic.Int64
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		id := connID.Add(1)
		go t.handleProxyConn(ctx, conn, pm.OpenChannel, id)
	}
}

// controlReadLoop 持續讀取 control channel 的 JSON 訊息，更新客戶端 UI。
//
// 訊息類型處理：
//   - "hello"：記錄遠端主機名稱（顯示在 UI 上）
//   - "devices"：更新遠端設備清單，若首次偵測到在線設備則自動執行 `adb connect`
//
// 自動 adb connect 機制：收到第一個 state="device" 的設備時，
// 自動執行 `adb connect 127.0.0.1:<proxyPort>`，讓設備出現在 `adb devices` 列表中。
// 這讓使用者不需手動操作即可開始使用 scrcpy 等工具。
func (t *pairTab) controlReadLoop(ctx context.Context, controlCh io.ReadWriteCloser) {
	dec := json.NewDecoder(controlCh)
	for {
		var msg ctrlMessage
		if err := dec.Decode(&msg); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("control channel 讀取結束", "error", err)
			t.mu.Lock()
			t.status = "control channel 已關閉"
			t.connected = false
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		if msg.Type == "hello" {
			t.mu.Lock()
			t.remoteHostname = msg.Hostname
			t.mu.Unlock()
			t.window.Invalidate()
			continue
		}

		if msg.Type != "devices" {
			continue
		}

		hasDevice := false
		for _, d := range msg.Devices {
			if d.State == "device" {
				hasDevice = true
				break
			}
		}

		t.mu.Lock()
		t.cliDevices = msg.Devices
		count := 0
		for _, d := range msg.Devices {
			if d.State == "device" {
				count++
			}
		}
		// 更新設備就緒信號：有設備時 close（通知等待中的 CNXN），
		// 設備消失時重建 channel（讓後續 CNXN 繼續等待）。
		if t.deviceReadyCh != nil {
			if hasDevice {
				select {
				case <-t.deviceReadyCh:
					// 已 close，不需重複操作
				default:
					close(t.deviceReadyCh)
				}
			} else {
				select {
				case <-t.deviceReadyCh:
					// 之前有設備但現在消失，重建 channel
					t.deviceReadyCh = make(chan struct{})
				default:
					// 尚未有設備，channel 仍開啟，繼續等待
				}
			}
		}
		shouldConnect := hasDevice && !t.autoConnected && t.proxyPort > 0
		if shouldConnect {
			t.autoConnected = true
		}
		proxyPort := t.proxyPort
		t.status = fmt.Sprintf("P2P 已連線，遠端 %d 個設備（ADB Proxy: 127.0.0.1:%d）", count, proxyPort)
		t.mu.Unlock()
		t.window.Invalidate()

		// 自動 adb connect：讓設備出現在 `adb devices`
		if shouldConnect {
			go func() {
				dialer := adb.NewDialer("")
				target := fmt.Sprintf("127.0.0.1:%d", proxyPort)
				if err := dialer.Connect(target); err != nil {
					slog.Debug("自動 adb connect 失敗", "target", target, "error", err)
				} else {
					slog.Debug("自動 adb connect 成功", "target", target)
				}
			}()
		}
	}
}

// === 伺服器（被控）模式邏輯 ===

// serverProcessOffer 處理對方的邀請碼，建立 Answer 並啟動被控端服務。
// 流程：EnsureADB → 解碼邀請碼 → 建立 PeerConnection → 設定 OnChannel 回調
// （監聽 control/adb-server/adb-stream/adb-fwd DataChannel）→ HandleOffer →
// 產生回應碼 → 自動複製到剪貼簿。
// 注意：不在此時設定 connected=true，而是等 control DataChannel 真正開啟後才切換。
func (t *pairTab) serverProcessOffer() {
	offerToken := t.srvOfferInEditor.Text()
	adbPort := t.config.ADBPort
	stunURLs := t.config.STUNServer

	if offerToken == "" {
		t.mu.Lock()
		t.status = "請貼入對方的邀請碼"
		t.mu.Unlock()
		t.window.Invalidate()
		return
	}

	t.mu.Lock()
	t.status = "檢查 ADB..."
	t.mu.Unlock()
	t.window.Invalidate()

	go func() {
		adbAddr := fmt.Sprintf("127.0.0.1:%d", adbPort)

		// 確保 ADB 可用
		if err := adb.EnsureADB(context.Background(), adbAddr, func(status string) {
			t.mu.Lock()
			t.status = status
			t.mu.Unlock()
			t.window.Invalidate()
		}); err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("ADB 錯誤: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		t.mu.Lock()
		t.status = "正在處理邀請碼..."
		t.mu.Unlock()
		t.window.Invalidate()

		// 解碼 Offer
		offer, err := decodeToken(offerToken)
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("無效邀請碼: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		iceConfig := parseICEConfig(stunURLs)
		pm, err := webrtc.NewPeerManager(iceConfig)
		if err != nil {
			t.mu.Lock()
			t.status = fmt.Sprintf("建立 PeerConnection 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		ctx, cancel := context.WithCancel(context.Background())

		// 監聽客戶端建立的 DataChannel
		pm.OnChannel(func(label string, rwc io.ReadWriteCloser) {
			slog.Debug("收到 DataChannel", "label", label)
			if label == "control" {
				// DataChannel 開啟 = P2P 真正連上，此時才切換到已連線 UI
				t.mu.Lock()
				t.connected = true
				t.status = "P2P 已連線，等待設備..."
				t.mu.Unlock()
				t.window.Invalidate()
				// 客戶端的 control channel → 啟動設備推送
				go t.devicePushLoop(ctx, rwc, adbAddr)
				return
			}
			// adb-server/{id} → 轉發到本機 ADB server
			if strings.HasPrefix(label, "adb-server/") {
				go t.handleADBServerConn(ctx, rwc, adbAddr)
				return
			}
			// adb-fwd/{id}/{serial}/{remoteSpec} → forward 連線到設備服務
			if strings.HasPrefix(label, "adb-fwd/") {
				parts := strings.SplitN(label, "/", 4)
				if len(parts) == 4 {
					go t.handleADBForwardConn(ctx, rwc, adbAddr, parts[2], parts[3])
				} else {
					rwc.Close()
				}
				return
			}
			// adb-stream/{id}/{serial}/{service} → device transport 串流
			if strings.HasPrefix(label, "adb-stream/") {
				parts := strings.SplitN(label, "/", 4)
				if len(parts) == 4 {
					go t.handleADBStreamConn(ctx, rwc, adbAddr, parts[2], parts[3])
				} else {
					rwc.Close()
				}
				return
			}
		})

		pm.OnDisconnect(func() {
			t.mu.Lock()
			t.status = "P2P 已斷線"
			t.connected = false
			t.srvDevices = nil
			t.mu.Unlock()
			t.window.Invalidate()
		})

		// 處理 Offer 並生成 Answer
		answerSDP, err := pm.HandleOffer(compactToSDP(offer))
		if err != nil {
			pm.Close()
			cancel()
			t.mu.Lock()
			t.status = fmt.Sprintf("處理 Offer 失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		answerToken, err := encodeToken(sdpToCompact(answerSDP))
		if err != nil {
			pm.Close()
			cancel()
			t.mu.Lock()
			t.status = fmt.Sprintf("編碼回應碼失敗: %v", err)
			t.mu.Unlock()
			t.window.Invalidate()
			return
		}

		t.mu.Lock()
		t.pm = pm
		t.cancel = cancel
		// 注意：不在此設 connected = true，等 control DataChannel 開啟才切換
		t.pendingClipboard = answerToken
		t.status = "回應碼已產生（已複製到剪貼簿）"
		t.mu.Unlock()
		t.srvAnswerOutEditor.SetText(answerToken)
		t.window.Invalidate()

		// 啟動 RTT 延遲輪詢
		go t.rttPollLoop(ctx, pm)
	}()
}

// devicePushLoop 追蹤 ADB 設備並透過 control channel 推送清單給客戶端。
// 使用 ADB tracker 的事件驅動模式（而非輪詢），設備增減時即時推送。
// 對每個在線設備額外查詢 features（如 shell_v2, cmd 等），讓客戶端的 CNXN 回應
// 能攜帶真實 features，避免 adb 功能不相容。
func (t *pairTab) devicePushLoop(ctx context.Context, controlCh io.ReadWriteCloser, adbAddr string) {
	tracker := adb.NewTracker(adbAddr)
	deviceCh := tracker.Track(ctx)
	table := adb.NewDeviceTable()
	enc := json.NewEncoder(controlCh)

	// 先發送主機名稱
	hostname, _ := os.Hostname()
	if err := enc.Encode(ctrlMessage{Type: "hello", Hostname: hostname}); err != nil {
		slog.Debug("發送 hello 失敗", "error", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case events, ok := <-deviceCh:
			if !ok {
				return
			}
			table.Update(events)
			devs := table.List()

			ctrlDevs := make([]ctrlDevice, len(devs))
			for i, d := range devs {
				ctrlDevs[i] = ctrlDevice{Serial: d.Serial, State: d.State}
				if d.State == "device" {
					if feat, err := queryDeviceFeatures(adbAddr, d.Serial); err == nil {
						ctrlDevs[i].Features = feat
					}
				}
			}

			// 推送給客戶端
			if err := enc.Encode(ctrlMessage{Type: "devices", Devices: ctrlDevs}); err != nil {
				slog.Debug("control channel 寫入失敗", "error", err)
				return
			}

			// 更新伺服器端 UI
			t.mu.Lock()
			t.srvDevices = ctrlDevs
			if len(ctrlDevs) > 0 {
				t.status = fmt.Sprintf("P2P 已連線，%d 個設備", len(ctrlDevs))
			} else {
				t.status = "P2P 已連線，等待設備..."
			}
			t.mu.Unlock()
			t.window.Invalidate()
		}
	}
}

// handleADBServerConn 將客戶端的 DataChannel 轉發到本機 ADB server。
func (t *pairTab) handleADBServerConn(ctx context.Context, rwc io.ReadWriteCloser, adbAddr string) {
	defer rwc.Close()

	conn, err := net.Dial("tcp", adbAddr)
	if err != nil {
		slog.Debug("連線本機 ADB server 失敗", "error", err)
		return
	}
	defer conn.Close()

	biCopy(ctx, rwc, conn)
}

// handleADBStreamConn 處理 device transport 的單一串流（被控端）。
// 收到客戶端建立的 adb-stream DataChannel 後：
//  1. 連線本機 ADB server
//  2. 發送 host:transport:<serial> 切換到目標設備
//  3. 發送 service 命令（如 shell:ls、sync: 等）
//  4. 通知客戶端就緒（寫入 1 byte: 1=成功, 0=失敗）
//  5. 雙向橋接 DataChannel ↔ ADB server（biCopy）
func (t *pairTab) handleADBStreamConn(ctx context.Context, rwc io.ReadWriteCloser, adbAddr, serial, service string) {
	defer rwc.Close()

	slog.Debug("stream: 開始處理", "serial", serial, "service", service)

	conn, err := net.Dial("tcp", adbAddr)
	if err != nil {
		slog.Debug("stream: 連線 ADB server 失敗", "error", err)
		rwc.Write([]byte{0})
		return
	}
	defer conn.Close()

	// 切換到目標設備
	if err := sendADBCmd(conn, fmt.Sprintf("host:transport:%s", serial)); err != nil {
		slog.Debug("stream: transport 命令失敗", "error", err)
		rwc.Write([]byte{0})
		return
	}
	if err := readADBStatus(conn); err != nil {
		slog.Debug("stream: transport 失敗", "serial", serial, "error", err)
		rwc.Write([]byte{0})
		return
	}

	slog.Debug("stream: transport 成功", "serial", serial)

	// 發送服務命令
	if err := sendADBCmd(conn, service); err != nil {
		slog.Debug("stream: service 命令失敗", "service", service, "error", err)
		rwc.Write([]byte{0})
		return
	}
	if err := readADBStatus(conn); err != nil {
		slog.Debug("stream: service 失敗", "service", service, "error", err)
		rwc.Write([]byte{0})
		return
	}

	// 通知客戶端就緒
	if n, err := rwc.Write([]byte{1}); err != nil {
		slog.Debug("stream: 就緒信號寫入失敗", "serial", serial, "service", service, "error", err, "n", n)
		return
	}

	slog.Debug("stream 已建立", "serial", serial, "service", service)

	// 雙向轉發（biCopy 結束時關閉雙方，避免死鎖）
	biCopy(ctx, rwc, conn)
}

// rttPollLoop 每 2 秒輪詢 WebRTC RTT 並更新 latencyMs。
func (t *pairTab) rttPollLoop(ctx context.Context, pm *webrtc.PeerManager) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed := false

			rtt := pm.GetRTT()
			ms := rtt.Milliseconds()
			if old := t.latencyMs.Load(); old != ms {
				t.latencyMs.Store(ms)
				changed = true
			}

			if addr := pm.GetRemoteAddr(); addr != "" {
				t.mu.Lock()
				if t.remoteAddr != addr {
					t.remoteAddr = addr
					changed = true
				}
				t.mu.Unlock()
			}

			if changed {
				t.window.Invalidate()
			}
		}
	}
}

// disconnect 結束 P2P 連線，清理所有資源並恢復初始 UI 狀態。
// 清理範圍：forward listeners、proxy listener、control channel、PeerConnection、adb disconnect。
func (t *pairTab) disconnect() {
	t.cleanup()
	t.latencyMs.Store(0)

	// 清除 UI 編輯器內容，恢復初始狀態
	t.cliOfferOutEditor.SetText("")
	t.cliAnswerInEditor.SetText("")
	t.srvOfferInEditor.SetText("")
	t.srvAnswerOutEditor.SetText("")
	t.srvProcessedOffer = ""
	t.cliProcessedAnswer = ""

	t.mu.Lock()
	t.remoteHostname = ""
	t.remoteAddr = ""
	t.status = "未開始"
	t.mu.Unlock()
	t.window.Invalidate()
}

func (t *pairTab) cleanup() {
	// 先關閉 forward listeners（用獨立鎖）
	t.closeFwdListeners()

	t.mu.Lock()
	port := t.proxyPort
	wasConnected := t.autoConnected
	if t.cancel != nil {
		t.cancel()
	}
	if t.proxyLn != nil {
		t.proxyLn.Close()
		t.proxyLn = nil
	}
	if t.controlCh != nil {
		t.controlCh.Close()
		t.controlCh = nil
	}
	if t.pm != nil {
		t.pm.Close()
	}
	t.connected = false
	t.proxyPort = 0
	t.autoConnected = false
	t.cliDevices = nil
	t.srvDevices = nil
	// 關閉 deviceReadyCh 以解除等待中的 CNXN handler
	if t.deviceReadyCh != nil {
		select {
		case <-t.deviceReadyCh:
		default:
			close(t.deviceReadyCh)
		}
		t.deviceReadyCh = nil
	}
	t.mu.Unlock()

	// 清理 adb connect
	if wasConnected && port > 0 {
		dialer := adb.NewDialer("")
		dialer.Disconnect(fmt.Sprintf("127.0.0.1:%d", port))
	}
}
