// sdp_codec.go 實作 CompactSDP 的二進位序列化與 Token 壓縮/解壓。
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
// # Token 編碼流程
//
// EncodeToken: CompactSDP → marshalBinary → deflate BestCompression → base64url（無 padding）
// DecodeToken: base64url 解碼 → deflate 解壓 → unmarshalBinary
//
// SDP 文字轉換邏輯見 sdp.go。
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
// 空 token 回傳明確錯誤，避免空位元組送入 deflate 後產生誤導性的 "unexpected EOF"。
func DecodeToken(token string) (CompactSDP, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return CompactSDP{}, fmt.Errorf("token is empty")
	}
	compressed, err := base64.RawURLEncoding.DecodeString(token)
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
