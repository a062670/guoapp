package core

// 规则型 Spider 的「接近真机」生命周期测试：
// 导入 → 重启 → 浏览/详情 → 订阅重导 → 显式刷新自愈 → 删除清理，
// 外加并发、BiliGuard 识别、drpy 来源链接记录。
//
// 这些用例覆盖的是 App 实际调用路径（importLibreTVSubscription /
// nativeCatalog / nativeDetail / nativeCategories / removeLibreTVSite /
// importDrpyScript），而不是单个内部函数——目标是等价于真机上
// 「导入订阅 → 点开片源 → 播放 → 重启 App」的操作强度。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ruleLifecycleFixture 统一夹具：规则、分类页、详情页、播放页各一份。
func ruleLifecycleFixture(t *testing.T) (*nativeEngine, *int32, func(string)) {
	t.Helper()
	ruleBody := ruleFixtureBody(t, "xbpq_wwgz.json")
	listBody := ruleFixtureBody(t, "xbpq_wwgz_list.html")
	detailBody := ruleFixtureBody(t, "xbpq_wwgz_detail.html")
	playBody := ruleFixtureBody(t, "xbpq_wwgz_play.html")
	var hits int32
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		atomic.AddInt32(&hits, 1)
		path := request.URL.Path
		switch {
		case path == "/rule.json":
			return sourceFixtureResponse(request, 200, ruleBody), nil
		case strings.Contains(path, "vod-list-id-"), strings.Contains(request.URL.RequestURI(), "vod-search"):
			return sourceFixtureResponse(request, 200, listBody), nil
		case strings.HasPrefix(path, "/vod-detail-id-"):
			return sourceFixtureResponse(request, 200, detailBody), nil
		case strings.HasPrefix(path, "/vod-play-id-"):
			return sourceFixtureResponse(request, 200, playBody), nil
		}
		return sourceFixtureResponse(request, 404, "not found"), nil
	})
	setRule := func(body string) { ruleBody = body }
	return engine, &hits, setRule
}

// TestRuleSourceSurvivesRestart 导入后重启引擎：站点、引擎标签、规则快照
// 与目录缓存都要还在（真机上等价于杀进程再打开）。
func TestRuleSourceSurvivesRestart(t *testing.T) {
	directory := t.TempDir()
	fixture := func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Path == "/rule.json":
			return sourceFixtureResponse(request, 200, ruleFixtureBody(t, "xbpq_wwgz.json")), nil
		case strings.Contains(request.URL.Path, "vod-list-id-"):
			return sourceFixtureResponse(request, 200, ruleFixtureBody(t, "xbpq_wwgz_list.html")), nil
		}
		return sourceFixtureResponse(request, 404, "not found"), nil
	}
	engine, err := newNativeEngine(directory)
	if err != nil {
		t.Fatal(err)
	}
	engine.downloader.client.Transport = sourceFixtureTransport(fixture)
	engine.downloader.limiter = newRequestLimiter(3, 0)
	engine.downloader.cfg.Retries = 1
	config := `{"sites":[{"key":"wwgz","name":"旺旺影视","type":3,"api":"csp_XBPQ","ext":"http://127.0.0.1:9/rule.json"}]}`
	if _, err := engine.importLibreTVSubscription(context.Background(), config); err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	engine.downloads.close()

	// 重启：同一引擎目录重新装配。
	restarted, err := newNativeEngine(directory)
	if err != nil {
		t.Fatal(err)
	}
	restarted.downloader.client.Transport = sourceFixtureTransport(fixture)
	restarted.downloader.limiter = newRequestLimiter(3, 0)
	restarted.downloader.cfg.Retries = 1
	t.Cleanup(restarted.downloads.close)

	sites := restarted.libretvSiteList()
	if len(sites) != 1 {
		t.Fatalf("重启后站点丢失：%+v", sites)
	}
	if sites[0]["engine"] != ruleEngineXBPQ {
		t.Fatalf("重启后引擎标签丢失：%+v", sites[0])
	}
	if sites[0]["ruleUrl"] != "http://127.0.0.1:9/rule.json" {
		t.Fatalf("重启后规则来源丢失：%+v", sites[0])
	}
	page, err := restarted.nativeCatalog(context.Background(), nativeInput{Source: libretvSourceID("wwgz"), Page: 1, Force: true})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("重启后浏览失败：%v %+v", err, page.Items)
	}
	if page.Items[0].Title != "遇见你" {
		t.Fatalf("重启后列表内容不符：%+v", page.Items[0])
	}
}

