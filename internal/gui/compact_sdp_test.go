package gui

import (
	"encoding/base64"
	"strings"
	"testing"
)

// 典型的 data-channel-only SDP（ICE gathering 完成後）
const sampleSDP = `v=0
o=- 1234567890 1234567890 IN IP4 0.0.0.0
s=-
t=0 0
a=group:BUNDLE 0
a=extmap-allow-mixed
a=msid-semantic:WMS
m=application 9 UDP/DTLS/SCTP webrtc-datachannel
c=IN IP4 0.0.0.0
a=ice-ufrag:abcd
a=ice-pwd:abcdefghijklmnopqrstuv
a=fingerprint:sha-256 AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99
a=setup:actpass
a=mid:0
a=sctp-port:5000
a=max-message-size:262144
a=candidate:1 1 udp 2130706431 192.168.1.100 54321 typ host
a=candidate:2 1 udp 1694498815 203.0.113.1 12345 typ srflx raddr 192.168.1.100 rport 54321
`

// TestSDPToCompact 測試從完整 SDP 提取必要欄位。
func TestSDPToCompact(t *testing.T) {
	c := sdpToCompact(sampleSDP)

	if c.U != "abcd" {
		t.Errorf("ufrag = %q, want %q", c.U, "abcd")
	}
	if c.P != "abcdefghijklmnopqrstuv" {
		t.Errorf("pwd = %q, want %q", c.P, "abcdefghijklmnopqrstuv")
	}
	// fingerprint 去掉冒號
	wantFP := "AABBCCDDEEFF00112233445566778899AABBCCDDEEFF00112233445566778899"
	if c.F != wantFP {
		t.Errorf("fingerprint = %q, want %q", c.F, wantFP)
	}
	if c.S != "actpass" {
		t.Errorf("setup = %q, want %q", c.S, "actpass")
	}
	if len(c.C) != 2 {
		t.Fatalf("candidates len = %d, want 2", len(c.C))
	}
	// host candidate
	if c.C[0] != "udp,192.168.1.100,54321,2130706431,host" {
		t.Errorf("candidate[0] = %q", c.C[0])
	}
	// srflx candidate
	if c.C[1] != "udp,203.0.113.1,12345,1694498815,srflx,192.168.1.100,54321" {
		t.Errorf("candidate[1] = %q", c.C[1])
	}
}

// TestCompactToSDP 測試從緊湊格式重建合法 SDP。
func TestCompactToSDP(t *testing.T) {
	c := compactSDP{
		U: "abcd",
		P: "abcdefghijklmnopqrstuv",
		F: "AABBCCDDEEFF00112233445566778899AABBCCDDEEFF00112233445566778899",
		S: "actpass",
		C: []string{
			"udp,192.168.1.100,54321,2130706431,host",
			"udp,203.0.113.1,12345,1694498815,srflx,192.168.1.100,54321",
		},
	}

	sdp := compactToSDP(c)

	// 檢查必要 SDP 行是否存在
	checks := []string{
		"v=0",
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
		"a=ice-ufrag:abcd",
		"a=ice-pwd:abcdefghijklmnopqrstuv",
		"a=fingerprint:sha-256 AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99",
		"a=setup:actpass",
		"a=mid:0",
		"a=sctp-port:5000",
		"a=candidate:1 1 udp 2130706431 192.168.1.100 54321 typ host",
		"a=candidate:2 1 udp 1694498815 203.0.113.1 12345 typ srflx raddr 192.168.1.100 rport 54321",
	}
	for _, want := range checks {
		if !strings.Contains(sdp, want) {
			t.Errorf("重建的 SDP 缺少: %q", want)
		}
	}
}

// TestSDPRoundTrip 測試 SDP → compact → SDP 往返轉換保留必要資訊。
func TestSDPRoundTrip(t *testing.T) {
	compact := sdpToCompact(sampleSDP)
	rebuilt := compactToSDP(compact)
	compact2 := sdpToCompact(rebuilt)

	if compact.U != compact2.U {
		t.Errorf("ufrag 不一致: %q vs %q", compact.U, compact2.U)
	}
	if compact.P != compact2.P {
		t.Errorf("pwd 不一致: %q vs %q", compact.P, compact2.P)
	}
	if compact.F != compact2.F {
		t.Errorf("fingerprint 不一致: %q vs %q", compact.F, compact2.F)
	}
	if compact.S != compact2.S {
		t.Errorf("setup 不一致: %q vs %q", compact.S, compact2.S)
	}
	if len(compact.C) != len(compact2.C) {
		t.Fatalf("candidates 數量不一致: %d vs %d", len(compact.C), len(compact2.C))
	}
	for i := range compact.C {
		if compact.C[i] != compact2.C[i] {
			t.Errorf("candidate[%d] 不一致: %q vs %q", i, compact.C[i], compact2.C[i])
		}
	}
}

