package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// libretvFixture 起一个本地 provide/vod 接口，按路径与查询参数返回预置内容。
type libretvFixture struct {
	server    *httptest.Server
	requests  []string
	lastPath  string
	lastQuery string
}

func newLibreTVFixture(t *testing.T, handler func(path string, query map[string]string) (int, string)) *libretvFixture {
	t.Helper()
	fixture := &libretvFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := map[string]string{}
		for key, values := range request.URL.Query() {
			if len(values) > 0 {
				query[key] = values[0]
			}
		}
		fixture.requests = append(fixture.requests, request.URL.RequestURI())
		fixture.lastPath, fixture.lastQuery = request.URL.Path, request.URL.RawQuery
		status, body := handler(request.URL.Path, query)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		writer.Write([]byte(body))
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

// forwardToFixture 把任意请求改道到本地 fixture，保留路径与查询参数。
func forwardToFixture(fixture *libretvFixture) sourceFixtureTransport {
	return sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		target := fixture.server.URL + request.URL.RequestURI()
		forwarded, err := http.NewRequestWithContext(request.Context(), request.Method, target, nil)
		if err != nil {
			return nil, err
		}
		return fixture.server.Client().Do(forwarded)
	})
}

// libretvTestDownloader 带 fixture 传输与空订阅表的 Downloader。
func libretvTestDownloader(t *testing.T, fixture *libretvFixture) *Downloader {
	t.Helper()
	downloader := sourceFixtureDownloader(t, forwardToFixture(fixture))
	downloader.libretv = newLibreTVStore(t.TempDir())
	return downloader
}

const libretvListBody = `{"code":1,"msg":"数据列表","page":1,"pagecount":3,"limit":"20","total":48,"list":[
 {"vod_id":101,"vod_name":"测试剧甲","vod_pic":"//img.example.com/a.jpg","type_name":"国产剧","vod_remarks":"更新至10集","vod_year":"2024","vod_area":"内地","vod_lang":"汉语普通话","vod_hits":"1234","vod_score":"8.5","vod_actor":"张三,李四","vod_director":"王五"},
 {"vod_id":102,"vod_name":"测试剧乙","vod_pic":"https://img.example.com/b.jpg","type_name":"动作片","vod_remarks":"全30集","vod_year":"2023","vod_hits":"56"}
]}`

const libretvDetailBody = `{"code":1,"msg":"数据列表","page":1,"pagecount":1,"limit":"20","total":1,"list":[
 {"vod_id":101,"vod_name":"测试剧甲","vod_pic":"https://img.example.com/a.jpg","type_name":"国产剧","vod_remarks":"更新至10集","vod_year":"2024","vod_area":"内地",
  "vod_score":8.6,"vod_actor":"张三,李四","vod_director":"王五",
  "vod_time":"2026-10-19 20:15:00","vod_tag":"剧情,爱情/悬疑",
  "vod_content":"<p>剧情简介&amp;彩蛋</p>","vod_play_from":"ffm3u8$$$mihua",
  "vod_play_url":"第01集$https://cdn.example.com/a1.m3u8#第02集$https://cdn.example.com/a2.m3u8$$$第01集$https://cdn.example.com/b1.m3u8#第02集$https://cdn.example.com/b2.m3u8"}
]}`

