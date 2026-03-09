// Package proxy 實作透明 TCP 代理，將本機 TCP 流量轉發到 WebRTC DataChannel。
package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
)

const defaultChunkSize = 16 * 1024 // 16KB

// Proxy 管理單一設備的 TCP 代理：在本機 port 監聽，將流量轉發到 DataChannel。
type Proxy struct {
	listener  net.Listener
	channel   io.ReadWriteCloser
	port      int
	chunkSize int

	cancel context.CancelFunc
	done   chan struct{}
}

// New 建立一個新的 Proxy。
// listenPort 為 0 時會自動分配 port。
func New(listenPort int, channel io.ReadWriteCloser) (*Proxy, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", listenPort))
	if err != nil {
		return nil, fmt.Errorf("監聽 port %d 失敗: %w", listenPort, err)
	}

	// 取得實際分配的 port
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
func (p *Proxy) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)

	go func() {
		defer close(p.done)

		for {
			conn, err := p.listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Debug("Accept 失敗", "error", err)
				return
			}

			slog.Info("新的 ADB 連線", "port", p.port, "remote", conn.RemoteAddr())
			go p.handleConn(ctx, conn)
		}
	}()
}

// handleConn 處理單一 TCP 連線，進行雙向資料泵浦。
func (p *Proxy) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	errc := make(chan error, 2)

	// TCP -> DataChannel（使用 chunking）
	go func() {
		errc <- ChunkedCopy(p.channel, conn, p.chunkSize)
	}()

	// DataChannel -> TCP
	go func() {
		_, err := io.Copy(conn, p.channel)
		errc <- err
	}()

	select {
	case err := <-errc:
		if err != nil {
			slog.Debug("代理傳輸結束", "port", p.port, "error", err)
		}
	case <-ctx.Done():
	}
}

// ChunkedCopy 從 src 讀取資料，以固定大小切片寫入 dst。
// 避免單次發送超過 SCTP MTU 限制。
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
				return nil
			}
			return err
		}
	}
}

// Stop 停止代理。
func (p *Proxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	err := p.listener.Close()
	<-p.done
	return err
}

// Port 回傳正在監聽的 port。
func (p *Proxy) Port() int {
	return p.port
}
