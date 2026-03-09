package webrtc_test

import (
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