func TestLibreTVParseSubscriptionConfig(t *testing.T) {
	body := `var config = {"spider":"https://example.com/spider.jar","sites":[
 {"key":"dsd","name":"帝屎迪","type":1,"api":"https://api.dsd.com/provide/vod","searchable":1},
 {"key":"drpy","name":"DRPY","type":3,"api":"https://example.com/drpy.js"},
 {"key":"noapi","name":"缺接口","type":1},
 {"name":"无键站点","type":1,"api":"https://other.example.com/api.php/provide/vod"},
 {"name":"重复站点","type":1,"api":"https://api.dsd.com/provide/vod/"}
 ]}`
	sites, drpyCandidates, _, skipped, err := parseLibreTVSubscription(body, "")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(sites) != 2 {
		t.Fatalf("应导入 2 个站点，实际 %d：%+v", len(sites), sites)
	}
	if sites[0].Key != "dsd" || sites[0].Name != "帝屎迪" || sites[0].API != "https://api.dsd.com/provide/vod" {
		t.Fatalf("站点 0 归一化错误: %+v", sites[0])
	}
	if sites[1].Key == "" || sites[1].Key == "dsd" || sites[1].Name != "无键站点" {
		t.Fatalf("站点 1 key 派生错误: %+v", sites[1])
	}
	if sites[1].API != "https://other.example.com/api.php/provide/vod" {
		t.Fatalf("站点 1 api 归一化错误: %+v", sites[1])
	}
	// type=3 的 .js 脚本源收成 drpy 候选（不自动导入），不再计入 skipped。
	if len(drpyCandidates) != 1 || drpyCandidates[0].Name != "DRPY" || drpyCandidates[0].API != "https://example.com/drpy.js" {
		t.Fatalf("drpy 候选提取错误: %+v", drpyCandidates)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "缺接口") {
		t.Fatalf("跳过原因不符合预期: %v", skipped)
	}
}

func TestLibreTVParseSubscriptionRejectsGarbage(t *testing.T) {
	if _, _, _, _, err := parseLibreTVSubscription("not a config at all", ""); err == nil {
		t.Fatal("非配置内容应当报错")
	}
	if _, _, _, _, err := parseLibreTVSubscription(`{"sites":[]}`, ""); err == nil {
		t.Fatal("空 sites 应当报错")
	}
	// 仅有 type=3 的 .js 脚本源：不算失败，返回 drpy 候选由用户确认导入。
	_, drpyCandidates, _, skipped, err := parseLibreTVSubscription(`{"sites":[{"key":"x","name":"x","type":3,"api":"https://a.com/x.js"}]}`, "")
	if err != nil {
		t.Fatalf("仅有 drpy 候选时不应报错: %v", err)
	}
	if len(drpyCandidates) != 1 || drpyCandidates[0].API != "https://a.com/x.js" {
		t.Fatalf("drpy 候选提取错误: %+v", drpyCandidates)
	}
	if len(skipped) != 0 {
		t.Fatalf("合法 .js 候选不应出现在跳过列表: %v", skipped)
	}
}

func TestLibreTVParseSubscriptionMixedDrpyCandidates(t *testing.T) {
	body := `var config = {"spider":"https://example.com/spider.jar","sites":[
 {"key":"a","name":"站点A","type":1,"api":"https://api.a.com/provide/vod"},
 {"key":"dr1","name":"脚本一","type":3,"api":"https://example.com/drpy.js"},
 {"key":"dr2","name":"脚本二","type":4,"api":"https://example.com/script2.js"},
 {"key":"dup","name":"重复脚本","type":3,"api":"https://example.com/drpy.js"},
 {"key":"bad","name":"非脚本","type":3,"api":"https://api.b.com/provide/vod"}
 ]}`
	sites, drpyCandidates, _, skipped, err := parseLibreTVSubscription(body, "")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("应导入 1 个站点，实际 %d：%+v", len(sites), sites)
	}
	// 重复 .js 静默去重，不重复提示。
	if len(drpyCandidates) != 2 {
		t.Fatalf("应提取 2 个 drpy 候选，实际 %d：%+v", len(drpyCandidates), drpyCandidates)
	}
	if drpyCandidates[0].Name != "脚本一" || drpyCandidates[0].API != "https://example.com/drpy.js" {
		t.Fatalf("候选 0 错误: %+v", drpyCandidates[0])
	}
	if drpyCandidates[1].Name != "脚本二" || drpyCandidates[1].API != "https://example.com/script2.js" {
		t.Fatalf("候选 1 错误: %+v", drpyCandidates[1])
	}
	// type≥3 但 api 不是 .js 的带原因跳过。
	if len(skipped) != 1 || !strings.Contains(skipped[0], "非脚本") || !strings.Contains(skipped[0], "暂不支持") {
		t.Fatalf("跳过原因不符合预期: %v", skipped)
	}
}

