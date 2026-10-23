package core

import (
	"context"
	"errors"
	"strings"
	"time"
)

type nativeReadRequest struct {
	sequence int64
	cancel   context.CancelFunc
	updated  time.Time
	// active 表示这次请求仍占用一个并发槽位。
	//
	// 取消（cancelRead / 新一代请求顶掉旧请求）时立刻置 false 并归还槽位，
	// 而不是等底层 goroutine 真正退出。原实现只 cancel 不归还，被取消的
	// 请求会一直占着名额直到网络超时——「全部站源」几十个源时，上一轮
	// 搜索/翻页的请求还没 unwound，下一轮就被 16 个名额卡住，直接报
	// 「正在处理的请求较多」。取消本身就是「不等结果了」，没有理由继续占位。
	active bool
}

// nativeReadLimit 同时在飞的「读请求」上限。
//
// 原作者按单源场景定的 16；「全部站源」一次刷新就是几十个源（每个源一次
// catalog / categories 读），再叠上搜索时被顶掉的旧请求，16 很容易打满并
// 把用户挡在「正在处理的请求较多」上。取消现在会立即归还槽位，这里再给
// 多源场景留出余量。
const nativeReadLimit = 48

// nativeReadSessions 会话记录上限（每个源一个 session，取消后保留记录用于
// 序号比较，15 分钟后清理）。
const nativeReadSessions = 256

func validNativeRead(input nativeInput) bool {
	return input.Sequence > 0 && len(input.Session) > 0 && len(input.Session) <= 128 &&
		strings.IndexFunc(input.Session, func(r rune) bool {
			return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || strings.ContainsRune("-_:.", r))
		}) < 0
}

func (engine *nativeEngine) beginRead(ctx context.Context, input nativeInput) (context.Context, func(), error) {
	if !validNativeRead(input) {
		return nil, nil, errors.New("请求标记无效")
	}
	engine.readMu.Lock()
	defer engine.readMu.Unlock()
	engine.prepareReadsLocked()
	previous := engine.reads[input.Session]
	if input.Sequence <= previous.sequence {
		return nil, nil, context.Canceled
	}
	if previous.cancel != nil {
		previous.cancel()
	}
	if previous.active {
		// 同一会话被新一代请求顶掉：旧请求已经没人等了，立刻归还槽位。
		previous.active = false
		engine.readCount--
	}
	if engine.readCount >= nativeReadLimit || len(engine.reads) >= nativeReadSessions && previous.sequence == 0 {
		return nil, nil, errors.New("正在处理的请求较多，请稍后重试")
	}
	work, cancel := context.WithCancel(ctx)
	engine.reads[input.Session] = nativeReadRequest{sequence: input.Sequence, cancel: cancel, updated: time.Now(), active: true}
	engine.readCount++
	finish := func() {
		cancel()
		engine.readMu.Lock()
		defer engine.readMu.Unlock()
		if request := engine.reads[input.Session]; request.sequence == input.Sequence && request.active {
			request.cancel, request.updated, request.active = nil, time.Now(), false
			engine.reads[input.Session] = request
			engine.readCount--
		}
	}
	return work, finish, nil
}

func (engine *nativeEngine) prepareReadsLocked() {
	if engine.reads == nil {
		engine.reads = map[string]nativeReadRequest{}
	}
	for key, request := range engine.reads {
		if request.cancel == nil && time.Since(request.updated) > 15*time.Minute {
			delete(engine.reads, key)
		}
	}
}

func (engine *nativeEngine) cancelRead(input nativeInput) {
	if !validNativeRead(input) {
		return
	}
	engine.readMu.Lock()
	defer engine.readMu.Unlock()
	engine.prepareReadsLocked()
	previous := engine.reads[input.Session]
	if input.Sequence < previous.sequence {
		return
	}
	if previous.cancel != nil {
		previous.cancel()
	}
	if previous.active {
		// 立刻归还槽位；底层 goroutine 收到 ctx 取消后会自行退出，finish
		// 看到 active=false 不会重复扣减。
		engine.readCount--
		previous.active = false
	}
	previous.cancel = nil
	previous.updated = time.Now()
	if previous.sequence < input.Sequence {
		previous.sequence = input.Sequence
	}
	if len(engine.reads) < nativeReadSessions || previous.sequence > 0 {
		engine.reads[input.Session] = previous
	}
}
