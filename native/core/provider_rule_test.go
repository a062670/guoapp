package core

// 规则型 Spider（XBPQ / 海阔 / Bili）的单元与端到端测试。
//
// 数据来源：testdata/rules/ 下的规则与页面全部取自真实订阅与站点
// （2026-10 抓取，主机已替换为占位地址，由 fixture 改道）：
//   - xbpq_wwgz.json     分享农民（旺旺影视）XBPQ 规则
//   - xbpq_wwgz_list.html  分类页（globalPicList 结构 + 3 个列表项）
//   - xbpq_wwgz_detail.html 详情页（title + 元数据 + numList 跳转）
//   - xbpq_wwgz_play.html  播放页（mac_url 脚本，含 $ / # 连接符）
//   - hiker_tuxiaobei.json 兔小贝海阔规则（JSON 分类 + HTML 搜索）
//   - hiker_yingso.json    影搜海阔规则（POST JSON 搜索）
//   - bili_bili.json       Bili 规则（classes 分类）

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// ruleFixtureBody 读 testdata 里的页面文件。
func ruleFixtureBody(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile("testdata/rules/" + name)
	if err != nil {
		t.Fatalf("读取测试数据失败（%s）：%v", name, err)
	}
	return string(body)
}

// ruleFixtureRule 读 testdata 里的规则 JSON。
// 与线上链路一致：先剥 // 注释（海阔规则常带注释）。
func ruleFixtureRule(t *testing.T, name string) map[string]any {
	t.Helper()
	var rule map[string]any
	body := ruleStripJSONComments(ruleFixtureBody(t, name))
	if err := json.Unmarshal([]byte(body), &rule); err != nil {
		t.Fatalf("规则 JSON 解析失败（%s）：%v", name, err)
	}
	return rule
}

// TestRuleStoreRoundTrip 规则表：写入 / 读取 / 删除 / 重启加载。
func TestRuleStoreRoundTrip(t *testing.T) {
	directory := t.TempDir()
	store := newRuleStore(directory)
	rule := map[string]any{"分类url": "https://example.com/list", "站名": "测试站"}
	if err := store.put(ruleEntry{Key: "demo", Engine: ruleEngineXBPQ, SourceURL: "https://example.com/r.json"}, rule); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	entry, ok := store.get("demo")
	if !ok || entry.Engine != ruleEngineXBPQ || entry.Rule["站名"] != "测试站" {
		t.Fatalf("读取结果不符：%+v ok=%v", entry, ok)
	}
	// 正文快照必须落盘（订阅下线后仍可用）。
	if _, err := os.Stat(directory + "/rules/demo.json"); err != nil {
		t.Fatalf("规则快照未落盘：%v", err)
	}
	// 重启加载。
	reloaded := loadRuleStore(directory)
	entry, ok = reloaded.get("demo")
	if !ok || entry.Rule["分类url"] != "https://example.com/list" {
		t.Fatalf("重启后规则丢失：%+v ok=%v", entry, ok)
	}
	// 删除连带删除正文。
	if err := reloaded.remove("demo"); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if _, err := os.Stat(directory + "/rules/demo.json"); err == nil {
		t.Fatal("删除后规则快照应消失")
	}
	if _, ok := reloaded.get("demo"); ok {
		t.Fatal("删除后索引应同步移除")
	}
	// 非法 key 拒绝写入（防路径穿越）。
	if err := store.put(ruleEntry{Key: "../evil", Engine: ruleEngineXBPQ}, rule); err == nil {
		t.Fatal("非法 key 应被拒绝")
	}
}

// baseURLForTest 订阅地址（相对规则路径按它补全）。
func baseURLForTest() string { return "https://sub.example.com/tvbox/XYQTVBox.json" }