func TestLibreTVImportSubscriptionDrpyCandidates(t *testing.T) {
	body := `var config = {"sites":[
 {"key":"dr1","name":"脚本一","type":3,"api":"https://example.com/drpy.js"}
 ]}`
	// 纯候选订阅不应发起任何 HTTP 请求。
	engine := sourceFixtureEngine(t, sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		t.Errorf("候选订阅不应发起网络请求: %s", request.URL)
		return nil, http.ErrHandlerTimeout
	}))
	engine.downloader.libretv = loadLibreTVStore(t.TempDir())
	result, err := engine.importLibreTVSubscription(context.Background(), body)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("导入结果类型错误: %T", result)
	}
	drpy, ok := payload["drpy"].([]map[string]any)
	if !ok {
		t.Fatalf("结果缺少 drpy 候选: %+v", payload)
	}
	if len(drpy) != 1 || drpy[0]["name"] != "脚本一" || drpy[0]["api"] != "https://example.com/drpy.js" {
		t.Fatalf("drpy 候选透传错误: %+v", drpy)
	}
	if total, _ := payload["total"].(int); total != 0 {
		t.Fatalf("drpy 候选不应计入已导入站点: %v", total)
	}
}

func TestLibreTVNormalizeSiteKeyFromAPI(t *testing.T) {
	key := libretvKeyFromAPI("https://Caiji.DYTTzyAPI.com/api.php/provide/vod")
	if !libretvKeyPattern.MatchString(key) {
		t.Fatalf("派生 key 非法: %q", key)
	}
	if libretvKeyFromAPI("https://a.com/x") == libretvKeyFromAPI("https://b.com/x") {
		t.Fatal("不同接口地址的 key 不应当相同")
	}
	if _, _, _, ok := normalizeLibreTVSite("", "", "/relative/path", 0); ok {
		t.Fatal("相对地址应当拒绝")
	}
	// key 非法时退化为派生 key。
	key, name, api, ok := normalizeLibreTVSite("bad key!", "", "https://a.com/provide/vod", 0)
	if !ok || key == "bad key!" || name != "a.com" || api != "https://a.com/provide/vod" {
		t.Fatalf("非法 key 未按预期退化: %q %q %q %v", key, name, api, ok)
	}
}

func TestLibreTVSplitDramaAndChapterIDs(t *testing.T) {
	source, sourceID, ok := splitProviderDramaID("libretv:dsd-abc123:101")
	if !ok || source != "libretv:dsd-abc123" || sourceID != "101" {
		t.Fatalf("剧集 ID 切分错误: %q %q %v", source, sourceID, ok)
	}
	source, _, ok = splitProviderDramaID("libretv:dsd-abc123:101:0-1")
	if !ok || source != "libretv:dsd-abc123" {
		t.Fatalf("分集 ID 切分错误: %q %v", source, ok)
	}
	if _, _, ok := splitProviderDramaID("libretv:dsd"); ok {
		t.Fatal("缺片源 ID 的 libretv 剧集应当判无效")
	}
	if _, _, ok := splitProviderDramaID("libretv:dsd:"); ok {
		t.Fatal("空片源 ID 的 libretv 剧集应当判无效")
	}
	if source, _, ok := splitProviderDramaID("hongguo:123"); !ok || source != sourceHongguo {
		t.Fatalf("普通站源切分被破坏: %q %v", source, ok)
	}
}