// TestRuleSourceRemoveCleansSnapshot 删除站点：站点、规则快照、目录缓存一并清掉，
// 再访问给出「未导入或已删除」而不是空列表。
func TestRuleSourceRemoveCleansSnapshot(t *testing.T) {
	engine, _, _ := ruleLifecycleFixture(t)
	config := `{"sites":[{"key":"wwgz","name":"旺旺影视","type":3,"api":"csp_XBPQ","ext":"http://127.0.0.1:9/rule.json"}]}`
	if _, err := engine.importLibreTVSubscription(context.Background(), config); err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	snapshot := engine.directory + "/rules/wwgz.json"
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatalf("规则快照未落盘：%v", err)
	}
	if err := engine.removeLibreTVSite("wwgz"); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if _, err := os.Stat(snapshot); err == nil {
		t.Fatal("删除后规则快照应移除")
	}
	if len(engine.libretvSiteList()) != 0 {
		t.Fatal("删除后站点列表应为空")
	}
	if _, err := engine.nativeCatalog(context.Background(), nativeInput{Source: libretvSourceID("wwgz"), Page: 1, Force: true}); err == nil {
		t.Fatal("已删除的站点应报错")
	}
}

// TestRuleSourceHealsOnForceRefresh 站点改版场景：旧规则失效 → 显式刷新
// 失败后按来源地址重拉新规则 → 自动恢复；重拉失败则保留旧快照与错误。
func TestRuleSourceHealsOnForceRefresh(t *testing.T) {
	engine, _, setRule := ruleLifecycleFixture(t)
	// 先让规则失效（模拟站点改版后上游还没更新规则）：数组选择器指向不存在的类。
	broken := ruleFixtureRule(t, "xbpq_wwgz.json")
	broken["数组"] = `class="no-such-class"&&`
	brokenBody, _ := json.Marshal(broken)
	setRule(string(brokenBody))
	config := `{"sites":[{"key":"wwgz","name":"旺旺影视","type":3,"api":"csp_XBPQ","ext":"http://127.0.0.1:9/rule.json"}]}`
	if _, err := engine.importLibreTVSubscription(context.Background(), config); err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	// 快照里存的是失效规则：浏览应当报错（探测已失败，但站点仍保留）。
	engine.mu.Lock()
	engine.catalogs = map[string][]nativeDrama{}
	engine.catalogStates = map[string]nativeCatalogState{}
	engine.mu.Unlock()
	page, err := engine.nativeCatalog(context.Background(), nativeInput{Source: libretvSourceID("wwgz"), Page: 1, Force: true})
	if err == nil {
		t.Fatalf("规则失效时应报错：%+v", page.Items)
	}
	// 规则更新到原地址：再来一次显式刷新应自愈。
	setRule(ruleFixtureBody(t, "xbpq_wwgz.json"))
	page, err = engine.nativeCatalog(context.Background(), nativeInput{Source: libretvSourceID("wwgz"), Page: 1, Force: true})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("刷新后应自愈：%v %+v", err, page.Items)
	}
	// 快照已更新为可用规则。
	entry, ok := engine.downloader.rulesStore().get("wwgz")
	if !ok || entry.Rule["数组"] != ruleFixtureRule(t, "xbpq_wwgz.json")["数组"] {
		t.Fatalf("刷新后快照未更新：%+v", entry.Rule["数组"])
	}
	// 规则地址彻底失效：保留旧快照，浏览仍然可用（订阅下线不影响已导入源）。
	engine.downloader.client.Transport = sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/rule.json" {
			return sourceFixtureResponse(request, 404, "gone"), nil
		}
		return sourceFixtureResponse(request, 200, ruleFixtureBody(t, "xbpq_wwgz_list.html")), nil
	})
	if _, err := engine.nativeCatalog(context.Background(), nativeInput{Source: libretvSourceID("wwgz"), Page: 1, Force: true}); err != nil {
		t.Fatalf("规则地址失效后应继续用旧快照：%v", err)
	}
}

// TestRuleSourceReimportIsIdempotent 同一订阅重复导入：不产生重复站点，
// 规则快照被覆盖更新（key 稳定）。
func TestRuleSourceReimportIsIdempotent(t *testing.T) {
	engine, _, _ := ruleLifecycleFixture(t)
	config := `{"sites":[{"key":"wwgz","name":"旺旺影视","type":3,"api":"csp_XBPQ","ext":"http://127.0.0.1:9/rule.json"}]}`
	for i := 0; i < 3; i++ {
		if _, err := engine.importLibreTVSubscription(context.Background(), config); err != nil {
			t.Fatalf("第 %d 次导入失败：%v", i+1, err)
		}
	}
	if sites := engine.libretvSiteList(); len(sites) != 1 {
		t.Fatalf("重复导入产生了重复站点：%+v", sites)
	}
	if entries := engine.downloader.rulesStore().list(); len(entries) != 1 {
		t.Fatalf("重复导入产生了重复规则：%+v", entries)
	}
}