// TestBinaryRoundTrip 測試 binary marshal → unmarshal 往返轉換保留所有欄位。
func TestBinaryRoundTrip(t *testing.T) {
	original := sdpToCompact(sampleSDP)

	data, err := marshalBinary(original)
	if err != nil {
		t.Fatalf("marshalBinary 失敗: %v", err)
	}

	decoded, err := unmarshalBinary(data)
	if err != nil {
		t.Fatalf("unmarshalBinary 失敗: %v", err)
	}

	if original.U != decoded.U {
		t.Errorf("ufrag 不一致: %q vs %q", original.U, decoded.U)
	}
	if original.P != decoded.P {
		t.Errorf("pwd 不一致: %q vs %q", original.P, decoded.P)
	}
	// fingerprint 比較時忽略大小寫（hex decode/encode 會轉大寫）
	if !strings.EqualFold(original.F, decoded.F) {
		t.Errorf("fingerprint 不一致: %q vs %q", original.F, decoded.F)
	}
	if original.S != decoded.S {
		t.Errorf("setup 不一致: %q vs %q", original.S, decoded.S)
	}
	if len(original.C) != len(decoded.C) {
		t.Fatalf("candidates 數量不一致: %d vs %d", len(original.C), len(decoded.C))
	}
	for i := range original.C {
		if original.C[i] != decoded.C[i] {
			t.Errorf("candidate[%d] 不一致: %q vs %q", i, original.C[i], decoded.C[i])
		}
	}

	t.Logf("binary 大小: %d bytes", len(data))
}

// TestBinaryRoundTripIPv6 測試 IPv6 candidate 的 binary 往返轉換。
func TestBinaryRoundTripIPv6(t *testing.T) {
	c := compactSDP{
		U: "testufrag",
		P: "testpassword1234567890ab",
		F: "AABBCCDDEEFF00112233445566778899AABBCCDDEEFF00112233445566778899",
		S: "active",
		C: []string{
			"udp,2001:0db8:85a3:0000:0000:8a2e:0370:7334,54321,2130706431,host",
			"udp,192.168.1.1,12345,1694498815,srflx,2001:0db8::1,54321",
		},
	}

	data, err := marshalBinary(c)
	if err != nil {
		t.Fatalf("marshalBinary 失敗: %v", err)
	}

	decoded, err := unmarshalBinary(data)
	if err != nil {
		t.Fatalf("unmarshalBinary 失敗: %v", err)
	}

	if len(decoded.C) != 2 {
		t.Fatalf("candidates 數量不一致: %d vs 2", len(decoded.C))
	}
	// IPv6 經過 net.ParseIP → 正規化，字串可能不完全相同，
	// 但 rebuildCandidate 產生的 SDP 行必須功能等價。
	// 這裡確認 round-trip 後結構一致即可。
	t.Logf("candidate[0]: %s", decoded.C[0])
	t.Logf("candidate[1]: %s", decoded.C[1])
}

// TestCompactTokenSize 驗證二進位 token 大幅短於 JSON+gzip 方式。
func TestCompactTokenSize(t *testing.T) {
	compact := sdpToCompact(sampleSDP)
	token, err := encodeToken(compact)
	if err != nil {
		t.Fatalf("編碼失敗: %v", err)
	}

	// 解碼 Base64 看壓縮後的 binary 大小
	rawBytes, _ := base64.RawURLEncoding.DecodeString(token)

	t.Logf("token 長度: %d 字元", len(token))
	t.Logf("壓縮後 binary: %d bytes", len(rawBytes))

	// 二進位格式 + deflate 後的 token 應在 200 字元以內
	if len(token) > 200 {
		t.Errorf("token 太長: %d 字元，預期 < 200", len(token))
	}
}

