package bridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/chris1004tw/remote-adb/internal/ioutil"
)

// ADB device transport 協定常數（little-endian wire representation）。
// 這些常數的值是 ASCII 字串的 little-endian 32-bit 表示（如 "CNXN" -> 0x4e584e43）。
const (
	aCNXN = 0x4e584e43 // "CNXN" — 連線握手
	aAUTH = 0x48545541 // "AUTH" — 認證（本實作跳過）
	aOPEN = 0x4e45504f // "OPEN" — 開啟新串流
	aOKAY = 0x59414b4f // "OKAY" — 確認/流控
	aWRTE = 0x45545257 // "WRTE" — 寫入資料
	aCLSE = 0x45534c43 // "CLSE" — 關閉串流

	aVersion      = 0x01000001  // A_VERSION_SKIP_CHECKSUM：version >= 此值時不驗證 checksum
	aMaxPayload   = 256 * 1024  // 256KB：單次 WRTE 最大 payload（CNXN 握手通告值）
	adbMsgHdrSize = 24          // 固定 24 byte header
	adbMaxDataLen = 1024 * 1024 // 安全上限 1MB，防止惡意或損壞的資料長度
	BiCopyChunk   = 16 * 1024   // 16KB：DataChannel 分塊寫入大小
)

// 預設 device banner：CNXN 回應的 banner 字串，包含常用 ADB features。
// 若無法取得真實設備的 features，則使用此保守預設值。
// features 決定 adb client 可使用的功能（如 shell_v2 支援互動式 shell、cmd 支援 pm/am 等）。
const defaultDeviceBanner = "device::features=shell_v2,cmd,stat_v2,ls_v2,fixed_push_mkdir,sendrecv_v2,sendrecv_v2_brotli,sendrecv_v2_lz4,sendrecv_v2_zstd"

// adbMsg 表示一條 ADB device transport 訊息（對應 24 byte header + payload）。
// command 為訊息類型（CNXN/OPEN/OKAY/WRTE/CLSE），
// arg0/arg1 的語義取決於 command（通常為 localID/remoteID）。
type adbMsg struct {
	command uint32
	arg0    uint32
	arg1    uint32
	data    []byte
}

// adbCmdName 回傳 ADB command 的可讀名稱。
func adbCmdName(cmd uint32) string {
	switch cmd {
	case aCNXN:
		return "CNXN"
	case aAUTH:
		return "AUTH"
	case aOPEN:
		return "OPEN"
	case aOKAY:
		return "OKAY"
	case aWRTE:
		return "WRTE"
	case aCLSE:
		return "CLSE"
	default:
		return fmt.Sprintf("0x%08x", cmd)
	}
}

// readADBTransportMsg 從 reader 讀取完整的 ADB transport 訊息（24 byte header + payload）。
// header 的 [12:16] 為 payload 長度，超過 adbMaxDataLen 時回傳錯誤（防止記憶體爆炸）。
func readADBTransportMsg(r io.Reader) (*adbMsg, error) {
	var hdr [adbMsgHdrSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	msg := &adbMsg{
		command: binary.LittleEndian.Uint32(hdr[0:4]),
		arg0:    binary.LittleEndian.Uint32(hdr[4:8]),
		arg1:    binary.LittleEndian.Uint32(hdr[8:12]),
	}
	dataLen := binary.LittleEndian.Uint32(hdr[12:16])
	if dataLen > adbMaxDataLen {
		return nil, fmt.Errorf("ADB message data too large: %d bytes", dataLen)
	}
	if dataLen > 0 {
		msg.data = make([]byte, dataLen)
		if _, err := io.ReadFull(r, msg.data); err != nil {
			return nil, err
		}
	}
	return msg, nil
}

// readADBMsgFromPrefix 讀取 ADB transport 訊息，前 4 bytes（command）已由 handleProxyConn 提前讀取。
// 用於 CNXN 訊息：handleProxyConn 需要先讀 4 bytes 判斷是 "CNXN" 還是 hex 長度。
func readADBMsgFromPrefix(prefix []byte, r io.Reader) (*adbMsg, error) {
	var rest [adbMsgHdrSize - 4]byte
	if _, err := io.ReadFull(r, rest[:]); err != nil {
		return nil, err
	}
	msg := &adbMsg{
		command: binary.LittleEndian.Uint32(prefix),
		arg0:    binary.LittleEndian.Uint32(rest[0:4]),
		arg1:    binary.LittleEndian.Uint32(rest[4:8]),
	}
	dataLen := binary.LittleEndian.Uint32(rest[8:12])
	if dataLen > adbMaxDataLen {
		return nil, fmt.Errorf("ADB message data too large: %d bytes", dataLen)
	}
	if dataLen > 0 {
		msg.data = make([]byte, dataLen)
		if _, err := io.ReadFull(r, msg.data); err != nil {
			return nil, err
		}
	}
	return msg, nil
}

// writeADBTransportMsg 將 adbMsg 編碼為 24 byte header + payload 並寫入 writer。
// header[16:20] 的 checksum 設為 0（因為 aVersion >= A_VERSION_SKIP_CHECKSUM）。
// header[20:24] 是 magic（command XOR 0xFFFFFFFF），用於基本的訊息完整性檢查。
func writeADBTransportMsg(w io.Writer, msg *adbMsg) error {
	var hdr [adbMsgHdrSize]byte
	binary.LittleEndian.PutUint32(hdr[0:4], msg.command)
	binary.LittleEndian.PutUint32(hdr[4:8], msg.arg0)
	binary.LittleEndian.PutUint32(hdr[8:12], msg.arg1)
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(len(msg.data)))
	// checksum: 0（version >= 0x01000001 不驗證）
	binary.LittleEndian.PutUint32(hdr[16:20], 0)
	binary.LittleEndian.PutUint32(hdr[20:24], msg.command^0xFFFFFFFF)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(msg.data) > 0 {
		_, err := w.Write(msg.data)
		return err
	}
	return nil
}

// nopRWC 是不做任何事的 io.ReadWriteCloser。
// 用於 sendOneShot 的臨時 stream，因為 one-shot 命令不需要真正的 DataChannel，
// 只需要走完 OKAY -> WRTE -> OKAY -> CLSE 的 transport 流程。
type nopRWC struct{}

func (nopRWC) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopRWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopRWC) Close() error                { return nil }

// PrefixedRWC 包裝一個 ReadWriteCloser，Read 時先回傳 prefix 再讀取底層 ch。
// 用途：setupStream 等待就緒信號時，遠端可能在同一個 SCTP 訊息中同時送出
// 就緒位元和第一筆資料。prefix 保存這些「多讀」的資料，避免遺失首包。
type PrefixedRWC struct {
	Ch     io.ReadWriteCloser
	Prefix []byte
	Off    int
}

func (p *PrefixedRWC) Read(buf []byte) (int, error) {
	if p.Off < len(p.Prefix) {
		n := copy(buf, p.Prefix[p.Off:])
		p.Off += n
		return n, nil
	}
	return p.Ch.Read(buf)
}

func (p *PrefixedRWC) Write(buf []byte) (int, error) { return p.Ch.Write(buf) }
func (p *PrefixedRWC) Close() error                  { return p.Ch.Close() }

// BiCopy 在兩個 ReadWriteCloser 之間雙向複製資料，使用 BiCopyChunk (16KB) 分塊。
// 委派給 ioutil.BiCopy，保持 bridge 套件內部呼叫端簽名不變。
func BiCopy(ctx context.Context, a, b io.ReadWriteCloser) {
	ioutil.BiCopy(ctx, a, b, BiCopyChunk)
}