// TestRuleCandidateDetection 三种 ext 形态与非法形态的识别。
func TestRuleCandidateDetection(t *testing.T) {
	makeEntry := func(api, ext string) libretvConfigSite {
		return libretvConfigSite{Key: "s1", Name: "站点", API: api, Type: 3, Ext: json.RawMessage(ext)}
	}
	// 1) ext 是远程规则地址。
	candidate, reason := ruleCandidateFromEntry(baseURLForTest(), makeEntry("csp_XBPQ", `"https://example.com/rule.json"`))
	if candidate.Engine != ruleEngineXBPQ || candidate.RuleURL != "https://example.com/rule.json" || reason != "" {
		t.Fatalf("远程规则识别失败：%+v %s", candidate, reason)
	}
	// 2) ext 是 {"json": 地址} 间接形态（Bili 真实写法）。
	candidate, reason = ruleCandidateFromEntry(baseURLForTest(), makeEntry("csp_Bili", `{"json":"https://example.com/bili.json"}`))
	if candidate.Engine != ruleEngineBili || candidate.RuleURL != "https://example.com/bili.json" || reason != "" {
		t.Fatalf("间接规则识别失败：%+v %s", candidate, reason)
	}
	// 3) ext 是内联规则。
	candidate, reason = ruleCandidateFromEntry(baseURLForTest(), makeEntry("csp_XYQHiker", `{"分类链接":"https://a.com/list/{cateId}","分类名称":"电影"}`))
	if candidate.Engine != ruleEngineHiker || len(candidate.Rule) == 0 || reason != "" {
		t.Fatalf("内联规则识别失败：%+v %s", candidate, reason)
	}
	// 3b) 相对路径（海阔订阅的真实写法 ./XYQHiker/xxx.json）：按订阅地址补全。
	if candidate, reason = ruleCandidateFromEntry(baseURLForTest(), makeEntry("csp_XYQHiker", `"./XYQHiker/影搜.json"`)); reason != "" || !strings.HasSuffix(candidate.RuleURL, "/tvbox/XYQHiker/%E5%BD%B1%E6%90%9C.json") {
		t.Fatalf("相对规则路径未补全：%+v %s", candidate, reason)
	}
	// 4) 非规则 Spider：不产生候选也不产生原因（交给后续分支）。
	if candidate, reason := ruleCandidateFromEntry(baseURLForTest(), makeEntry("csp_AppYsV2", `"https://a.com/api.php/provide/vod"`)); candidate.Engine != "" || reason != "" {
		t.Fatalf("非规则 Spider 不应被识别：%+v %s", candidate, reason)
	}
	// 5) 规则型 Spider 但 ext 无法用。
	if _, reason := ruleCandidateFromEntry(baseURLForTest(), makeEntry("csp_XBPQ", `"clan://local/rule.json"`)); reason == "" {
		t.Fatal("clan:// 本地规则应给出跳过原因")
	}
	// 6) 规则形状校验：缺必需字段。
	if reason := validateRuleDocument(ruleEngineXBPQ, map[string]any{"站名": "x"}); reason == "" {
		t.Fatal("XBPQ 缺分类url 应校验失败")
	}
	if reason := validateRuleDocument(ruleEngineHiker, map[string]any{"分类名称": "电影"}); reason == "" {
		t.Fatal("海阔缺分类链接 应校验失败")
	}
	if reason := validateRuleDocument(ruleEngineBili, map[string]any{"cookie": "x"}); reason == "" {
		t.Fatal("Bili 缺 classes 应校验失败")
	}
	// 同一订阅里的两条同引擎内联规则身份必须不同（按内容哈希去重）。
	first := ruleCandidate{Engine: ruleEngineBili, Rule: map[string]any{"class": []any{map[string]any{"type_name": "搞笑"}}}}
	second := ruleCandidate{Engine: ruleEngineBili, Rule: map[string]any{"class": []any{map[string]any{"type_name": "鬼畜"}}}}
	if first.identity() == second.identity() {
		t.Fatal("不同内联规则不应被视为同一条")
	}
	// class/filters 是社区派生写法，也要认。
	if reason := validateRuleDocument(ruleEngineBili, map[string]any{"class": []any{map[string]any{"type_name": "电影", "type_id": "1"}}}); reason != "" {
		t.Fatalf("class 键名应通过校验：%s", reason)
	}
}