// TestRuleSourceConcurrentBrowse 并发浏览（配合 -race 使用）：
// 多站点 × 多协程同时抓分类页，不得出现数据竞争或互相污染。
func TestRuleSourceConcurrentBrowse(t *testing.T) {
	engine, _, _ := ruleLifecycleFixture(t)
	var sites []string
	for _, key := range []string{"a", "b", "c"} {
		config := `{"sites":[{"key":"` + key + `","name":"站点` + key + `","type":3,"api":"csp_XBPQ","ext":"http://127.0.0.1:9/rule.json"}]}`
		if _, err := engine.importLibreTVSubscription(context.Background(), config); err != nil {
			t.Fatalf("导入 %s 失败：%v", key, err)
		}
		sites = append(sites, libretvSourceID(key))
	}
	var wait sync.WaitGroup
	errs := make(chan error, len(sites)*8)
	for _, source := range sites {
		for i := 0; i < 8; i++ {
			wait.Add(1)
			go func(source string) {
				defer wait.Done()
				page, err := engine.nativeCatalog(context.Background(), nativeInput{Source: source, Page: 1, Force: true})
				if err != nil {
					errs <- err
					return
				}
				if len(page.Items) == 0 {
					errs <- errNoRuleItems
				}
			}(source)
		}
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发浏览失败：%v", err)
	}
}

// errNoRuleItems 并发用例里的空结果哨兵（避免直接依赖 errors.New）。
var errNoRuleItems = jsonError("并发浏览返回空列表")

// jsonError 简单错误类型（测试内使用）。
type jsonError string

func (e jsonError) Error() string { return string(e) }

// TestBiliGuardRuleIsAccepted BiliGuard（Bili 加固版）：
// api 是 csp_BiliGuard、ext 是 {"json": 地址}，规则文件被 HTML 包裹时也要能导入。
func TestBiliGuardRuleIsAccepted(t *testing.T) {
	rule := ruleFixtureRule(t, "bili_bili.json")
	ruleBody, _ := json.Marshal(rule)
	wrapped := "<html><body><pre>" + string(ruleBody) + "</pre></body></html>"
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Path == "/rule.html":
			return sourceFixtureResponse(request, 200, wrapped), nil
		case strings.Contains(request.URL.Path, "/wbi/search/type"):
			return sourceFixtureResponse(request, 200, `{"code":0,"data":{"result":[{"bvid":"BV1xx411c7mD","title":"搞笑合集","pic":"//i0.hdslb.com/a.jpg","duration":"10:00"}]}}`), nil
		case strings.Contains(request.URL.Path, "/x/web-interface/nav"):
			return sourceFixtureResponse(request, 200, `{"code":0,"data":{"wbi_img":{"img_url":"https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png","sub_url":"https://i0.hdslb.com/bfs/wbi/4932caff0ff746eab6f01bf08b70ac45.png"}}}`), nil
		}
		return sourceFixtureResponse(request, 404, "not found"), nil
	})
	config := `{"sites":[{"key":"bili_guard","name":"哔哔合集","type":3,"api":"csp_BiliGuard","ext":{"json":"http://127.0.0.1:9/rule.html"}}]}`
	result, err := engine.importLibreTVSubscription(context.Background(), config)
	if err != nil {
		t.Fatalf("BiliGuard 导入失败：%v", err)
	}
	payload, _ := result.(map[string]any)
	added, _ := payload["added"].([]map[string]any)
	if len(added) != 1 || added[0]["engine"] != ruleEngineBili {
		t.Fatalf("BiliGuard 应落成 Bili 引擎：%+v（skipped=%v）", added, payload["skipped"])
	}
	categories, err := engine.nativeCategories(context.Background(), libretvSourceID("bili_guard"), true)
	if err != nil || len(categories) < 2 {
		t.Fatalf("BiliGuard 分类失败：%v %+v", err, categories)
	}
}