func TestLibreTVFetchCatalogPage(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		if query["ac"] != "detail" {
			t.Errorf("缺少 ac=detail: %v", query)
		}
		if query["pg"] != "2" {
			t.Errorf("分页参数错误: %v", query)
		}
		return 200, libretvListBody
	})
	downloader := libretvTestDownloader(t, fixture)
	site := libretvSite{Key: "dsd", Name: "帝屎迪", API: fixture.server.URL + "/provide/vod"}
	dramas, hasMore, err := downloader.fetchLibreTVCatalogPage(context.Background(), site, 2, "", "")
	if err != nil {
		t.Fatalf("拉取列表失败: %v", err)
	}
	if len(dramas) != 2 {
		t.Fatalf("应解析 2 部剧，实际 %d", len(dramas))
	}
	first := dramas[0]
	if first.ID != "libretv:dsd:101" || first.Title != "测试剧甲" {
		t.Fatalf("剧集字段错误: %+v", first)
	}
	if first.Cover != "//img.example.com/a.jpg" || first.TypeName != "国产剧" {
		t.Fatalf("海报/分类字段错误: %+v", first)
	}
	if first.Episodes != 10 || first.ReleaseStatus != "ongoing" {
		t.Fatalf("集数/状态解析错误: %+v", first)
	}
	if first.Score != "8.5" || first.Actor != "张三,李四" || first.Director != "王五" {
		t.Fatalf("评分/主演/导演解析错误: %+v", first)
	}
	if second := dramas[1]; second.Episodes != 30 || second.ReleaseStatus != "finished" {
		t.Fatalf("完结状态解析错误: %+v", second)
	} else if second.Score != "" || second.Actor != "" || second.Director != "" {
		// 缺省字段不得产生占位值。
		t.Fatalf("缺省评分/主演/导演应为空: %+v", second)
	}
	if !hasMore {
		t.Fatal("page=2 < pagecount=3，应当还有下一页")
	}
}

func TestLibreTVFetchCatalogPageSearchAndLastPage(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		if query["wd"] == "" {
			t.Errorf("搜索缺少 wd: %v", query)
		}
		if query["pg"] != "1" {
			t.Errorf("搜索应当固定第一页: %v", query)
		}
		return 200, `{"code":1,"page":1,"pagecount":1,"limit":"20","list":[]}`
	})
	downloader := libretvTestDownloader(t, fixture)
	site := libretvSite{Key: "dsd", Name: "帝屎迪", API: fixture.server.URL + "/provide/vod"}
	dramas, hasMore, err := downloader.fetchLibreTVCatalogPage(context.Background(), site, 2, "", "测试")
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(dramas) != 0 || hasMore {
		t.Fatalf("空结果且已到末页: %d %v", len(dramas), hasMore)
	}
}

func TestLibreTVFetchCatalogPageRejectsNonJSON(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		return 200, "<html>网关错误</html>"
	})
	downloader := libretvTestDownloader(t, fixture)
	site := libretvSite{Key: "dsd", Name: "帝屎迪", API: fixture.server.URL + "/provide/vod"}
	if _, _, err := downloader.fetchLibreTVCatalogPage(context.Background(), site, 1, "", ""); err == nil {
		t.Fatal("非 JSON 响应应当报错")
	}
}

