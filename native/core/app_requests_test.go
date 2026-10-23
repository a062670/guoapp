package core

import (
	"context"
	"fmt"
	"testing"
)

// TestCancelReadReleasesSlotImmediately 取消读请求必须立刻归还并发槽位。
//
// 原实现只 cancel 不归还：被取消的请求要等底层 goroutine 真正退出（网络
// 超时最长 45 秒）才释放名额。「全部站源」搜索/翻页时上一轮请求被顶掉、
// 下一轮立刻又来，十几个名额很快被这些「已经没人要」的请求占满，用户看到
// 的就是「暂时无法加载：正在处理的请求较多」。
func TestCancelReadReleasesSlotImmediately(t *testing.T) {
	engine := &nativeEngine{}
	ctx := context.Background()
	finishes := make([]func(), 0, nativeReadLimit)
	for index := 0; index < nativeReadLimit; index++ {
		_, finish, err := engine.beginRead(ctx, nativeInput{Session: fmt.Sprintf("s-%d", index), Sequence: 1})
		if err != nil {
			t.Fatalf("beginRead %d: %v", index, err)
		}
		finishes = append(finishes, finish)
	}
	if engine.readCount != nativeReadLimit {
		t.Fatalf("readCount = %d, want %d", engine.readCount, nativeReadLimit)
	}
	if _, _, err := engine.beginRead(ctx, nativeInput{Session: "overflow", Sequence: 1}); err == nil {
		t.Fatal("limit was not enforced")
	}

	// 第 0 个请求被新一代请求顶掉：槽位必须立刻回到可用池。
	engine.cancelRead(nativeInput{Session: "s-0", Sequence: 2})
	if engine.readCount != nativeReadLimit-1 {
		t.Fatalf("cancel did not release the slot: %d", engine.readCount)
	}
	_, finish, err := engine.beginRead(ctx, nativeInput{Session: "after", Sequence: 1})
	if err != nil {
		t.Fatalf("slot was not reusable after cancel: %v", err)
	}
	finishes = append(finishes, finish)

	// 已经被取消的请求再走一次 finish（goroutine 退出）不能重复扣减。
	finishes[0]()
	finishes[0]()
	if engine.readCount != nativeReadLimit {
		t.Fatalf("double release corrupted the counter: %d", engine.readCount)
	}
	for _, finish := range finishes[1:] {
		finish()
	}
	if engine.readCount != 0 {
		t.Fatalf("slots leaked after finish: %d", engine.readCount)
	}
}

// TestBeginReadSupersedesPreviousSequence 同一会话的新序号会顶掉旧请求并归还槽位。
func TestBeginReadSupersedesPreviousSequence(t *testing.T) {
	engine := &nativeEngine{}
	ctx := context.Background()
	_, first, err := engine.beginRead(ctx, nativeInput{Session: "catalog-a", Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.beginRead(ctx, nativeInput{Session: "catalog-a", Sequence: 1}); err == nil {
		t.Fatal("duplicate sequence should be rejected")
	}
	_, second, err := engine.beginRead(ctx, nativeInput{Session: "catalog-a", Sequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	if engine.readCount != 1 {
		t.Fatalf("superseded request kept its slot: %d", engine.readCount)
	}
	first()
	second()
	if engine.readCount != 0 {
		t.Fatalf("readCount = %d, want 0", engine.readCount)
	}
	if len(engine.reads) != 1 {
		t.Fatalf("session record should be kept for sequence comparison: %d", len(engine.reads))
	}
}