// TestDrpyImportRecordsSourceURL 粘贴脚本 + 来源链接：记录后 WebDAV 同步
// 才能在对端按地址重建；直接粘贴地址时以地址为准；非法地址拒绝。
func TestDrpyImportRecordsSourceURL(t *testing.T) {
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		return sourceFixtureResponse(request, 200, drpyScriptFixture), nil
	})
	script := `var rule = {title: '测试源', host: 'https://example.com'};
function home() { return {class: []}; }`
	result, err := engine.importDrpyScript(context.Background(), script, "https://example.com/rule.js")
	if err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	payload, _ := result.(map[string]any)
	if payload["scriptUrl"] != "https://example.com/rule.js" {
		t.Fatalf("来源链接未记录：%+v", payload)
	}
	sites := engine.drpySiteList()
	found := false
	for _, site := range sites {
		if site["scriptUrl"] == "https://example.com/rule.js" {
			found = true
		}
	}
	if !found {
		t.Fatalf("站点列表里没有来源链接：%+v", sites)
	}
	// 非法来源链接：明确拒绝，不静默存空。
	if _, err := engine.importDrpyScript(context.Background(), script, "clan://x/y.js"); err == nil {
		t.Fatal("非法来源链接应报错")
	}
	// 直接粘贴地址导入：忽略 sourceURL，以地址为准。
	engine2 := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		return sourceFixtureResponse(request, 200, drpyScriptFixture), nil
	})
	if _, err := engine2.importDrpyScript(context.Background(), "https://example.com/real.js", "https://example.com/other.js"); err != nil {
		t.Fatalf("地址导入失败：%v", err)
	}
	for _, site := range engine2.drpySiteList() {
		if site["scriptUrl"] == "https://example.com/other.js" {
			t.Fatal("地址导入不应采用 sourceURL")
		}
	}
}

// TestHikerEnginesLiveSites 海阔引擎真机冒烟（默认跳过）：
//
//	RULE_LIVE=1 go test ./core/ -run TestHikerEnginesLiveSites -v
//
// 逐个尝试真实海阔站点（规则取自 XYQ 订阅，2026-10 抓取）：
//   - 能取到片单的站点：断言标题/链接形状，并尝试详情；
//   - 全部不可用（沙箱网络受限 / 站点已死 / 密码页）：t.Skip 并打印原因，
//     在真机网络上重跑即可变成有效用例。
func TestHikerEnginesLiveSites(t *testing.T) {
	if os.Getenv("RULE_LIVE") == "" {
		t.Skip("设置 RULE_LIVE=1 才会访问真实站点")
	}
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.downloads.close)
	type liveCase struct{ file, key, name, api string }
	cases := []liveCase{
		{"hiker_bidi.json", "bidi", "哔嘀影视", "https://v.xlys.ltd.ua/"},
		{"hiker_ddys.json", "ddys", "低端影视", "https://www.ddys.run/"},
		{"hiker_hitv.json", "hitv", "好影快看", "https://www.hitv.app/"},
		{"hiker_aowu.json", "aowu", "嗷呜动漫", "https://www.aowu.tv/"},
		{"hiker_yinghua.json", "yh", "樱花动漫", "http://www.yinghuadm.cn/"},
		{"hiker_duanjuwu.json", "djw", "短剧屋", "http://www.metaysw.com/"},
	}
	var reasons []string
	okCount := 0
	for _, c := range cases {
		rule := ruleFixtureRule(t, c.file)
		site := libretvSite{Key: c.key, Name: c.name, API: c.api, Type: 1, Engine: ruleEngineHiker}
		entry := ruleEntry{Key: c.key, Engine: ruleEngineHiker, Rule: rule}
		categories, _ := engine.downloader.fetchHikerCategories(site, entry)
		items, _, err := engine.downloader.fetchHikerCatalogPage(context.Background(), site, entry, 1, "1", "")
		if err != nil {
			reasons = append(reasons, c.name+"："+err.Error())
			continue
		}
		if len(items) == 0 {
			reasons = append(reasons, c.name+"：片单为空")
			continue
		}
		okCount++
		t.Logf("[%s] 分类 %d / 片单 %d，首条 %q", c.name, len(categories), len(items), items[0].Title)
		if items[0].Title == "" || items[0].SourceID == "" {
			t.Fatalf("[%s] 片单字段缺失：%+v", c.name, items[0])
		}
		_, chapters, detailErr := engine.downloader.fetchHikerDetail(context.Background(), site, entry, items[0].SourceID)
		if detailErr != nil {
			reasons = append(reasons, c.name+" 详情："+detailErr.Error())
			continue
		}
		t.Logf("[%s] 详情分集 %d，首条 %q", c.name, len(chapters), func() string {
			if len(chapters) > 0 {
				return chapters[0].VideoURL
			}
			return ""
		}())
	}
	if okCount == 0 {
		t.Skipf("本次运行没有可用海阔站点（%d 个失败）：%v", len(reasons), reasons)
	}
}

