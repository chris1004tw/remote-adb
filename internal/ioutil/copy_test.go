package ioutil_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/chris1004tw/remote-adb/internal/ioutil"
)

// TestChunkedCopy_SmallData 驗證小資料正確切片傳輸。
func TestChunkedCopy_SmallData(t *testing.T) {
	src := bytes.NewReader([]byte("hello world"))
	dst := &bytes.Buffer{}

	n, err := ioutil.ChunkedCopy(dst, src, 4) // 4 bytes per chunk
	if err != nil {
		t.Fatalf("ChunkedCopy 失敗: %v", err)
	}
	if n != 11 {
		t.Errorf("回傳位元組數 = %d, 預期 11", n)
	}
	if dst.String() != "hello world" {
		t.Errorf("結果 = %q, 預期 %q", dst.String(), "hello world")
	}
}

// TestChunkedCopy_LargeData 驗證 100KB 大資料完整傳輸。
func TestChunkedCopy_LargeData(t *testing.T) {
	data := make([]byte, 100*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	src := bytes.NewReader(data)
	dst := &bytes.Buffer{}

	n, err := ioutil.ChunkedCopy(dst, src, 16*1024)
	if err != nil {
		t.Fatalf("ChunkedCopy 失敗: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("回傳位元組數 = %d, 預期 %d", n, len(data))
	}
	if !bytes.Equal(dst.Bytes(), data) {
		t.Error("資料不一致")
	}
}

// TestChunkedCopy_EmptyData 驗證空資料不產生輸出。
func TestChunkedCopy_EmptyData(t *testing.T) {
	src := bytes.NewReader([]byte{})
	dst := &bytes.Buffer{}

	n, err := ioutil.ChunkedCopy(dst, src, 1024)
	if err != nil {
		t.Fatalf("ChunkedCopy 失敗: %v", err)
	}
	if n != 0 {
		t.Errorf("回傳位元組數 = %d, 預期 0", n)
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

// TestChunkedCopy_RespectChunkSize 驗證每次 Write 不超過 chunkSize。
func TestChunkedCopy_RespectChunkSize(t *testing.T) {
	data := make([]byte, 100)
	src := bytes.NewReader(data)
	rec := &writerRecorder{}

	n, err := ioutil.ChunkedCopy(rec, src, 30)
	if err != nil {
		t.Fatalf("ChunkedCopy 失敗: %v", err)
	}
	if n != 100 {
		t.Errorf("回傳位元組數 = %d, 預期 100", n)
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

// TestChunkedCopy_Bidirectional 驗證透過 pipe 傳輸時資料完整性。
func TestChunkedCopy_Bidirectional(t *testing.T) {
	testData := []byte("bidirectional test data")

	r, w := io.Pipe()
	done := make(chan []byte, 1)

	// 讀取端：從 pipe 讀取所有資料
	go func() {
		data, _ := io.ReadAll(r)
		done <- data
	}()

	// 寫入端：透過 ChunkedCopy 分塊寫入 pipe
	go func() {
		ioutil.ChunkedCopy(w, bytes.NewReader(testData), 8)
		w.Close()
	}()

	received := <-done
	if !bytes.Equal(received, testData) {
		t.Errorf("收到 %q, 預期 %q", string(received), string(testData))
	}
}

// errWriter 模擬寫入錯誤的 Writer。
type errWriter struct {
	err error
}

func (w *errWriter) Write([]byte) (int, error) {
	return 0, w.err
}

// TestChunkedCopy_WriteError 驗證寫入錯誤正確回傳。
func TestChunkedCopy_WriteError(t *testing.T) {
	writeErr := errors.New("write failed")
	src := bytes.NewReader([]byte("some data"))
	dst := &errWriter{err: writeErr}

	_, err := ioutil.ChunkedCopy(dst, src, 4)
	if !errors.Is(err, writeErr) {
		t.Errorf("預期 write error，但收到 %v", err)
	}
}

// errReader 模擬讀取錯誤的 Reader。
type errReader struct {
	err error
}

func (r *errReader) Read([]byte) (int, error) {
	return 0, r.err
}

// TestChunkedCopy_ReadError 驗證讀取錯誤正確回傳。
func TestChunkedCopy_ReadError(t *testing.T) {
	readErr := errors.New("read failed")
	src := &errReader{err: readErr}
	dst := &bytes.Buffer{}

	_, err := ioutil.ChunkedCopy(dst, src, 4)
	if !errors.Is(err, readErr) {
		t.Errorf("預期 read error，但收到 %v", err)
	}
}

// shortWriter 模擬每次只寫入部分資料的 Writer，用於驗證 short write 迴圈重試。
// 每次 Write 最多寫入 maxPerWrite 位元組，模擬底層 Writer 的分段行為。
type shortWriter struct {
	buf         bytes.Buffer
	maxPerWrite int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n > w.maxPerWrite {
		n = w.maxPerWrite
	}
	return w.buf.Write(p[:n])
}

// TestChunkedCopy_ShortWrite 驗證 Writer 每次只寫入部分資料時，
// ChunkedCopy 會正確迴圈重試直到所有資料寫完。
func TestChunkedCopy_ShortWrite(t *testing.T) {
	data := []byte("hello world, this is a short write test!")
	src := bytes.NewReader(data)
	dst := &shortWriter{maxPerWrite: 3} // 每次最多寫 3 bytes

	n, err := ioutil.ChunkedCopy(dst, src, 10)
	if err != nil {
		t.Fatalf("ChunkedCopy 失敗: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("回傳位元組數 = %d, 預期 %d", n, len(data))
	}
	if !bytes.Equal(dst.buf.Bytes(), data) {
		t.Errorf("結果 = %q, 預期 %q", dst.buf.String(), string(data))
	}
}

// zeroWriter 模擬 Write 回傳 0 且無 error 的異常 Writer，
// 應觸發 io.ErrShortWrite 防止無限迴圈。
type zeroWriter struct{}

func (w *zeroWriter) Write([]byte) (int, error) {
	return 0, nil
}

// TestChunkedCopy_ZeroWrite 驗證 Writer 回傳 (0, nil) 時回傳 io.ErrShortWrite。
func TestChunkedCopy_ZeroWrite(t *testing.T) {
	src := bytes.NewReader([]byte("data"))
	dst := &zeroWriter{}

	_, err := ioutil.ChunkedCopy(dst, src, 4)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("預期 io.ErrShortWrite，但收到 %v", err)
	}
}
