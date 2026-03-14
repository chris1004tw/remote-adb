// Package ioutil 提供跨模組共用的 I/O 工具函式。
//
// 此套件集中管理通用的資料複製邏輯，避免各模組重複實作。
// import path 為 "github.com/chris1004tw/remote-adb/internal/ioutil"，
// 不與 Go 標準庫已棄用的 "io/ioutil" 衝突。
package ioutil

import (
	"context"
	"io"
	"sync"
)

// 分級 sync.Pool，依 chunkSize 分開管理 buffer。
//
// 設計意圖：ChunkedCopy 在專案中僅使用兩種固定 chunkSize（16KB 和 32KB），
// 若用通用 pool（以 chunkSize 為 key 的 map 或單一 pool 配最大 size），
// 會引入額外查找開銷或記憶體浪費（小 chunk 拿到大 buffer）。
// 分級 pool 讓每種 size 各自回收，零查找成本、零浪費。
// 非標準 size 直接 make，不進 pool（YAGNI：目前無其他 size 需求）。
var (
	pool16KB = sync.Pool{
		New: func() any {
			// 回傳 *[]byte（指標）而非 []byte，避免 interface boxing 時
			// 對 slice header 產生額外堆分配。
			b := make([]byte, 16*1024)
			return &b
		},
	}
	pool32KB = sync.Pool{
		New: func() any {
			b := make([]byte, 32*1024)
			return &b
		},
	}
)

// BiCopy 在 a 和 b 之間雙向複製資料，使用 chunkSize 分塊。
//
// 設計意圖：統一 agent、directsrv、bridge 的雙向橋接邏輯，
// 確保所有路徑都有分塊保護（DataChannel SCTP 限制）和雙向 Close 保護。
//
// 參數：
//   - ctx：用於取消（ctx.Done() 時結束）
//   - a, b：雙向複製的兩端，結束時會被 Close
//   - chunkSize：每次 Read 的 buffer 上限（DataChannel 建議 16KB，TCP 可用 32KB）
//
// 行為：
//   - 啟動兩個 goroutine 分別執行 a→b 和 b→a 的 ChunkedCopy
//   - 任一方向結束或 ctx 取消時，關閉雙方以解除另一方向的 Read 阻塞
//   - 等待兩個 goroutine 都完成後才返回，避免 goroutine 洩漏
func BiCopy(ctx context.Context, a, b io.ReadWriteCloser, chunkSize int) {
	errc := make(chan error, 2)
	go func() { _, err := ChunkedCopy(a, b, chunkSize); errc <- err }()
	go func() { _, err := ChunkedCopy(b, a, chunkSize); errc <- err }()
	select {
	case <-errc:
	case <-ctx.Done():
	}
	a.Close()
	b.Close()
	<-errc // 等待第二個 goroutine 完成
}

// ChunkedCopy 以固定大小（chunkSize）分塊從 src 讀取並寫入 dst。
//
// 設計意圖：WebRTC DataChannel 底層的 SCTP 協定有訊息大小限制，
// 每次寫入超過 SCTP MTU（約 16KB）會增加分片開銷與記憶體緩衝壓力。
// 使用 chunkSize 大小的 buffer 進行 Read，確保每次 Write 的資料量不超過限制。
//
// 參數：
//   - dst: 寫入目標
//   - src: 讀取來源
//   - chunkSize: 每次 Read 的 buffer 上限（實際讀取量可能小於此值）
//
// 回傳值：
//   - int64: 成功寫入 dst 的總位元組數
//   - error: 非 io.EOF 的讀取或寫入錯誤；src 讀到 EOF 時回傳 nil
//
// Short write 保護：若 dst.Write 回傳的寫入量小於請求量，
// 會迴圈重試直到該塊資料全部寫完。若 Write 回傳 (0, nil)，
// 視為異常行為，回傳 io.ErrShortWrite 防止無限迴圈。
func ChunkedCopy(dst io.Writer, src io.Reader, chunkSize int) (int64, error) {
	// 若 chunkSize 為 16KB 或 32KB，從對應 sync.Pool 取得 buffer 重用，
	// 降低高頻短命連線場景（如 scrcpy 多條 DataChannel）的 GC 壓力。
	// 其他 size 直接 make，不進 pool。
	var buf []byte
	switch chunkSize {
	case 16 * 1024:
		bp := pool16KB.Get().(*[]byte)
		buf = *bp
		defer pool16KB.Put(bp)
	case 32 * 1024:
		bp := pool32KB.Get().(*[]byte)
		buf = *bp
		defer pool32KB.Put(bp)
	default:
		buf = make([]byte, chunkSize)
	}
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			written := 0
			for written < n {
				wn, werr := dst.Write(buf[written:n])
				total += int64(wn)
				if werr != nil {
					return total, werr
				}
				if wn == 0 {
					return total, io.ErrShortWrite
				}
				written += wn
			}
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}