// TestXBPQMarkers XBPQ 截取语法：标记 / 通配 / 替换 / 序号过滤 / 转义。
func TestXBPQMarkers(t *testing.T) {
	// 前后标记。
	if value := xbpqCut("<a>标题</a>", xbpqParseField("<a>&&</a>")); value != "标题" {
		t.Fatalf("前后标记截取失败：%q", value)
	}
	// 单标记：取标记之后全部。
	if value := xbpqCut(`<div class="x">正文`, xbpqParseField(`class="x">`)); value != "正文" {
		t.Fatalf("单标记截取失败：%q", value)
	}
	// 通配：<a*> 匹配开标签。
	if value := xbpqCut(`<a href="/x" title="标题">`, xbpqParseField(`<a*>`)); value != "" {
		t.Fatalf("通配截取异常：%q", value)
	}
	if value := xbpqCut(`<a href="/x">正文</a>`, xbpqParseField(`<a*>&&</a>`)); value != "正文" {
		t.Fatalf("通配前后标记失败：%q", value)
	}
	// 数组：成对标记。
	items := xbpqCutAll("<li>1</li><li>2</li>", xbpqParseField("<li>&&</li>"))
	if len(items) != 2 || items[0] != "1" || items[1] != "2" {
		t.Fatalf("成对标记数组失败：%v", items)
	}
	// 数组：单标记切分。
	items = xbpqCutAll(`<p class="i">1<p class="i">2<p class="i">3`, xbpqParseField(`class="i">`))
	if len(items) != 3 {
		t.Fatalf("单标记数组失败：%v", items)
	}
	// 序号过滤（1 起，支持区间）。
	items = xbpqCutAll("<li>1</li><li>2</li><li>3</li>", xbpqParseField("<li>&&</li>[不包含:1#3-3]"))
	if len(items) != 1 || items[0] != "2" {
		t.Fatalf("序号过滤失败：%v", items)
	}
	// 关键词过滤。
	items = xbpqCutAll("<li>首页</li><li>资讯</li><li>电影</li>", xbpqParseField("<li>&&</li>[不包含:首页#资讯]"))
	if len(items) != 1 || items[0] != "电影" {
		t.Fatalf("关键词过滤失败：%v", items)
	}
	// 替换：空 = 删除；通配 (*)。
	if value := xbpqApplyReplace("播放01", "播放>>空", 1); value != "01" {
		t.Fatalf("替换删除失败：%q", value)
	}
	if value := xbpqApplyReplace("第(01)集", "(*)>>空", 1); value != "第集" {
		t.Fatalf("通配替换失败：%q", value)
	}
	// 序号占位。
	if value := xbpqApplyReplace("第X集", "X>><序号>", 7); value != "第7集" {
		t.Fatalf("序号替换失败：%q", value)
	}
	// 转义：\# 表示字面量 #，不参与指令切分。
	parts := xbpqSplitDirective(`a\#b#c`, '#')
	if len(parts) != 2 || parts[0] != "a#b" {
		t.Fatalf("指令转义切分失败：%v", parts)
	}
	// $$ 与 && 等价。
	if items := xbpqCutAll("<li>1</li>", xbpqParseField("<li>$$</li>")); len(items) != 1 || items[0] != "1" {
		t.Fatalf("$$ 分隔失败：%v", items)
	}
}

// TestXBPQImportBrowseDetailSearch 端到端：订阅导入 → 分类 → 详情（跳转 + 播放页）→ 搜索。
func TestXBPQImportBrowseDetailSearch(t *testing.T) {
	ruleBody := ruleFixtureBody(t, "xbpq_wwgz.json")
	listBody := ruleFixtureBody(t, "xbpq_wwgz_list.html")
	detailBody := ruleFixtureBody(t, "xbpq_wwgz_detail.html")
	playBody := ruleFixtureBody(t, "xbpq_wwgz_play.html")
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
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
	config := `{"sites":[{"key":"wwgz","name":"旺旺影视","type":3,"api":"csp_XBPQ","ext":"http://127.0.0.1:9/rule.json"}]}`
	result, err := engine.importLibreTVSubscription(context.Background(), config)
	if err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	payload, _ := result.(map[string]any)
	added, _ := payload["added"].([]map[string]any)
	if len(added) != 1 {
		t.Fatalf("应导入 1 个站点，实际 %d：%+v", len(added), payload)
	}
	if added[0]["engine"] != ruleEngineXBPQ {
		t.Fatalf("站点引擎标签错误：%+v", added[0])
	}
	if verified, _ := payload["verified"].(int); verified != 1 {
		t.Fatalf("站点应探测成功（本地 fixture）：verified=%d skipped=%v", verified, payload["skipped"])
	}
	source := libretvSourceID("wwgz")
	// 分类：电影 / 电视剧 / 综艺 / 动漫 / 短剧（序号 ID）。
	categories, err := engine.nativeCategories(context.Background(), source, true)
	if err != nil {
		t.Fatalf("分类失败：%v", err)
	}
	if len(categories) != 6 || categories[1].Name != "电影" || categories[1].ID != "1" {
		t.Fatalf("分类不符：%+v", categories)
	}
	// 列表。
	page, err := engine.nativeCatalog(context.Background(), nativeInput{Source: source, Page: 1, Force: true})
	if err != nil {
		t.Fatalf("列表失败：%v", err)
	}
	if len(page.Items) != 3 || page.Items[0].Title != "遇见你" {
		t.Fatalf("列表不符：%+v", page.Items)
	}
	if !strings.HasPrefix(page.Items[0].Cover, "https://laudio-cdn") {
		t.Fatalf("封面未取到：%q", page.Items[0].Cover)
	}
	// 详情：跳转 → 播放页 → mac_url 替换链路 → 分集。
	drama := page.Items[0]
	detail, err := engine.nativeDetail(context.Background(), drama)
	if err != nil {
		t.Fatalf("详情失败：%v", err)
	}
	detailPayload, _ := detail.(map[string]any)
	chapters, _ := detailPayload["chapters"].([]Chapter)
	if len(chapters) != 1 {
		t.Fatalf("分集数不符：%d %+v", len(chapters), chapters)
	}
	if chapters[0].VideoURL != "https://cdn.example.com/wwgz/index.m3u8" {
		t.Fatalf("播放地址未解开：%q", chapters[0].VideoURL)
	}
	// 搜索：搜索url 与分类页共用列表结构。
	search, err := engine.nativeCatalog(context.Background(), nativeInput{Source: source, Query: "庆余年", Page: 1, Force: true})
	if err != nil {
		t.Fatalf("搜索失败：%v", err)
	}
	if len(search.Items) == 0 {
		t.Fatalf("搜索结果为空")
	}
}

