package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// drpyRuleScript rule 驱动型脚本（无 home/category/detail/search/play），
// 形态与社区「荐片」一类脚本一致：只写 rule，靠运行时默认函数 + 片段。
const drpyRuleScript = `var rule = {
    title: '规则演示',
    host: '__HOST__',
    homeUrl: '/api/tag/hand',
    url: '/api/crumb/list?page=fypage&type=0&fyfilter',
    detailUrl: '/api/node/detail?id=fyid',
    searchUrl: '/api/video/search?key=**&page=fypage',
    class_name: '全部&电影&电视剧',
    class_url: '0&1&2',
    filter_url: 'area={{fl.area or "0"}}&sort={{fl.sort or "update"}}',
    filter_def: {0: {area: '0', sort: 'update'}},
    headers: {'User-Agent': 'rule-demo/1.0', 'JPAUTH': 'token-demo'},
    play_parse: true,
    lazy: '',
    一级: ` + "`js:" + `
        var d = [];
        let html = request(input);
        html = JSON.parse(html).data;
        html.forEach(it => {
            d.push({title: it.title, img: it.thumbnail, desc: it.mask, url: it.id})
        });
        setResult(d);
    ` + "`," + `
    二级: ` + "`js:" + `
        let html = request(input);
        let node = JSON.parse(html).data;
        VOD = {
            vod_id: node.id,
            vod_name: node.title,
            vod_pic: node.thumbnail,
            vod_remarks: node.score,
            vod_content: node.description,
            vod_play_from: '在线',
            vod_play_url: node.new_m3u8_list.map(it => it.title + '$' + it.url).join('#')
        };
    ` + "`," + `
    搜索: ` + "`js:" + `
        var d = [];
        let html = request(input);
        html = JSON.parse(html).data;
        html.forEach(it => {
            d.push({title: it.title, img: it.thumbnail, desc: it.mask, url: it.id})
        });
        setResult(d);
    ` + "`," + `
};
`

func newDrpyRuleFixture(t *testing.T) *libretvFixture {
	t.Helper()
	return newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		switch path {
		case "/api/crumb/list":
			if query["area"] != "0" || query["sort"] != "update" {
				return http.StatusBadRequest, `{"data":[]}`
			}
			return http.StatusOK, `{"data":[
			 {"id":101,"title":"规则剧甲","thumbnail":"https://img.test/a.jpg","mask":"更新至3集"},
			 {"id":102,"title":"规则剧乙","thumbnail":"https://img.test/b.jpg","mask":"全8集"}
			]}`
		case "/api/node/detail":
			return http.StatusOK, `{"data":{"id":"101","title":"规则剧甲","thumbnail":"https://img.test/a.jpg",
				"score":"9.1","description":"简介","new_m3u8_list":[
				{"title":"第01集","url":"https://cdn.test/a1.mp4"},{"title":"第02集","url":"https://cdn.test/a2.mp4"}]}}`
		case "/api/video/search":
			return http.StatusOK, `{"data":[{"id":201,"title":"搜索结果","thumbnail":"https://img.test/s.jpg","mask":"全2集"}]}`
		}
		return http.StatusNotFound, "not found"
	})
}

func TestDrpyRuleBrowse(t *testing.T) {
	fixture := newDrpyRuleFixture(t)
	engine := drpyFixtureEngine(t, fixture)
	script := strings.ReplaceAll(drpyRuleScript, "__HOST__", fixture.server.URL)
	result, err := engine.importDrpyScript(context.Background(), script, "")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	key := result.(map[string]any)["key"].(string)
	site, ok := engine.downloader.drpy.get(drpySourceID(key))
	if !ok {
		t.Fatal("站点未保存")
	}

	// 1) 分类来自 class_name / class_url。
	categories, err := engine.downloader.fetchDrpyCategories(context.Background(), site)
	if err != nil {
		t.Fatalf("分类失败: %v", err)
	}
	if len(categories) != 3 || categories[0].Name != "全部" || categories[1].ID != "1" {
		t.Fatalf("分类错误: %+v", categories)
	}

	// 2) 一级：url 模板 + fyfilter 默认值 + 片段解析。
	dramas, hasMore, err := engine.downloader.fetchDrpyCatalogPage(context.Background(), site, 1, "0", "")
	if err != nil {
		t.Fatalf("目录失败: %v", err)
	}
	if len(dramas) != 2 || dramas[0].Title != "规则剧甲" || dramas[0].Cover == "" {
		t.Fatalf("目录映射错误: %+v", dramas)
	}
	if !hasMore {
		t.Fatal("有内容时应允许翻页")
	}

	// 3) 二级：detailUrl + VOD。
	drama, chapters, err := engine.downloader.fetchDrpyDetail(context.Background(), site, dramas[0].SourceID)
	if err != nil {
		t.Fatalf("详情失败: %v", err)
	}
	// 片段把 score 写进 vod_remarks（与社区脚本一致）。
	if drama.Title != "规则剧甲" || drama.Remark != "9.1" {
		t.Fatalf("详情映射错误: %+v", drama)
	}
	if len(chapters) != 2 || chapters[0].Title != "第01集" {
		t.Fatalf("分集错误: %+v", chapters)
	}
	if chapters[0].VideoURL != "https://cdn.test/a1.mp4" {
		t.Fatalf("直链未保留: %q", chapters[0].VideoURL)
	}

	// 4) 搜索：searchUrl 的 ** 替换 + 搜索片段。
	found, _, err := engine.downloader.fetchDrpyCatalogPage(context.Background(), site, 1, "", "规则")
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(found) != 1 || found[0].Title != "搜索结果" {
		t.Fatalf("搜索结果错误: %+v", found)
	}
}

