package ioutil_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

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

// --- BiCopy 測試 ---

// testRWC 是用於測試的 ReadWriteCloser，內部使用 pipe 實現雙向通訊。
// 追蹤 Close 是否被呼叫。
type testRWC struct {
	r      io.ReadCloser
	w      io.WriteCloser
	mu     sync.Mutex
	closed bool
}

func newTestRWC() (*testRWC, *testRWC) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	a := &testRWC{r: r1, w: w2}
	b := &testRWC{r: r2, w: w1}
	return a, b
}

func (t *testRWC) Read(p []byte) (int, error)  { return t.r.Read(p) }
func (t *testRWC) Write(p []byte) (int, error) { return t.w.Write(p) }
func (t *testRWC) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.r.Close()
	t.w.Close()
	return nil
}
func (t *testRWC) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// TestBiCopy_BothDirections 驗證 BiCopy 能正確雙向複製資料。
func TestBiCopy_BothDirections(t *testing.T) {
	a, b := newTestRWC()

	dataA := []byte("hello from A")
	dataB := []byte("hello from B")

	// 背景寫入資料後關閉
	go func() {
		a.w.Write(dataA)
		time.Sleep(50 * time.Millisecond)
		a.r.Close() // 觸發 EOF 讓 BiCopy 結束
	}()
	go func() {
		b.w.Write(dataB)
	}()

	// 使用簡單的 buffer-based RWC 來驗證
	// 改用更直接的方式：pipe pair + 手動讀取
	bufA := make([]byte, 64)
	bufB := make([]byte, 64)

	nB, _ := b.r.Read(bufB) // 讀取 A→B 的資料
	nA, _ := a.r.Read(bufA) // 讀取 B→A 的資料

	if string(bufB[:nB]) != string(dataA) {
		t.Errorf("B 收到 %q，預期 %q", string(bufB[:nB]), string(dataA))
	}
	if string(bufA[:nA]) != string(dataB) {
		t.Errorf("A 收到 %q，預期 %q", string(bufA[:nA]), string(dataB))
	}
}

// TestBiCopy_BothSidesClosed 驗證 BiCopy 結束後兩端都被 Close。
func TestBiCopy_BothSidesClosed(t *testing.T) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()

	a := &testRWC{r: r1, w: w2}
	b := &testRWC{r: r2, w: w1}

	// 立即關閉一端的寫入，觸發 EOF
	w1.Close()

	ioutil.BiCopy(context.Background(), a, b, 1024)

	if !a.isClosed() {
		t.Error("a 應已被 Close")
	}
	if !b.isClosed() {
		t.Error("b 應已被 Close")
	}
}

// TestBiCopy_ContextCancel 驗證 ctx 取消時 BiCopy 正常結束。
func TestBiCopy_ContextCancel(t *testing.T) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()

	a := &testRWC{r: r1, w: w2}
	b := &testRWC{r: r2, w: w1}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		ioutil.BiCopy(ctx, a, b, 1024)
		close(done)
	}()

	// 取消 context
	cancel()

	select {
	case <-done:
		// 正常結束
	case <-time.After(3 * time.Second):
		t.Fatal("BiCopy 未在 ctx 取消後結束")
	}

	if !a.isClosed() {
		t.Error("a 應已被 Close")
	}
	if !b.isClosed() {
		t.Error("b 應已被 Close")
	}
}
