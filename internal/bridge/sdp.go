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
// # 二進位序列化格式
//
// marshalBinary/unmarshalBinary 將 CompactSDP 編碼為緊湊二進位格式：
//
//	[1B ufragLen][ufrag][1B pwdLen][pwd]
//	[32B fingerprint raw bytes]
//	[1B setup enum: 0=actpass,1=active,2=passive]
//	[1B candidateCount]
//	  每個 candidate:
//	    [1B proto: 0=udp,1=tcp]
//	    [1B ipLen: 4=IPv4, 16=IPv6]
//	    [4/16B IP]
//	    [2B port BE]
//	    [4B priority BE]
//	    [1B typ: 0=host,1=srflx,2=prflx,3=relay]
//	    [1B hasRaddr: 0/1]
//	    若 hasRaddr:
//	      [1B raddrLen: 4/16]
//	      [4/16B raddr]
//	      [2B rport BE]
//
// 本檔案只依賴標準庫，零 GUI 依賴。
package bridge

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
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
func FilterCandidates(candidates []string) []string {
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

// --- 二進位序列化 ---

var setupToEnum = map[string]byte{"actpass": 0, "active": 1, "passive": 2}
var enumToSetup = [...]string{"actpass", "active", "passive"}

var typToEnum = map[string]byte{"host": 0, "srflx": 1, "prflx": 2, "relay": 3}
var enumToTyp = [...]string{"host", "srflx", "prflx", "relay"}

// marshalBinary 將 CompactSDP 編碼為二進位格式。
func marshalBinary(c CompactSDP) ([]byte, error) {
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
		return nil, fmt.Errorf("failed to decode fingerprint hex: %w", err)
	}
	buf.Write(fpBytes)

	// setup
	s, ok := setupToEnum[c.S]
	if !ok {
		return nil, fmt.Errorf("unknown setup value: %q", c.S)
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
		return fmt.Errorf("invalid candidate format: %q", compact)
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
		return fmt.Errorf("invalid IP: %q", parts[1])
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
		return fmt.Errorf("invalid port: %w", err)
	}
	binary.Write(buf, binary.BigEndian, uint16(port))

	// priority
	pri, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil {
		return fmt.Errorf("invalid priority: %w", err)
	}
	binary.Write(buf, binary.BigEndian, uint32(pri))

	// type
	t, ok := typToEnum[parts[4]]
	if !ok {
		return fmt.Errorf("unknown candidate type: %q", parts[4])
	}
	buf.WriteByte(t)

	// raddr/rport
	if len(parts) >= 7 {
		buf.WriteByte(1)
		rip := net.ParseIP(parts[5])
		if rip == nil {
			return fmt.Errorf("invalid raddr: %q", parts[5])
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
			return fmt.Errorf("invalid rport: %w", err)
		}
		binary.Write(buf, binary.BigEndian, uint16(rport))
	} else {
		buf.WriteByte(0)
	}

	return nil
}

// unmarshalBinary 將二進位資料解碼為 CompactSDP。
func unmarshalBinary(data []byte) (CompactSDP, error) {
	r := bytes.NewReader(data)
	var c CompactSDP

	// ufrag
	uLen, err := r.ReadByte()
	if err != nil {
		return c, fmt.Errorf("failed to read ufrag length: %w", err)
	}
	uBuf := make([]byte, uLen)
	if _, err := io.ReadFull(r, uBuf); err != nil {
		return c, fmt.Errorf("failed to read ufrag: %w", err)
	}
	c.U = string(uBuf)

	// pwd
	pLen, err := r.ReadByte()
	if err != nil {
		return c, fmt.Errorf("failed to read pwd length: %w", err)
	}
	pBuf := make([]byte, pLen)
	if _, err := io.ReadFull(r, pBuf); err != nil {
		return c, fmt.Errorf("failed to read pwd: %w", err)
	}
	c.P = string(pBuf)

	// fingerprint: 32 bytes → uppercase hex
	fpBuf := make([]byte, 32)
	if _, err := io.ReadFull(r, fpBuf); err != nil {
		return c, fmt.Errorf("failed to read fingerprint: %w", err)
	}
	c.F = strings.ToUpper(hex.EncodeToString(fpBuf))

	// setup
	sEnum, err := r.ReadByte()
	if err != nil {
		return c, fmt.Errorf("failed to read setup: %w", err)
	}
	if int(sEnum) >= len(enumToSetup) {
		return c, fmt.Errorf("unknown setup enum: %d", sEnum)
	}
	c.S = enumToSetup[sEnum]

	// candidates
	cCount, err := r.ReadByte()
	if err != nil {
		return c, fmt.Errorf("failed to read candidate count: %w", err)
	}
	c.C = make([]string, cCount)
	for i := range c.C {
		cc, err := unmarshalCandidate(r)
		if err != nil {
			return c, fmt.Errorf("failed to decode candidate[%d]: %w", i, err)
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
		return "", fmt.Errorf("failed to read IP: %w", err)
	}

	// port
	var port uint16
	if err := binary.Read(r, binary.BigEndian, &port); err != nil {
		return "", fmt.Errorf("failed to read port: %w", err)
	}

	// priority
	var priority uint32
	if err := binary.Read(r, binary.BigEndian, &priority); err != nil {
		return "", fmt.Errorf("failed to read priority: %w", err)
	}

	// type
	typByte, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	if int(typByte) >= len(enumToTyp) {
		return "", fmt.Errorf("unknown candidate type enum: %d", typByte)
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
			return "", fmt.Errorf("failed to read raddr: %w", err)
		}
		var rport uint16
		if err := binary.Read(r, binary.BigEndian, &rport); err != nil {
			return "", fmt.Errorf("failed to read rport: %w", err)
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

// EncodeToken 將 CompactSDP 編碼為可傳輸的 token 字串。
// 流程：CompactSDP → 二進位序列化 → deflate 壓縮（BestCompression）→ base64url 編碼。
// 使用 RawURLEncoding（無 padding）進一步減少字元數。
func EncodeToken(c CompactSDP) (string, error) {
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

// DecodeToken 是 EncodeToken 的逆操作：base64url 解碼 → deflate 解壓 → 二進位反序列化。
func DecodeToken(token string) (CompactSDP, error) {
	compressed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return CompactSDP{}, fmt.Errorf("failed to decode base64: %w", err)
	}
	r := flate.NewReader(bytes.NewReader(compressed))
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return CompactSDP{}, fmt.Errorf("failed to decompress deflate: %w", err)
	}
	return unmarshalBinary(data)
}