func TestDrpyRulePlayFallsBackToEpisodeURL(t *testing.T) {
	fixture := newDrpyRuleFixture(t)
	engine := drpyFixtureEngine(t, fixture)
	script := strings.ReplaceAll(drpyRuleScript, "__HOST__", fixture.server.URL)
	result, err := engine.importDrpyScript(context.Background(), script, "")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	key := result.(map[string]any)["key"].(string)
	site, _ := engine.downloader.drpy.get(drpySourceID(key))

	// 分集地址本身是直链：没有 play / lazy 也应能播。
	media, err := engine.downloader.resolveDrpyMedia(context.Background(), Task{
		DramaID: "x",
		Chapter: Chapter{
			Source:   drpySourceID(key),
			ID:       providerChapterID(drpySourceID(key), drpyEncodeSegment("101"), drpyBlobFromParts("在线", "https://cdn.test/a1.mp4")),
			VideoURL: "",
		},
	}, site)
	if err != nil {
		t.Fatalf("直链分集播放失败: %v", err)
	}
	if !strings.HasSuffix(media.URL, "a1.mp4") {
		t.Fatalf("播放地址错误: %q", media.URL)
	}

	// tvbox-xg 专有协议给出明确提示，而不是泛泛的「无法播放」。
	_, err = engine.downloader.resolveDrpyMedia(context.Background(), Task{
		DramaID: "x",
		Chapter: Chapter{
			Source: drpySourceID(key),
			ID:     providerChapterID(drpySourceID(key), drpyEncodeSegment("101"), drpyBlobFromParts("边下边播", "tvbox-xg:abc")),
		},
	}, site)
	if err == nil || !strings.Contains(err.Error(), "tvbox-xg") {
		t.Fatalf("专有协议应给出明确提示，实际: %v", err)
	}
}

func TestDrpyRuleFilterDefaults(t *testing.T) {
	driver := drpyRuleSpec{
		FilterURL: "area={{fl.area or \"0\"}}&sort={{fl.sort or \"update\"}}&year={{fl.year}}",
		FilterDef: map[string]any{"1": map[string]any{"area": "3", "year": "2024"}},
	}
	if got := drpyRuleFilter(driver, "1"); got != "area=3&sort=update&year=2024" {
		t.Fatalf("筛选默认值错误: %q", got)
	}
	if got := drpyRuleFilter(driver, "9"); got != "area=0&sort=update&year=" {
		t.Fatalf("缺省筛选错误: %q", got)
	}
}

func TestDrpyRuleURLTemplates(t *testing.T) {
	spec := drpyRuleSpec{Host: "http://api.test", FilterURL: "a=1"}
	got := drpyRuleURL(spec, "/list?page=fypage&id=fyclass&kw=**&fyfilter", "2", "", "电 影", 3)
	if !strings.HasPrefix(got, "http://api.test/list?") ||
		!strings.Contains(got, "page=3") ||
		!strings.Contains(got, "id=2") ||
		!strings.Contains(got, "kw=%E7%94%B5+%E5%BD%B1") ||
		!strings.Contains(got, "a=1") {
		t.Fatalf("模板替换错误: %q", got)
	}
	if got := drpyRuleURL(spec, "detail?id=fyid", "", "abc", "", 1); got != "http://api.test/detail?id=abc" {
		t.Fatalf("详情模板错误: %q", got)
	}
}

// 用本地 httptest 起 drpy 引擎（与 browse 测试一致）。
func drpyFixtureEngine(t *testing.T, fixture *libretvFixture) *nativeEngine {
	t.Helper()
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.downloads.close)
	engine.downloader.client.Transport = fixtureTransport{server: fixture.server}
	engine.downloader.limiter = newRequestLimiter(3, 0)
	engine.downloader.cfg.Retries = 1
	return engine
}

// fixtureTransport 把请求转发到本地 httptest 服务（保留路径与查询串）。
type fixtureTransport struct {
	server *httptest.Server
}

func (transport fixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	forwarded := request.Clone(request.Context())
	forwarded.URL.Scheme = "http"
	forwarded.URL.Host = strings.TrimPrefix(transport.server.URL, "http://")
	forwarded.RequestURI = ""
	forwarded.Host = ""
	return http.DefaultTransport.RoundTrip(forwarded)
}

// TestDrpyRuleHTMLResponseHint 接口挂了返回 HTML 时，错误要能说明是网页
// 而不是 JSON（而不是只抛 SyntaxError）。
func TestDrpyRuleHTMLResponseHint(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		return http.StatusNotFound, "<html><body>404</body></html>"
	})
	engine := drpyFixtureEngine(t, fixture)
	script := strings.ReplaceAll(drpyRuleScript, "__HOST__", fixture.server.URL)
	result, err := engine.importDrpyScript(context.Background(), script, "")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	site, _ := engine.downloader.drpy.get(drpySourceID(result.(map[string]any)["key"].(string)))
	_, _, err = engine.downloader.fetchDrpyCatalogPage(context.Background(), site, 1, "0", "")
	if err == nil {
		t.Fatal("接口返回网页时应当报错")
	}
	message := err.Error()
	if !strings.Contains(message, "网页") || !strings.Contains(message, "一级解析失败") {
		t.Fatalf("错误文案缺少上下文: %v", message)
	}
	if len([]rune(message)) > 200 {
		t.Fatalf("错误文案超长会被截断: %d 字", len([]rune(message)))
	}
}
