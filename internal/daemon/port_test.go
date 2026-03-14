package daemon_test

import (
	"fmt"
	"net"
	"testing"

	"github.com/chris1004tw/remote-adb/internal/daemon"
)

func TestPortAllocator_Sequential(t *testing.T) {
	pa := daemon.NewPortAllocator(20000, 20010)

	p1, err := pa.Allocate()
	if err != nil {
		t.Fatalf("第一次分配失敗: %v", err)
	}
	if p1 != 20000 {
		t.Errorf("第一個 port = %d, 預期 20000", p1)
	}

	p2, err := pa.Allocate()
	if err != nil {
		t.Fatalf("第二次分配失敗: %v", err)
	}
	if p2 != 20001 {
		t.Errorf("第二個 port = %d, 預期 20001", p2)
	}
}

func TestPortAllocator_Release(t *testing.T) {
	pa := daemon.NewPortAllocator(20000, 20002)

	p1, _ := pa.Allocate()
	p2, _ := pa.Allocate()

	if pa.UsedCount() != 2 {
		t.Errorf("UsedCount = %d, 預期 2", pa.UsedCount())
	}

	pa.Release(p1)
	if pa.UsedCount() != 1 {
		t.Errorf("釋放後 UsedCount = %d, 預期 1", pa.UsedCount())
	}

	// 重新分配應該拿到釋放的 port
	p3, err := pa.Allocate()
	if err != nil {
		t.Fatalf("重新分配失敗: %v", err)
	}
	if p3 != p1 {
		t.Errorf("重新分配的 port = %d, 預期 %d", p3, p1)
	}
	_ = p2
}

func TestPortAllocator_Exhausted(t *testing.T) {
	pa := daemon.NewPortAllocator(20000, 20001)

	_, err1 := pa.Allocate()
	_, err2 := pa.Allocate()
	if err1 != nil || err2 != nil {
		t.Fatalf("前兩次分配不應失敗")
	}

	_, err3 := pa.Allocate()
	if err3 == nil {
		t.Error("Port 範圍已滿，應回傳錯誤")
	}
}

// TestPortAllocator_AllocateListener 驗證 AllocateListener 回傳可用的 listener，
// 且 listener 在回傳時仍處於監聽狀態（未被關閉），可直接接受連線。
func TestPortAllocator_AllocateListener(t *testing.T) {
	pa := daemon.NewPortAllocator(21000, 21010)

	ln, port, err := pa.AllocateListener()
	if err != nil {
		t.Fatalf("AllocateListener 失敗: %v", err)
	}
	defer ln.Close()

	if port != 21000 {
		t.Errorf("port = %d, 預期 21000", port)
	}

	// 驗證 listener 仍在監聽：嘗試 dial 應成功
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listener 應仍在監聽，但 Dial 失敗: %v", err)
	}
	conn.Close()

	// 驗證 port 已被標記為 used
	if pa.UsedCount() != 1 {
		t.Errorf("UsedCount = %d, 預期 1", pa.UsedCount())
	}
}

// TestPortAllocator_AllocateListenerSkipsOccupied 驗證 AllocateListener
// 會跳過被外部程式佔用的 port，分配下一個可用 port。
func TestPortAllocator_AllocateListenerSkipsOccupied(t *testing.T) {
	// 先佔用 21100 port
	occupied, err := net.Listen("tcp", "127.0.0.1:21100")
	if err != nil {
		t.Fatalf("預佔 port 21100 失敗: %v", err)
	}
	defer occupied.Close()

	pa := daemon.NewPortAllocator(21100, 21110)

	ln, port, err := pa.AllocateListener()
	if err != nil {
		t.Fatalf("AllocateListener 失敗: %v", err)
	}
	defer ln.Close()

	// 應跳過已佔用的 21100，分配 21101
	if port != 21101 {
		t.Errorf("port = %d, 預期 21101（應跳過被佔用的 21100）", port)
	}
}

// TestPortAllocator_AllocateListenerExhausted 驗證所有 port 都被分配後，
// AllocateListener 回傳錯誤。
func TestPortAllocator_AllocateListenerExhausted(t *testing.T) {
	pa := daemon.NewPortAllocator(21200, 21201)

	ln1, _, err := pa.AllocateListener()
	if err != nil {
		t.Fatalf("第一次 AllocateListener 失敗: %v", err)
	}
	defer ln1.Close()

	ln2, _, err := pa.AllocateListener()
	if err != nil {
		t.Fatalf("第二次 AllocateListener 失敗: %v", err)
	}
	defer ln2.Close()

	_, _, err = pa.AllocateListener()
	if err == nil {
		t.Error("Port 範圍已滿，AllocateListener 應回傳錯誤")
	}
}
