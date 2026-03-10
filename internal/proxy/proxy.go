// Package proxy 實作透明 TCP 代理，將本機 TCP 流量轉發到 WebRTC DataChannel。
//
// 設計重點：同一時間只允許一條 TCP 連線使用共用的 DataChannel。
//
// 為何採用單連線設計？
// ADB device transport 協定是在單一 TCP 連線上進行多工，如果兩條 TCP 連線
// 同時對同一個 DataChannel 讀寫，位元串流會交錯導致協定解析錯誤。
// 因此當新的 ADB 連線到達時（例如使用者重新執行 adb shell），代理會：
//  1. 關閉舊的 TCP 連線
//  2. 等待舊連線的寫入 goroutine 完全退出（確保 channel 不再被舊連線寫入）
//  3. 才啟動新連線的轉發
//
// 雙向橋接架構：
//   - channelToConn goroutine（全域唯一）：DataChannel → 當前活躍 TCP 連線
//   - connToChannel goroutine（每條連線一個）：TCP 連線 → DataChannel
package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
)

// defaultChunkSize 定義每次寫入 DataChannel 的最大資料塊大小。
// WebRTC DataChannel 底層使用 SCTP 協定，SCTP 的 MTU 通常在 16KB 左右。
// 若單次寫入超過此大小，pion 會自動分片，但可能造成效能損耗或緩衝區壓力。
// 16KB 是在吞吐量與分片開銷之間的平衡值。
const defaultChunkSize = 16 * 1024 // 16KB

// Proxy 管理單一設備的 TCP 代理：在本機 port 監聽，將流量轉發到 DataChannel。
//
// 同一時間只有一條活躍連線可以使用 channel，避免多連線同時讀寫造成資料交錯。
//
// goroutine 架構：
//   - Accept loop（Start 內的匿名 goroutine）：接受新連線、替換舊連線
//   - channelToConn（唯一一個）：持續從 channel 讀取 → 寫入 active conn
//   - connToChannel（每條連線一個，但同時間只有一個在執行）：從 conn 讀取 → 寫入 channel
type Proxy struct {
	listener  net.Listener       // 本機 TCP 監聽器，綁定在 127.0.0.1
	channel   io.ReadWriteCloser // WebRTC DataChannel（detach 後的 io.ReadWriteCloser）
	port      int                // 實際監聽的 port 號
	chunkSize int                // 每次寫入 channel 的最大位元組數（預設 16KB）

	cancel context.CancelFunc // 取消 Start 建立的 context，觸發所有 goroutine 退出
	done   chan struct{}       // Start 的 Accept loop 結束時 close，供 Stop 等待

	mu     sync.Mutex // 保護 active 欄位的並行存取
	active net.Conn   // 當前活躍的 TCP 連線；nil 表示無活躍連線
}

// New 建立一個新的 Proxy。
// listenPort 為 0 時由作業系統自動分配可用 port（Daemon 動態分配場景）。
// 僅綁定 127.0.0.1，避免暴露 ADB 連線給區網其他主機。
func New(listenPort int, channel io.ReadWriteCloser) (*Proxy, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", listenPort))
	if err != nil {
		return nil, fmt.Errorf("監聽 port %d 失敗: %w", listenPort, err)
	}

	// 取得實際分配的 port（listenPort 為 0 時需要查詢真實 port）
	actualPort := listener.Addr().(*net.TCPAddr).Port

	return &Proxy{
		listener:  listener,
		channel:   channel,
		port:      actualPort,
		chunkSize: defaultChunkSize,
		done:      make(chan struct{}),
	}, nil
}

// Start 開始接受 TCP 連線並轉發。
//
// 同一時間只處理一條連線：新連線到達時，關閉舊連線並等待其寫入器結束，
// 確保 channel 不會被多個 goroutine 同時寫入。
//
// 連線替換流程（當新連線到達時）：
//  1. 將 active 設為 nil → channelToConn 讀到資料時發現無活躍連線，直接丟棄
//  2. 關閉舊 conn → 舊 connToChannel 的 Read 會收到 EOF，goroutine 退出
//  3. 等待 prevDone channel close → 確保舊 goroutine 已完全退出，不再寫入 channel
//  4. 設定新 conn 為 active → channelToConn 開始轉發資料到新連線
//  5. 啟動新的 connToChannel goroutine
//
// 為何不用 mutex 保護 channel 寫入？
// 透過 prevDone 等待機制，保證新舊 connToChannel 不會同時存在，
// 因此不需要額外的鎖來保護 channel 寫入端。
func (p *Proxy) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)

	// 啟動唯一的 channelToConn goroutine：負責 DataChannel → TCP 方向
	// 此 goroutine 與 Proxy 生命週期相同，不隨連線替換而重建
	go p.channelToConn(ctx)

	go func() {
		defer close(p.done)

		// prevDone 追蹤前一個 connToChannel goroutine 的結束狀態
		var prevDone chan struct{}

		for {
			conn, err := p.listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Debug("Accept 失敗", "error", err)
				return
			}

			slog.Debug("ADB 連線", "port", p.port, "remote", conn.RemoteAddr())

			// === 連線替換步驟 1-3：清理舊連線 ===
			// 先將 active 設為 nil，防止 channelToConn 將資料寫入即將關閉的舊 conn
			p.mu.Lock()
			old := p.active
			p.active = nil
			p.mu.Unlock()
			if old != nil {
				old.Close() // 觸發舊 connToChannel 收到讀取錯誤並退出
			}
			if prevDone != nil {
				<-prevDone // 阻塞等待舊 connToChannel goroutine 完全結束
			}

			// === 連線替換步驟 4-5：啟動新連線 ===
			p.mu.Lock()
			p.active = conn
			p.mu.Unlock()

			done := make(chan struct{})
			prevDone = done
			go func(c net.Conn, d chan struct{}) {
				defer close(d) // 通知下一次替換：此 goroutine 已結束
				p.connToChannel(c)
			}(conn, done)
		}
	}()
}

