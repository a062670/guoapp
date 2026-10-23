package core

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// 请求限流器的并发行为回归。
//
// 「全部站源」可能包含几十个**不同**站点各请求一次；如果全局并发上限太
// 低（曾经是 3），这些互不相关的请求会排成一队，用户看到的就是长时间
// 转圈（反馈里说的「卡死」）。这里是那次修复的护栏：
//   - 不同 host 之间互不阻塞，最多 concurrency 个同时在飞；
//   - 同一 host 仍然串行（gate.busy），单个站点不会被并发打爆。

func newTestRequest() *http.Request {
	request, err := http.NewRequest(http.MethodGet, "https://a.example.com/x", nil)
	if err != nil {
		panic(err)
	}
	return request
}

// TestRequestLimiterAllowsConcurrentHosts 不同 host 可以并发到上限。
func TestRequestLimiterAllowsConcurrentHosts(t *testing.T) {
	const want = 8
	limiter := newRequestLimiterWithExternal(3, want, time.Millisecond)
	// 用 8 个不同的 host，每个都应当能立刻拿到令牌。
	hosts := []string{
		"a.example.com", "b.example.com", "c.example.com", "d.example.com",
		"e.example.com", "f.example.com", "g.example.com", "h.example.com",
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	peak := 0
	for _, host := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			request, _ := http.NewRequest(http.MethodGet, "https://"+host+"/x", nil)
			release, err := limiter.acquire(context.Background(), request)
			if err != nil {
				t.Errorf("%s acquire 失败：%v", host, err)
				return
			}
			mu.Lock()
			granted++
			if granted > peak {
				peak = granted
			}
			mu.Unlock()
			// 占住 60ms，让并发窗口可观测。
			time.Sleep(60 * time.Millisecond)
			mu.Lock()
			granted--
			mu.Unlock()
			release()
		}(host)
	}
	wg.Wait()
	if peak < want {
		t.Fatalf("不同 host 应能并发到 %d，实测峰值 %d——「全部站源」会退化成排队",
			want, peak)
	}
}

// TestRequestLimiterSerializesSameHost 同一 host 仍然串行（保护小站）。
func TestRequestLimiterSerializesSameHost(t *testing.T) {
	limiter := newRequestLimiterWithExternal(3, 8, time.Millisecond)
	var mu sync.Mutex
	inFlight := 0
	peak := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, _ := http.NewRequest(http.MethodGet, "https://same.example.com/x", nil)
			release, err := limiter.acquire(context.Background(), request)
			if err != nil {
				t.Errorf("acquire 失败：%v", err)
				return
			}
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("同一 host 应串行（峰值 1），实测 %d——会把小站打爆", peak)
	}
}

// TestRequestLimiterSpacesSameHost 同一 host 的两次请求之间有最小间隔。
func TestRequestLimiterSpacesSameHost(t *testing.T) {
	const interval = 80 * time.Millisecond
	limiter := newRequestLimiterWithExternal(3, 8, interval)
	request, _ := http.NewRequest(http.MethodGet, "https://spaced.example.com/x", nil)
	first, err := limiter.acquire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	first()
	second, err := limiter.acquire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	second()
	if elapsed < interval/2 {
		t.Fatalf("同一 host 两次请求间隔 %v，应至少约 %v", elapsed, interval)
	}
}

// TestRequestLimiterBuiltinSourcesKeepConservativeLimit 内置源（红果等）必须
// 沿用原作者的并发上限，不能被第三方源的放宽影响。
//
// 红果 / 黄果 / 野果这些站点有账号与反爬风控，并发一旦放大就可能被限流或
// 封号；用户导入的第三方向才需要更高并行度。两者必须是两套计数。
func TestRequestLimiterBuiltinSourcesKeepConservativeLimit(t *testing.T) {
	const builtinLimit = 3
	limiter := newRequestLimiterWithExternal(builtinLimit, 8, time.Millisecond)
	// 用多个不同 host，但都映射到内置源（hongguoduanju.com 及其变体）。
	hosts := []string{
		"hongguoduanju.com",
		"www.hongguoduanju.com",
		"huangguoai.com",
		"huangguo.video",
		"tideember.cc",
		"huangju.net",
	}
	var mu sync.Mutex
	inFlight := 0
	peak := 0
	var wg sync.WaitGroup
	for _, host := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			request, _ := http.NewRequest(http.MethodGet, "https://"+host+"/x", nil)
			release, err := limiter.acquire(context.Background(), request)
			if err != nil {
				t.Errorf("%s acquire 失败：%v", host, err)
				return
			}
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			release()
		}(host)
	}
	wg.Wait()
	if peak > builtinLimit {
		t.Fatalf("内置源并发峰值 %d 超过上限 %d——红果等源可能被风控", peak, builtinLimit)
	}
}
