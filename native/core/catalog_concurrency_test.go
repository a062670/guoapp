package core

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// 真机强度的并发基准（默认跳过，需要网络）：
//
//	SUB_LIVE=1 go test ./core/ -run TestCatalogConcurrencyBenchmark -v
//
// 目的是验证「全部站源」这类多源加载是不是真的并行了：串行逐个请求 8 个源
// 的耗时应接近「8 × 单源耗时」，并发（受限流器约束）应接近「1~2 × 单源」。
// 这个数字比任何理论推断都直接——之前正是它暴露了 Go 侧全局并发只有 3。
func TestCatalogConcurrencyBenchmark(t *testing.T) {
	if os.Getenv("SUB_LIVE") == "" {
		t.Skip("设置 SUB_LIVE=1 才会访问真实站点")
	}
	raw, err := os.ReadFile("/tmp/subs2/影视仓聚合(单仓).json")
	if err != nil {
		t.Skipf("缺少真实订阅样本：%v", err)
	}
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.downloads.close)

	if _, err := engine.importLibreTVSubscription(context.Background(), string(raw)); err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	var sources []string
	for _, site := range engine.downloader.libretv.list() {
		sources = append(sources, libretvSourceID(site.Key))
		if len(sources) >= 8 {
			break
		}
	}
	if len(sources) < 4 {
		t.Skipf("可用源不足（%d 个），跳过", len(sources))
	}

	load := func(source string) time.Duration {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		_, _ = engine.nativeCatalog(ctx, nativeInput{Source: source, Page: 1, Force: true})
		return time.Since(start)
	}

	// 先在「预热」状态测单源耗时（第一个请求常含 DNS/TLS 建连）。
	var single time.Duration
	for _, source := range sources[:3] {
		if elapsed := load(source); elapsed > 0 {
			single = elapsed
			break
		}
	}
	if single <= 0 {
		t.Skip("所有源都请求失败，跳过")
	}

	started := time.Now()
	for _, source := range sources {
		load(source)
	}
	serial := time.Since(started)

	started = time.Now()
	var wg sync.WaitGroup
	for _, source := range sources {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			load(source)
		}(source)
	}
	wg.Wait()
	concurrent := time.Since(started)

	t.Logf("源数 %d，单源约 %v", len(sources), single.Round(time.Millisecond))
	t.Logf("串行 %v | 并发 %v | 提速 %.1fx",
		serial.Round(time.Millisecond),
		concurrent.Round(time.Millisecond),
		float64(serial)/float64(max(int64(concurrent), 1)),
	)
	if concurrent >= serial {
		t.Fatalf("并发没有快于串行（%v >= %v）——限流器可能又把请求串行化了",
			concurrent, serial)
	}
}
