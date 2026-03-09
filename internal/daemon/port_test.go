package daemon_test

import (
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
