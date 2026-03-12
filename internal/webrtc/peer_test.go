package webrtc_test

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/chris1004tw/remote-adb/internal/webrtc"
)

// 測試兩個 PeerManager 透過本地信令建立 P2P DataChannel 並雙向傳輸。
func TestPeerManager_OfferAnswer_DataChannelRoundTrip(t *testing.T) {
	config := webrtc.ICEConfig{}

	offerer, err := webrtc.NewPeerManager(config)
	if err != nil {
		t.Fatalf("建立 offerer 失敗: %v", err)
	}
	defer offerer.Close()

	answerer, err := webrtc.NewPeerManager(config)
	if err != nil {
		t.Fatalf("建立 answerer 失敗: %v", err)
	}
	defer answerer.Close()

	// Answerer 監聽 DataChannel
	receivedCh := make(chan struct {
		label string
		data  []byte
	}, 1)
	answerer.OnChannel(func(label string, rwc io.ReadWriteCloser) {
		buf := make([]byte, 1024)
		n, err := rwc.Read(buf)
		if err != nil {
			t.Errorf("讀取 DataChannel 失敗: %v", err)
			return
		}
		receivedCh <- struct {
			label string
			data  []byte
		}{label: label, data: buf[:n]}
	})

	// 先建立 DataChannel（非阻塞），再交換 SDP
	ch, err := offerer.OpenChannel("adb/DEV001/sess-001")
	if err != nil {
		t.Fatalf("OpenChannel 失敗: %v", err)
	}

	offerSDP, err := offerer.CreateOffer()
	if err != nil {
		t.Fatalf("CreateOffer 失敗: %v", err)
	}

	answerSDP, err := answerer.HandleOffer(offerSDP)
	if err != nil {
		t.Fatalf("HandleOffer 失敗: %v", err)
	}

	if err := offerer.HandleAnswer(answerSDP); err != nil {
		t.Fatalf("HandleAnswer 失敗: %v", err)
	}

	// 連線建立後寫入資料（Write 會自動等待 DC 開啟）
	testData := []byte("hello from offerer")
	if _, err := ch.Write(testData); err != nil {
		t.Fatalf("寫入 DataChannel 失敗: %v", err)
	}

	select {
	case received := <-receivedCh:
		if received.label != "adb/DEV001/sess-001" {
			t.Errorf("label = %q, 預期 %q", received.label, "adb/DEV001/sess-001")
		}
		if string(received.data) != "hello from offerer" {
			t.Errorf("data = %q, 預期 %q", string(received.data), "hello from offerer")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超時未收到 DataChannel 資料")
	}
}

func TestPeerManager_OnDisconnect(t *testing.T) {
	config := webrtc.ICEConfig{}

	offerer, err := webrtc.NewPeerManager(config)
	if err != nil {
		t.Fatalf("建立 offerer 失敗: %v", err)
	}

	answerer, err := webrtc.NewPeerManager(config)
	if err != nil {
		t.Fatalf("建立 answerer 失敗: %v", err)
	}

	disconnected := make(chan struct{}, 1)
	answerer.OnDisconnect(func() {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	})

	// 需要至少一個 DataChannel 才能建立連線
	offerer.OpenChannel("keepalive")

	offerSDP, err := offerer.CreateOffer()
	if err != nil {
		t.Fatalf("CreateOffer 失敗: %v", err)
	}
	answerSDP, err := answerer.HandleOffer(offerSDP)
	if err != nil {
		t.Fatalf("HandleOffer 失敗: %v", err)
	}
	if err := offerer.HandleAnswer(answerSDP); err != nil {
		t.Fatalf("HandleAnswer 失敗: %v", err)
	}

	// 等待連線建立
	time.Sleep(1 * time.Second)

	// 關閉 offerer，answerer 應偵測到斷線
	offerer.Close()

	select {
	case <-disconnected:
		// 成功偵測到斷線
	case <-time.After(10 * time.Second):
		t.Fatal("超時未偵測到斷線")
	}

	answerer.Close()
}

func TestPeerManager_MultipleChannels(t *testing.T) {
	config := webrtc.ICEConfig{}

	offerer, err := webrtc.NewPeerManager(config)
	if err != nil {
		t.Fatalf("建立 offerer 失敗: %v", err)
	}
	defer offerer.Close()

	answerer, err := webrtc.NewPeerManager(config)
	if err != nil {
		t.Fatalf("建立 answerer 失敗: %v", err)
	}
	defer answerer.Close()

	labels := make(chan string, 3)
	answerer.OnChannel(func(label string, rwc io.ReadWriteCloser) {
		labels <- label
		rwc.Close()
	})

	// 先建立所有 DataChannel，再交換 SDP
	channels := make([]io.ReadWriteCloser, 0, 3)
	for _, name := range []string{"adb/DEV001/s1", "adb/DEV002/s2", "adb/DEV003/s3"} {
		ch, err := offerer.OpenChannel(name)
		if err != nil {
			t.Fatalf("OpenChannel(%q) 失敗: %v", name, err)
		}
		channels = append(channels, ch)
	}

	offerSDP, _ := offerer.CreateOffer()
	answerSDP, _ := answerer.HandleOffer(offerSDP)
	offerer.HandleAnswer(answerSDP)

	// 驗證全部收到
	received := make(map[string]bool)
	for i := 0; i < 3; i++ {
		select {
		case l := <-labels:
			received[l] = true
		case <-time.After(10 * time.Second):
			t.Fatalf("超時，只收到 %d/3 個 DataChannel", i)
		}
	}

	for _, expected := range []string{"adb/DEV001/s1", "adb/DEV002/s2", "adb/DEV003/s3"} {
		if !received[expected] {
			t.Errorf("未收到 DataChannel: %q", expected)
		}
	}

	for _, ch := range channels {
		ch.Close()
	}
}

// TestPeerManager_CloseUnblocksPendingChannels 驗證 PeerManager.Close() 能解除
// 尚未就緒的 pendingChannel 阻塞（H1 修復驗證）。
// 場景：建立 DataChannel 後未做 SDP 交換（OnOpen 永遠不會觸發），
// 呼叫 pm.Close() 應讓 Read/Close 立即回傳 ErrPeerClosed。
func TestPeerManager_CloseUnblocksPendingChannels(t *testing.T) {
	pm, err := webrtc.NewPeerManager(webrtc.ICEConfig{})
	if err != nil {
		t.Fatalf("建立 PeerManager 失敗: %v", err)
	}

	// 建立 DataChannel 但不做 SDP 交換，OnOpen 永遠不觸發
	ch, err := pm.OpenChannel("test-pending")
	if err != nil {
		t.Fatalf("OpenChannel 失敗: %v", err)
	}

	// 在背景 goroutine 中嘗試 Read（會阻塞在 wait()）
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := ch.Read(buf)
		readDone <- err
	}()

	// 確認 Read 確實在阻塞（短暫等待後不應完成）
	select {
	case <-readDone:
		t.Fatal("Read 不應在 Close 前回傳")
	case <-time.After(100 * time.Millisecond):
		// 預期：Read 仍在阻塞
	}

	// 關閉 PeerManager，應解除 Read 的阻塞
	pm.Close()

	select {
	case err := <-readDone:
		if !errors.Is(err, webrtc.ErrPeerClosed) {
			t.Errorf("Read 回傳 %v，預期 %v", err, webrtc.ErrPeerClosed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pm.Close() 後 Read 仍未解除阻塞（超時 5 秒）")
	}

	// Close() 也應立即回傳而非阻塞
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- ch.Close()
	}()

	select {
	case err := <-closeDone:
		if !errors.Is(err, webrtc.ErrPeerClosed) {
			t.Errorf("Close 回傳 %v，預期 %v", err, webrtc.ErrPeerClosed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pm.Close() 後 ch.Close() 仍未回傳（超時 5 秒）")
	}
}

// TestPeerManager_OpenChannel_NormalFlow 驗證正常 SDP 交換後，
// pendingChannel 的 Read/Write 能正確運作（H12 移除 mutex 後的回歸測試）。
func TestPeerManager_OpenChannel_NormalFlow(t *testing.T) {
	offerer, err := webrtc.NewPeerManager(webrtc.ICEConfig{})
	if err != nil {
		t.Fatalf("建立 offerer 失敗: %v", err)
	}
	defer offerer.Close()

	answerer, err := webrtc.NewPeerManager(webrtc.ICEConfig{})
	if err != nil {
		t.Fatalf("建立 answerer 失敗: %v", err)
	}
	defer answerer.Close()

	// Answerer 端收到 DataChannel 後回寫資料
	echoCh := make(chan struct{})
	answerer.OnChannel(func(label string, rwc io.ReadWriteCloser) {
		defer close(echoCh)
		buf := make([]byte, 256)
		n, err := rwc.Read(buf)
		if err != nil {
			t.Errorf("answerer Read 失敗: %v", err)
			return
		}
		// 回寫收到的資料
		if _, err := rwc.Write(buf[:n]); err != nil {
			t.Errorf("answerer Write 失敗: %v", err)
		}
	})

	ch, err := offerer.OpenChannel("echo-test")
	if err != nil {
		t.Fatalf("OpenChannel 失敗: %v", err)
	}

	offerSDP, err := offerer.CreateOffer()
	if err != nil {
		t.Fatalf("CreateOffer 失敗: %v", err)
	}
	answerSDP, err := answerer.HandleOffer(offerSDP)
	if err != nil {
		t.Fatalf("HandleOffer 失敗: %v", err)
	}
	if err := offerer.HandleAnswer(answerSDP); err != nil {
		t.Fatalf("HandleAnswer 失敗: %v", err)
	}

	// Write 會等待 DataChannel 就緒後傳送（驗證無 mutex 後仍正確）
	testData := []byte("echo-payload")
	if _, err := ch.Write(testData); err != nil {
		t.Fatalf("Write 失敗: %v", err)
	}

	// 讀取 echo 回應
	buf := make([]byte, 256)
	readDone := make(chan struct {
		n   int
		err error
	}, 1)
	go func() {
		n, err := ch.Read(buf)
		readDone <- struct {
			n   int
			err error
		}{n, err}
	}()

	select {
	case result := <-readDone:
		if result.err != nil {
			t.Fatalf("Read 失敗: %v", result.err)
		}
		if string(buf[:result.n]) != "echo-payload" {
			t.Errorf("回應 = %q，預期 %q", string(buf[:result.n]), "echo-payload")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超時未收到 echo 回應")
	}
}