func TestLibreTVFetchDetailMultiPlaySources(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		if query["ac"] != "detail" || query["ids"] != "101" {
			t.Errorf("详情参数错误: %v", query)
		}
		return 200, libretvDetailBody
	})
	downloader := libretvTestDownloader(t, fixture)
	site := libretvSite{Key: "dsd", Name: "帝屎迪", API: fixture.server.URL + "/provide/vod"}
	drama, chapters, err := downloader.fetchLibreTVDetail(context.Background(), site, "101")
	if err != nil {
		t.Fatalf("拉取详情失败: %v", err)
	}
	if drama.Title != "测试剧甲" || drama.Desc != "剧情简介&彩蛋" {
		t.Fatalf("详情元数据错误: %+v", drama)
	}
	// vod_score 为数字（8.6）时也要归一化成字符串。
	if drama.Score != "8.6" || drama.Actor != "张三,李四" || drama.Director != "王五" {
		t.Fatalf("详情评分/主演/导演错误: %+v", drama)
	}
	// vod_time 归一化为「YYYY-MM-DD HH:mm」；vod_tag 与地区/语言/年份合并去重。
	if drama.UpdateTime != "2026-10-19 20:15" {
		t.Fatalf("vod_time 归一化错误: %q", drama.UpdateTime)
	}
	wantTags := []string{"剧情", "爱情", "悬疑", "内地", "2024"}
	if strings.Join(drama.Tags, "|") != strings.Join(wantTags, "|") {
		t.Fatalf("vod_tag 合并错误: %v（期望 %v）", drama.Tags, wantTags)
	}
	if len(chapters) != 4 {
		t.Fatalf("两个播放源各 2 集，应有 4 集，实际 %d", len(chapters))
	}
	// 集号相同时追记播放源名，排序后标题序列应为 01/01·mihua/02/02·mihua。
	expectedTitles := []string{"第01集", "第01集·mihua", "第02集", "第02集·mihua"}
	for index, chapter := range chapters {
		if chapter.Title != expectedTitles[index] {
			t.Fatalf("分集 %d 标题错误: %q（期望 %q）", index, chapter.Title, expectedTitles[index])
		}
		if chapter.Source != "libretv:dsd" {
			t.Fatalf("分集 source 错误: %+v", chapter)
		}
		if !strings.HasPrefix(chapter.Referer, "http") {
			t.Fatalf("分集 referer 缺失: %+v", chapter)
		}
	}
	urls := map[string]bool{}
	for _, chapter := range chapters {
		urls[chapter.VideoURL] = true
	}
	for _, expected := range []string{"https://cdn.example.com/a1.m3u8", "https://cdn.example.com/a2.m3u8", "https://cdn.example.com/b1.m3u8", "https://cdn.example.com/b2.m3u8"} {
		if !urls[expected] {
			t.Fatalf("缺少播放地址 %s", expected)
		}
	}
}

func TestLibreTVFetchDetailSingleMovie(t *testing.T) {
	body := `{"code":1,"list":[{"vod_id":9,"vod_name":"单片","vod_play_from":"ffm3u8","vod_play_url":"正片$https://cdn.example.com/only.mp4"}]}`
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		return 200, body
	})
	downloader := libretvTestDownloader(t, fixture)
	site := libretvSite{Key: "k", Name: "N", API: fixture.server.URL + "/provide/vod"}
	_, chapters, err := downloader.fetchLibreTVDetail(context.Background(), site, "9")
	if err != nil {
		t.Fatalf("拉取详情失败: %v", err)
	}
	if len(chapters) != 1 || chapters[0].VideoURL != "https://cdn.example.com/only.mp4" {
		t.Fatalf("单片解析错误: %+v", chapters)
	}
}

func TestLibreTVFetchDetailRejectsInvalidEntries(t *testing.T) {
	body := `{"code":1,"list":[{"vod_id":9,"vod_name":"坏片","vod_play_url":"第01集$not-a-url#第02集$https://ok.example.com/a.mp4"}]}`
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		return 200, body
	})
	downloader := libretvTestDownloader(t, fixture)
	site := libretvSite{Key: "k", Name: "N", API: fixture.server.URL + "/provide/vod"}
	_, chapters, err := downloader.fetchLibreTVDetail(context.Background(), site, "9")
	if err != nil {
		t.Fatalf("拉取详情失败: %v", err)
	}
	if len(chapters) != 1 || chapters[0].VideoURL != "https://ok.example.com/a.mp4" {
		t.Fatalf("非法播放地址未被过滤: %+v", chapters)
	}
}