// TestRuleEnginesLiveNetwork 真实网络冒烟（默认跳过）：
//
//	RULE_LIVE=1 go test ./core/ -run TestRuleEnginesLiveNetwork -v
//
// 直接导入真实订阅里的 XBPQ 规则并走完 分类 → 列表 → 详情 → 分集，
// 用来在发版前确认「线上规则格式没有漂移」。
func TestRuleEnginesLiveNetwork(t *testing.T) {
	if os.Getenv("RULE_LIVE") == "" {
		t.Skip("设置 RULE_LIVE=1 才会访问真实站点")
	}
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.downloads.close)
	config := `{"sites":[{"key":"wwgz_live","name":"旺旺影视","type":3,"api":"csp_XBPQ","ext":"https://gh-proxy.com/https://raw.githubusercontent.com/maoystv/6/refs/heads/main/XBPQ/%E5%88%86%E4%BA%AB%E5%86%9C%E6%B0%91.json"}]}`
	result, err := engine.importLibreTVSubscription(context.Background(), config)
	if err != nil {
		t.Fatalf("线上导入失败：%v", err)
	}
	payload, _ := result.(map[string]any)
	if added, _ := payload["added"].([]map[string]any); len(added) != 1 {
		t.Fatalf("线上导入未成功：%+v", payload)
	}
	page, err := engine.nativeCatalog(context.Background(), nativeInput{Source: libretvSourceID("wwgz_live"), Page: 1, Force: true})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("线上列表失败：%v", err)
	}
	t.Logf("线上列表 %d 条，首条 %q", len(page.Items), page.Items[0].Title)
	detail, err := engine.nativeDetail(context.Background(), page.Items[0])
	if err != nil {
		t.Fatalf("线上详情失败：%v", err)
	}
	detailPayload, _ := detail.(map[string]any)
	chapters, _ := detailPayload["chapters"].([]Chapter)
	if len(chapters) == 0 {
		t.Fatal("线上详情没有分集")
	}
	matched := 0
	for _, chapter := range chapters {
		if isProviderHTTPMediaURL(chapter.VideoURL) {
			matched++
		}
	}
	if matched == 0 {
		t.Fatalf("线上分集地址都不是可播放地址：%+v", chapters)
	}
	t.Logf("线上分集 %d 条，首条 %q", len(chapters), chapters[0].VideoURL)
	// 播放链路：真实 m3u8 交给 resolveProviderMedia（拉播放列表 / 校验直链），
	// 等价于 App 里点播放。
	if strings.Contains(chapters[0].VideoURL, ".m3u8") {
		task := Task{DramaID: page.Items[0].ID, Chapter: chapters[0], Index: 0}
		media, mediaErr := engine.downloader.resolveProviderMedia(context.Background(), task)
		if mediaErr != nil {
			t.Fatalf("线上播放链路失败：%v", mediaErr)
		}
		if !strings.Contains(media.Playlist, "#EXTM3U") {
			t.Fatalf("播放列表未取到：%q", media.Playlist[:min(80, len(media.Playlist))])
		}
		t.Logf("播放链路 OK：url=%s 播放列表 %d 字节", media.URL, len(media.Playlist))
	}
	// B 站：分类直出 + wbi 签名搜索 + 详情分 P（匿名可用性冒烟）。
	biliRule := ruleFixtureRule(t, "bili_bili.json")
	biliSite := libretvSite{Key: "bili_live", Name: "B站", API: "https://www.bilibili.com/", Type: 1, Engine: ruleEngineBili}
	biliEntry := ruleEntry{Key: "bili_live", Engine: ruleEngineBili, Rule: biliRule}
	biliItems, _, err := engine.downloader.fetchBiliCatalogPage(context.Background(), biliSite, biliEntry, 1, "1", "")
	if err != nil {
		t.Fatalf("线上 B 站列表失败：%v", err)
	}
	if len(biliItems) == 0 {
		t.Fatal("线上 B 站列表为空")
	}
	t.Logf("B 站 %d 条，首条 %q", len(biliItems), biliItems[0].Title)
	biliDrama, biliChapters, err := engine.downloader.fetchBiliDetail(context.Background(), biliSite, biliEntry, biliItems[0].SourceID)
	if err != nil || len(biliChapters) == 0 {
		t.Fatalf("线上 B 站详情失败：%v %+v", err, biliChapters)
	}
	t.Logf("B 站详情 %q 分集 %d，首条 %q", biliDrama.Title, len(biliChapters), biliChapters[0].VideoURL)
}