// TestHikerJSONSearch 海阔 JSON 模式 + POST 搜索（影搜规则）。
func TestHikerJSONSearch(t *testing.T) {
	searchBody := `{"code":0,"data":[{"title":"庆余年 第二季","key":"abc123","type":"剧集"},{"title":"庆余年 第一季","key":"def456"}]}`
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost && request.URL.Path == "/v3/ali/search" {
			// 请求体必须是 JSON（海阔的 POST请求数据 是 JSON 串）。
			return sourceFixtureResponse(request, 200, searchBody), nil
		}
		return sourceFixtureResponse(request, 404, "not found"), nil
	})
	rule := ruleFixtureRule(t, "hiker_yingso.json")
	site := libretvSite{Key: "yingso", Name: "影搜", API: "http://127.0.0.1:9/", Type: 1, Engine: ruleEngineHiker}
	items, _, err := engine.downloader.fetchHikerCatalogPage(context.Background(), site, ruleEntry{Key: "yingso", Engine: ruleEngineHiker, Rule: rule}, 1, "", "庆余年")
	if err != nil {
		t.Fatalf("搜索失败：%v", err)
	}
	if len(items) != 2 || items[0].Title != "庆余年 第二季" {
		t.Fatalf("搜索结果不符：%+v", items)
	}
	// 链接加了前缀：https://www.aliyundrive.com/s/<key>。
	detailURL, _ := drpyDecodeSegment(items[0].SourceID)
	if detailURL != "https://www.aliyundrive.com/s/abc123" {
		t.Fatalf("链接前缀未生效：%q", detailURL)
	}
}

// TestHikerHTMLBrowse 海阔 HTML 模式（兔小贝规则）：分类直出 + 列表 + 选集。
func TestHikerHTMLBrowse(t *testing.T) {
	listBody := `<html><body><div class="list-con">
<div class="items"><a href="/play/101"><mip-img src="https://img.example.com/1.jpg"></mip-img><span class="title">小兔儿歌</span><span class="time">03:20</span></a></div>
<div class="items"><a href="/play/102"><mip-img src="https://img.example.com/2.jpg"></mip-img><span class="title">小兔故事</span><span class="time">05:01</span></a></div>
</div></body></html>`
	detailBody := `<html><body><div class="video-list">
<div class="video"><mip-search-video video-src="https://cdn.example.com/txb/1.mp4"></mip-search-video>立即播放</div>
<div class="video"><mip-search-video video-src="https://cdn.example.com/txb/2.mp4"></mip-search-video>立即播放</div>
</div></body></html>`
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Path, "/list/mip-data"), strings.HasPrefix(request.URL.Path, "/search/index"):
			return sourceFixtureResponse(request, 200, listBody), nil
		case strings.HasPrefix(request.URL.Path, "/play/"):
			return sourceFixtureResponse(request, 200, detailBody), nil
		}
		return sourceFixtureResponse(request, 404, "not found"), nil
	})
	rule := ruleFixtureRule(t, "hiker_tuxiaobei.json")
	site := libretvSite{Key: "txb", Name: "兔小贝", API: "http://127.0.0.1:9/", Type: 1, Engine: ruleEngineHiker}
	entry := ruleEntry{Key: "txb", Engine: ruleEngineHiker, Rule: rule}
	// 分类直出：4 个（儿歌/故事/国学/启蒙）。
	categories, err := engine.downloader.fetchHikerCategories(site, entry)
	if err != nil || len(categories) != 4 || categories[0].Name != "儿歌" {
		t.Fatalf("分类不符：%v %v", categories, err)
	}
	if value := hikerCategoryValue(hikerParseRule(rule), "1"); value != "2" {
		t.Fatalf("分类值映射错误：%q", value)
	}
	// 列表：搜索接口是 HTML 模式（搜索截取模式 1）。
	items, _, err := engine.downloader.fetchHikerCatalogPage(context.Background(), site, entry, 1, "", "小兔")
	if err != nil {
		t.Fatalf("列表失败：%v", err)
	}
	if len(items) != 2 || items[0].Title != "小兔儿歌" || items[0].Remark != "03:20" {
		t.Fatalf("列表不符：%+v", items)
	}
	// 详情：选集列表 .video，链接取 video-src 属性。
	drama, chapters, err := engine.downloader.fetchHikerDetail(context.Background(), site, entry, items[0].SourceID)
	if err != nil {
		t.Fatalf("详情失败：%v", err)
	}
	if drama.Title == "" || len(chapters) != 2 || chapters[0].VideoURL != "https://cdn.example.com/txb/1.mp4" {
		t.Fatalf("分集不符：%v %+v", drama.Title, chapters)
	}
}

