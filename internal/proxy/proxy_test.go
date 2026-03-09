package proxy_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/chris1004tw/remote-adb/internal/proxy"
)

// TestChunkedCopy 驗證 chunkedCopy 正確切片傳輸。
func TestChunkedCopy_SmallData(t *testing.T) {
	src := bytes.NewReader([]byte("hello world"))
	dst := &bytes.Buffer{}

	err := proxy.ChunkedCopy(dst, src, 4) // 4 bytes per chunk
	if err != nil {
		t.Fatalf("ChunkedCopy 失敗: %v", err)
	}

	if dst.String() != "hello world" {
		t.Errorf("結果 = %q, 預期 %q", dst.String(), "hello world")
	}
}

func TestChunkedCopy_LargeData(t *testing.T) {
	// 100KB 的資料，16KB chunks
	data := make([]byte, 100*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	src := bytes.NewReader(data)
	dst := &bytes.Buffer{}

	err := proxy.ChunkedCopy(dst, src, 16*1024)
	if err != nil {
		t.Fatalf("ChunkedCopy 失敗: %v", err)
	}

	if !bytes.Equal(dst.Bytes(), data) {
		t.Error("資料不一致")
	}
}

func TestChunkedCopy_EmptyData(t *testing.T) {
	src := bytes.NewReader([]byte{})
	dst := &bytes.Buffer{}

	err := proxy.ChunkedCopy(dst, src, 1024)
	if err != nil {
		t.Fatalf("ChunkedCopy 失敗: %v", err)
	}
	if dst.Len() != 0 {
		t.Errorf("應為空，但有 %d bytes", dst.Len())
	}
}

// writerRecorder 記錄每次 Write 的大小，用於驗證 chunking。
type writerRecorder struct {
	sizes []int
}

func (w *writerRecorder) Write(p []byte) (int, error) {
	w.sizes = append(w.sizes, len(p))
	return len(p), nil
}

func TestChunkedCopy_RespectChunkSize(t *testing.T) {
	data := make([]byte, 100)
	src := bytes.NewReader(data)
	rec := &writerRecorder{}

	err := proxy.ChunkedCopy(rec, src, 30)
	if err != nil {
		t.Fatalf("ChunkedCopy 失敗: %v", err)
	}

	// 100 bytes / 30 chunk = 4 次寫入（30+30+30+10）
	total := 0
	for _, s := range rec.sizes {
		if s > 30 {
			t.Errorf("Write 大小 %d 超過 chunk size 30", s)
		}
		total += s
	}
	if total != 100 {
		t.Errorf("總寫入量 = %d, 預期 100", total)
	}
}

// pipe 測試：模擬雙向傳輸
func TestBidirectionalCopy(t *testing.T) {
	// 模擬 TCP conn <-> DataChannel
	r1, w1 := io.Pipe() // 方向 1: TCP -> DC
	r2, w2 := io.Pipe() // 方向 2: DC -> TCP

	testData := []byte("bidirectional test data")
	done := make(chan []byte, 1)

	// 讀取方（需讀完全部資料，否則 pipe 會阻塞）
	go func() {
		data, _ := io.ReadAll(r2)
		done <- data
	}()

	// 寫入方（chunked）
	go func() {
		proxy.ChunkedCopy(w2, r1, 8)
		w2.Close()
	}()

	w1.Write(testData)
	w1.Close()

	received := <-done
	if !bytes.Equal(received, testData) {
		t.Errorf("收到 %q, 預期 %q", string(received), string(testData))
	}
}