func TestLibreTVStorePersistence(t *testing.T) {
	directory := t.TempDir()
	store := loadLibreTVStore(directory)
	if _, err := store.put(libretvSite{Key: "dsd", Name: "帝屎迪", API: "https://api.dsd.com/provide/vod"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	reloaded := loadLibreTVStore(directory)
	site, ok := reloaded.get("libretv:dsd")
	if !ok || site.Name != "帝屎迪" {
		t.Fatalf("重载失败: %+v %v", site, ok)
	}
	if _, ok := reloaded.get("libretv:missing"); ok {
		t.Fatal("不存在的站点不应返回")
	}
	if err := reloaded.remove("dsd"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, ok := loadLibreTVStore(directory).get("libretv:dsd"); ok {
		t.Fatal("删除后不应仍然存在")
	}
}

func TestLibreTVUpdateTimeNormalization(t *testing.T) {
	cases := map[string]string{
		"":                          "",
		"2026-10-19 20:15:00":       "2026-10-19 20:15",
		"2026-10-19 20:15":          "2026-10-19 20:15",
		"2026-10-19":                "2026-10-19 00:00",
		"1760866500":                time.Unix(1760866500, 0).Format("2006-01-02 15:04"),
		"1760866500000":             time.Unix(1760866500, 0).Format("2006-01-02 15:04"),
		"2026/10/19 20:15:30 extra": "2026/10/19 20:15:30",
	}
	for raw, want := range cases {
		if got := libretvUpdateTime(raw); got != want {
			t.Fatalf("libretvUpdateTime(%q) = %q，期望 %q", raw, got, want)
		}
	}
}

func TestLibreTVValidNativeCategory(t *testing.T) {
	if !validNativeCategory("libretv:dsd", "12") {
		t.Fatal("数字 type_id 应当有效")
	}
	if validNativeCategory("libretv:dsd", "abc") {
		t.Fatal("非数字分类应当无效")
	}
	if !validNativeCategory("libretv:dsd", "") {
		t.Fatal("空分类（全部）应当有效")
	}
}

// libretvConfigFixture 提供分类与列表/详情内容；订阅配置本身由测试内联返回
// （需要 fixture 地址，而 fixture 启动时地址未知）。
func libretvConfigFixture(t *testing.T) *libretvFixture {
	t.Helper()
	return newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		switch {
		case query["ac"] == "list":
			return 200, `{"code":1,"class":[{"type_id":1,"type_name":"国产剧"},{"type_id":"2","type_name":"动作片"}]}`
		case query["ids"] == "101":
			return 200, libretvDetailBody
		default:
			return 200, libretvListBody
		}
	})
}

func TestLibreTVImportSubscriptionAndRemove(t *testing.T) {
	fixture := libretvConfigFixture(t)
	// 配置里的 api 指向 fixture 自身。
	engine := sourceFixtureEngine(t, forwardToFixture(fixture))
	engine.downloader.libretv = loadLibreTVStore(t.TempDir())
	configURL := fixture.server.URL + "/libretv.json"
	// 重新构造配置内容（需要 fixture 地址，启动时未知）。
	engine.downloader.client.Transport = sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/libretv.json") {
			body := `{"sites":[{"key":"dsd","name":"测试片源","type":1,"api":"` + fixture.server.URL + `/provide/vod"}]}`
			return sourceFixtureResponse(request, 200, body), nil
		}
		return forwardToFixture(fixture).RoundTrip(request)
	})

	// 1) 通过订阅配置导入。
	result, err := engine.importLibreTVSubscription(context.Background(), configURL)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("导入返回类型错误: %T", result)
	}
	added, _ := payload["added"].([]map[string]any)
	if len(added) != 1 || added[0]["name"] != "测试片源" {
		t.Fatalf("导入站点错误: %+v", added)
	}
	if verified, _ := payload["verified"].(int); verified != 1 {
		t.Fatalf("探测未通过: %+v", payload)
	}

	// 2) 站源列表应包含导入站点与缓存数（导入时探测已写入目录）。
	sites := engine.libretvSiteList()
	if len(sites) != 1 {
		t.Fatalf("站点列表错误: %+v", sites)
	}
	key, _ := sites[0]["key"].(string)
	if key != "dsd" {
		t.Fatalf("站点 key 错误: %+v", sites[0])
	}
	if count, _ := sites[0]["count"].(int); count == 0 {
		t.Fatalf("站点缓存数应为正: %+v", sites[0])
	}

	// 3) 目录/详情/分类链路可用。
	source := libretvSourceID(key)
	catalog, err := engine.nativeCatalog(context.Background(), nativeInput{Source: source, Page: 1, Force: true})
	if err != nil {
		t.Fatalf("目录拉取失败: %v", err)
	}
	if len(catalog.Items) != 2 || catalog.Items[0].Source != source {
		t.Fatalf("目录内容错误: %+v", catalog.Items)
	}
	detail, err := engine.nativeDetail(context.Background(), catalog.Items[0])
	if err != nil {
		t.Fatalf("详情拉取失败: %v", err)
	}
	chapters, _ := detail.(map[string]any)["chapters"].([]Chapter)
	if len(chapters) != 4 {
		t.Fatalf("详情应有 4 集，实际 %d: %+v", len(chapters), chapters)
	}
	categories, err := engine.nativeCategories(context.Background(), source, false)
	if err != nil {
		t.Fatalf("分类拉取失败: %v", err)
	}
	if len(categories) != 3 || categories[1].ID != "1" || categories[2].Name != "动作片" {
		t.Fatalf("分类解析错误: %+v", categories)
	}

	// 4) 删除站点后，目录与站点表都应清空。
	if err := engine.removeLibreTVSite(key); err != nil {
		t.Fatalf("删除站点失败: %v", err)
	}
	if sites := engine.libretvSiteList(); len(sites) != 0 {
		t.Fatalf("删除后站点表应为空: %+v", sites)
	}
	engine.mu.Lock()
	_, cached := engine.catalogs[source]
	engine.mu.Unlock()
	if cached {
		t.Fatal("删除后目录缓存应清空")
	}
	if _, _, err := engine.downloader.fetchLibreTVCatalogPage(context.Background(), libretvSite{Key: key, API: "x"}, 1, "", ""); err == nil {
		t.Fatal("无效接口地址应当报错")
	}
}