// TestBiliCategoriesAndCatalog Bili：分类直出 + 搜索接口（wbi 签名）+ 详情分 P。
func TestBiliCategoriesAndCatalog(t *testing.T) {
	navBody := `{"code":0,"data":{"wbi_img":{"img_url":"https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png","sub_url":"https://i0.hdslb.com/bfs/wbi/4932caff0ff746eab6f01bf08b70ac45.png"}}}`
	searchBody := `{"code":0,"data":{"result":[{"bvid":"BV1xx411c7mD","title":"<em class=\"keyword\">搞笑</em>名场面","pic":"//i0.hdslb.com/bfs/archive/a.jpg","description":"合集","duration":"12:30","author":"UP主"}]}}`
	viewBody := `{"code":0,"data":{"title":"搞笑名场面合集","pic":"//i0.hdslb.com/bfs/archive/a.jpg","desc":"简介","pages":[{"page":1,"cid":111,"part":"上集"},{"page":2,"cid":222,"part":"下集"}]}}`
	var searchQuery url.Values
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Path, "/x/web-interface/nav"):
			return sourceFixtureResponse(request, 200, navBody), nil
		case strings.Contains(request.URL.Path, "/wbi/search/type"):
			searchQuery, _ = url.ParseQuery(request.URL.RawQuery)
			return sourceFixtureResponse(request, 200, searchBody), nil
		case strings.Contains(request.URL.Path, "/x/web-interface/view"):
			return sourceFixtureResponse(request, 200, viewBody), nil
		}
		return sourceFixtureResponse(request, 404, "not found"), nil
	})
	rule := ruleFixtureRule(t, "bili_bili.json")
	site := libretvSite{Key: "bili", Name: "B站", API: "http://127.0.0.1:9/", Type: 1, Engine: ruleEngineBili}
	entry := ruleEntry{Key: "bili", Engine: ruleEngineBili, Rule: rule}
	categories, err := engine.downloader.fetchBiliCategories(site, entry)
	if err != nil || len(categories) != 3 || categories[0].Name != "搞笑" {
		t.Fatalf("分类不符：%v %v", categories, err)
	}
	items, _, err := engine.downloader.fetchBiliCatalogPage(context.Background(), site, entry, 1, "1", "")
	if err != nil {
		t.Fatalf("列表失败：%v", err)
	}
	if len(items) != 1 || items[0].Title != "搞笑名场面" {
		t.Fatalf("列表不符：%+v", items)
	}
	// wbi 签名参数必须带上（wts + w_rid）。
	if searchQuery.Get("w_rid") == "" || searchQuery.Get("wts") == "" || searchQuery.Get("keyword") != "搞笑" {
		t.Fatalf("wbi 签名缺失：%v", searchQuery)
	}
	// 详情：分 P 即分集，地址指向节目页。
	_, chapters, err := engine.downloader.fetchBiliDetail(context.Background(), site, entry, items[0].SourceID)
	if err != nil {
		t.Fatalf("详情失败：%v", err)
	}
	if len(chapters) != 2 || !strings.Contains(chapters[0].VideoURL, "bilibili.com/video/BV1xx411c7mD?p=1") {
		t.Fatalf("分集不符：%+v", chapters)
	}
	// 风控 412 要给可读提示。
	blocked := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		return sourceFixtureResponse(request, 412, "blocked"), nil
	})
	_, _, err = blocked.downloader.fetchBiliCatalogPage(context.Background(), site, entry, 1, "1", "")
	if err == nil || !strings.Contains(err.Error(), "风控") {
		t.Fatalf("412 应给出风控提示：%v", err)
	}
}
