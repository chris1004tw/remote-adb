// sdp.go 實作 SDP 緊湊編碼（compactSDP），用於 WebRTC 手動配對的 token 交換。
//
// # SDP 緊湊格式
//
// 完整的 WebRTC SDP 包含大量樣板行（v=, o=, s=, m= 等固定內容），
// 實際需要交換的只有 ice-ufrag、ice-pwd、fingerprint、setup role 和 candidates。
// CompactSDP 只保留這些必要欄位，配合二進位序列化 + deflate 壓縮 + base64 編碼，
// 將 token 長度壓縮到約 100-200 字元，方便使用者手動複製貼上。
//
// # ICE Candidate 過濾
//
// FilterCandidates 過濾無用的 candidate（loopback、link-local、重複 srflx），
// 減少 token 長度。
//
// 二進位序列化格式與 Token 壓縮邏輯見 sdp_codec.go。
//
// 本檔案只依賴標準庫，零 GUI 依賴。
package bridge

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// CompactSDP 只保留 WebRTC data-channel SDP 的必要欄位。
// 完整 SDP 的樣板行（v=, o=, s=, m=application 等）可從預設值重建。
type CompactSDP struct {
	U string   `json:"u"` // ice-ufrag
	P string   `json:"p"` // ice-pwd
	F string   `json:"f"` // fingerprint hash（hex，無冒號）
	S string   `json:"s"` // setup role（actpass/active/passive）
	C []string `json:"c"` // candidates，格式: proto,ip,port,priority,type[,raddr,rport]
}

// SDPToCompact 從完整 SDP 提取必要欄位，產生 CompactSDP。
func SDPToCompact(sdp string) CompactSDP {
	var c CompactSDP
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
	c.C = FilterCandidates(c.C)
	return c
}

// FilterCandidates 過濾無用的 ICE candidate，減少 token 長度。
//
// 過濾規則：
//  1. 移除 loopback IP（127.x.x.x、::1）— 遠端連線無用
//  2. 移除 IPv6 link-local（fe80:: 開頭）— 跨 NAT 無用
//  3. srflx 同公網 IP + 同協定去重，保留最高 priority —
//     多張網卡映射到相同公網 IP 時只需保留一個 srflx candidate
//
// 設計意圖：減少手動複製 token 的長度，從 ~375 字元降至接近 200 字元。
// 每條 candidate 字串只做一次 strings.Split 解析，避免重複分割。
func FilterCandidates(candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}

	// candidateEntry 為已解析的 candidate 欄位，避免同一條字串重複 Split。
	type candidateEntry struct {
		raw      string // 原始字串，最終結果直接取用
		ip       net.IP // 解析後的 IP，用於 loopback/link-local 判斷
		proto    string // 協定（udp/tcp），srflx 去重 key 組成
		priority uint64 // 優先度，srflx 去重時保留最高者
		typ      string // candidate 類型（host/srflx/prflx/relay）
	}

	// 第一步：一次解析全部 candidate，同時過濾 loopback 和 link-local
	var entries []candidateEntry
	for _, c := range candidates {
		parts := strings.Split(c, ",")
		if len(parts) < 5 {
			continue
		}
		ip := net.ParseIP(parts[1])
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		pri, _ := strconv.ParseUint(parts[3], 10, 64)
		entries = append(entries, candidateEntry{
			raw:      c,
			ip:       ip,
			proto:    parts[0],
			priority: pri,
			typ:      parts[4],
		})
	}

	// 第二步：srflx 同公網 IP + 同協定去重，保留最高 priority
	// key = "proto,ip"（srflx 的公網 IP），value = 該 key 中最高 priority 的索引
	type srflxBest struct {
		idx      int    // entries 中的索引
		priority uint64 // 該 key 中目前最高的 priority
	}
	bestSrflx := make(map[string]srflxBest)

	for i, e := range entries {
		if e.typ != "srflx" {
			continue
		}
		key := e.proto + "," + e.ip.String()
		if existing, ok := bestSrflx[key]; !ok || e.priority > existing.priority {
			bestSrflx[key] = srflxBest{idx: i, priority: e.priority}
		}
	}

	// 建立保留的 srflx 索引集合
	keepIdx := make(map[int]bool, len(bestSrflx))
	for _, best := range bestSrflx {
		keepIdx[best.idx] = true
	}

	// 第三步：組裝結果——非 srflx 全部保留，srflx 只保留去重勝出者
	var result []string
	for i, e := range entries {
		if e.typ == "srflx" {
			if !keepIdx[i] {
				continue
			}
		}
		result = append(result, e.raw)
	}
	return result
}

// CandidateStats 統計 CompactSDP 中各類型 ICE candidate 的數量。
// 回傳 host、srflx、relay 的計數，用於日誌記錄和排查 TURN fallback 問題。
func (c CompactSDP) CandidateStats() (host, srflx, relay int) {
	for _, cand := range c.C {
		parts := strings.SplitN(cand, ",", 6)
		if len(parts) < 5 {
			continue
		}
		switch parts[4] {
		case "host":
			host++
		case "srflx":
			srflx++
		case "relay":
			relay++
		}
	}
	return
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

// CompactToSDP 從緊湊格式重建合法的 data-channel SDP。
func CompactToSDP(c CompactSDP) string {
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
func insertColons(hexStr string) string {
	var parts []string
	for i := 0; i+2 <= len(hexStr); i += 2 {
		parts = append(parts, hexStr[i:i+2])
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