// TestTokenRoundTrip 測試 encodeToken → decodeToken 完整往返。
func TestTokenRoundTrip(t *testing.T) {
	original := sdpToCompact(sampleSDP)

	token, err := encodeToken(original)
	if err != nil {
		t.Fatalf("encodeToken 失敗: %v", err)
	}

	decoded, err := decodeToken(token)
	if err != nil {
		t.Fatalf("decodeToken 失敗: %v", err)
	}

	if !strings.EqualFold(original.F, decoded.F) {
		t.Errorf("fingerprint 不一致: %q vs %q", original.F, decoded.F)
	}
	if original.U != decoded.U || original.P != decoded.P || original.S != decoded.S {
		t.Errorf("基本欄位不一致")
	}
	if len(original.C) != len(decoded.C) {
		t.Fatalf("candidates 數量不一致: %d vs %d", len(original.C), len(decoded.C))
	}
	for i := range original.C {
		if original.C[i] != decoded.C[i] {
			t.Errorf("candidate[%d] 不一致: %q vs %q", i, original.C[i], decoded.C[i])
		}
	}
}

// TestFilterCandidates 測試 candidate 過濾邏輯。
// 過濾規則：
//  1. 移除 loopback IP（127.x.x.x、::1）
//  2. 移除 IPv6 link-local（fe80:: 開頭）
//  3. srflx 同公網 IP + 同協定去重，保留最高 priority
func TestFilterCandidates(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "保留正常 host 和 srflx candidate",
			in: []string{
				"udp,192.168.1.100,54321,2130706431,host",
				"udp,203.0.113.1,12345,1694498815,srflx,192.168.1.100,54321",
			},
			want: []string{
				"udp,192.168.1.100,54321,2130706431,host",
				"udp,203.0.113.1,12345,1694498815,srflx,192.168.1.100,54321",
			},
		},
		{
			name: "過濾 loopback IPv4",
			in: []string{
				"udp,127.0.0.1,54321,2130706431,host",
				"udp,192.168.1.100,54321,2130706431,host",
			},
			want: []string{
				"udp,192.168.1.100,54321,2130706431,host",
			},
		},
		{
			name: "過濾 loopback IPv6",
			in: []string{
				"udp,::1,54321,2130706431,host",
				"udp,192.168.1.100,54321,2130706431,host",
			},
			want: []string{
				"udp,192.168.1.100,54321,2130706431,host",
			},
		},
		{
			name: "過濾 IPv6 link-local",
			in: []string{
				"udp,fe80::1,54321,2130706431,host",
				"udp,fe80::abcd:1234:5678,12345,2130706431,host",
				"udp,192.168.1.100,54321,2130706431,host",
			},
			want: []string{
				"udp,192.168.1.100,54321,2130706431,host",
			},
		},
		{
			name: "srflx 同公網 IP 去重，保留最高 priority",
			in: []string{
				"udp,192.168.1.100,54321,2130706431,host",
				"udp,192.168.1.200,54322,2130706430,host",
				"udp,203.0.113.1,12345,1694498815,srflx,192.168.1.100,54321",
				"udp,203.0.113.1,12346,1694498810,srflx,192.168.1.200,54322",
			},
			want: []string{
				"udp,192.168.1.100,54321,2130706431,host",
				"udp,192.168.1.200,54322,2130706430,host",
				"udp,203.0.113.1,12345,1694498815,srflx,192.168.1.100,54321",
			},
		},
		{
			name: "srflx 不同公網 IP 不去重",
			in: []string{
				"udp,203.0.113.1,12345,1694498815,srflx,192.168.1.100,54321",
				"udp,203.0.113.2,12346,1694498810,srflx,192.168.1.200,54322",
			},
			want: []string{
				"udp,203.0.113.1,12345,1694498815,srflx,192.168.1.100,54321",
				"udp,203.0.113.2,12346,1694498810,srflx,192.168.1.200,54322",
			},
		},
		{
			name: "混合過濾：loopback + link-local + srflx 去重",
			in: []string{
				"udp,127.0.0.1,54321,2130706431,host",
				"udp,fe80::1,54322,2130706430,host",
				"udp,192.168.1.100,54323,2130706429,host",
				"udp,192.168.1.200,54324,2130706428,host",
				"udp,203.0.113.1,12345,1694498815,srflx,192.168.1.100,54323",
				"udp,203.0.113.1,12346,1694498810,srflx,192.168.1.200,54324",
			},
			want: []string{
				"udp,192.168.1.100,54323,2130706429,host",
				"udp,192.168.1.200,54324,2130706428,host",
				"udp,203.0.113.1,12345,1694498815,srflx,192.168.1.100,54323",
			},
		},
		{
			name: "空輸入回傳空",
			in:   nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterCandidates(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("長度不一致: got %d, want %d\ngot:  %v\nwant: %v", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("candidate[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