// channelToConn 從 DataChannel 讀取資料，寫入當前活躍的 TCP 連線。
//
// 生命週期：與 Proxy 相同，從 Start 到 Stop 只有一個此 goroutine 存在。
// 這確保 channel 的讀取端不會被多個 goroutine 競爭。
//
// 當連線替換時（active 暫時為 nil），從 channel 讀到的資料會被丟棄。
// 這是可接受的，因為舊連線的殘留資料對新連線沒有意義。
func (p *Proxy) channelToConn(ctx context.Context) {
	buf := make([]byte, 32*1024)
	for {
		n, err := p.channel.Read(buf)
		if n > 0 {
			// 取得當前活躍連線的快照（可能為 nil，表示正在替換中）
			p.mu.Lock()
			conn := p.active
			p.mu.Unlock()
			if conn != nil {
				if _, writeErr := conn.Write(buf[:n]); writeErr != nil {
					slog.Debug("寫入 TCP 失敗", "port", p.port, "error", writeErr)
				}
			}
			// conn == nil 時資料被丟棄，避免寫入已關閉的舊連線
		}
		if err != nil {
			if ctx.Err() == nil {
				slog.Debug("channel 讀取結束", "port", p.port, "error", err)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// connToChannel 從 TCP 連線讀取資料，以 ChunkedCopy 分塊寫入 DataChannel。
//
// 生命週期：每條 TCP 連線產生一個 goroutine，當連線關閉（正常 EOF 或被替換強制關閉）
// 時自動退出。透過 Start 中的 prevDone 機制，保證同一時間只有一個此 goroutine 在寫入 channel。
//
// defer 中的清理邏輯：
//   - conn.Close()：確保連線資源被釋放（可能已被 Start 的替換流程關閉，Close 是冪等的）
//   - 如果 active 仍指向此 conn，將其設為 nil（正常結束，非被替換的情況）
func (p *Proxy) connToChannel(conn net.Conn) {
	defer func() {
		conn.Close()
		p.mu.Lock()
		if p.active == conn {
			p.active = nil
		}
		p.mu.Unlock()
	}()
	ChunkedCopy(p.channel, conn, p.chunkSize)
}

// ChunkedCopy 從 src 讀取資料，以固定大小的 buffer 分塊寫入 dst。
//
// 為何需要分塊？
// WebRTC DataChannel 底層的 SCTP 協定有訊息大小限制。雖然 pion 會自動處理
// 超大訊息的分片與重組，但每次寫入的資料量若超過 SCTP MTU（約 16KB），
// 會增加分片開銷與記憶體緩衝壓力。使用 chunkSize 大小的 buffer 進行 Read，
// 確保每次 Write 的資料量不超過限制，兼顧效能與穩定性。
//
// 注意：src.Read 本身可能回傳小於 chunkSize 的資料，這是正常行為。
// chunkSize 只是 buffer 上限，實際寫入量以 Read 回傳的 n 為準。
func ChunkedCopy(dst io.Writer, src io.Reader, chunkSize int) error {
	buf := make([]byte, chunkSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil // 正常結束：來源端關閉
			}
			return err
		}
	}
}

// Stop 停止代理，釋放所有資源。
//
// 關閉順序：
//  1. cancel context → 通知 channelToConn 檢查 ctx.Err() 並退出
//  2. 關閉活躍 TCP 連線 → 觸發 connToChannel 收到讀取錯誤並退出
//  3. 關閉 listener → 觸發 Accept loop 收到錯誤並退出
//  4. 等待 done channel → 確保 Accept loop 已完全結束
func (p *Proxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	// 關閉活躍連線，讓 connToChannel 退出
	p.mu.Lock()
	if p.active != nil {
		p.active.Close()
	}
	p.mu.Unlock()
	err := p.listener.Close()
	<-p.done // 等待 Accept loop goroutine 結束
	return err
}

// Port 回傳正在監聽的 port 號。
// 當建構時指定 listenPort=0（自動分配），可透過此方法取得實際 port。
func (p *Proxy) Port() int {
	return p.port
}