func TestLibreTVImportBareAPIURL(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		return 200, libretvListBody
	})
	engine := sourceFixtureEngine(t, forwardToFixture(fixture))
	engine.downloader.libretv = loadLibreTVStore(t.TempDir())

	// 直接粘贴裸 provide/vod 地址（非 JSON 配置）也能导入。
	result, err := engine.importLibreTVSubscription(context.Background(), fixture.server.URL+"/api.php/provide/vod")
	if err != nil {
		t.Fatalf("裸接口导入失败: %v", err)
	}
	payload := result.(map[string]any)
	added, _ := payload["added"].([]map[string]any)
	if len(added) != 1 {
		t.Fatalf("裸接口应导入单站点: %+v", added)
	}
	if api, _ := added[0]["api"].(string); api != fixture.server.URL+"/api.php/provide/vod" {
		t.Fatalf("裸接口 api 归一化错误: %q", api)
	}
}

func TestLibreTVImportRejectsUnreachable(t *testing.T) {
	engine := sourceFixtureEngine(t, sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		return nil, http.ErrHandlerTimeout
	}))
	engine.downloader.libretv = loadLibreTVStore(t.TempDir())
	if _, err := engine.importLibreTVSubscription(context.Background(), "https://libretv.invalid/config.json"); err == nil {
		t.Fatal("不可达链接应当报错")
	}
	if _, err := engine.importLibreTVSubscription(context.Background(), "   "); err == nil {
		t.Fatal("空输入应当报错")
	}
}

func TestLibreTVImportMultipleSites(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		time.Sleep(30 * time.Millisecond)
		return 200, libretvListBody
	})
	engine := sourceFixtureEngine(t, forwardToFixture(fixture))
	engine.downloader.libretv = loadLibreTVStore(t.TempDir())
	config := `{"sites":[` +
		`{"key":"k1","name":"站点一","type":1,"api":"` + fixture.server.URL + `/provide/vod"},` +
		`{"key":"k2","name":"站点二","type":1,"api":"` + fixture.server.URL + `/provide/vod2"}]}`
	started := time.Now()
	result, err := engine.importLibreTVSubscription(context.Background(), config)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	payload := result.(map[string]any)
	added, _ := payload["added"].([]map[string]any)
	if len(added) != 2 {
		t.Fatalf("两个站点都应导入: %+v", added)
	}
	if verified, _ := payload["verified"].(int); verified != 2 {
		t.Fatalf("两个站点都应探测通过: %+v", payload)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("导入耗时过长: %v", elapsed)
	}
}
