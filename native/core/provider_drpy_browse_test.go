package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// drpyBrowseScript 真实形态的 drpy2 脚本：category/detail/search 都是
// async 函数，内部 await req(...) 拉取 JSON 后原样透传（drpy2 里最常见的
// 「API 透传」型脚本），home 返回静态分类。__HOST__ 导入前替换为 fixture
// 地址，从而让 req 打到本地 httptest 服务。
const drpyBrowseScript = `var rule = {
    title: '演示影视',
    host: '__HOST__',
    url: '/index.php/api/vod',
    headers: {'User-Agent': MOBILE_UA},
    timeout: 5000,
    play_parse: false,
    lazy: 'https://example.com/lazy',
    double: false,
    推荐: function(){ },
    一级: function(){ },
    二级: function(){ },
    搜索: function(){ },
    home: function(filter){
        return {class: [{type_id: '1', type_name: '国产剧'}, {type_id: '2', type_name: '动作片'}]};
    },
    category: async function(tid, pg, filter, extend){
        let url = rule.host + '/index.php/api/vod?ac=detail&t=' + tid + '&pg=' + pg;
        let res = await req(url, {headers: rule.headers});
        return JSON.parse(res.content);
    },
    detail: async function(vodId){
        let url = rule.host + '/index.php/api/vod?ac=detail&ids=' + encodeURIComponent(vodId);
        let res = await req(url);
        return JSON.parse(res.content);
    },
    search: async function(wd, quick, pg){
        let url = rule.host + '/index.php/api/vod?ac=detail&wd=' + encodeURIComponent(wd);
        let res = await req(url);
        return JSON.parse(res.content);
    },
    play: function(flag, episodeKey, flags){
        return {url: 'https://video.test/play/' + episodeKey + '.m3u8', jx: 0, parse: 0};
    }
};
function init(ext){}
function home(filter){ return rule.home(filter); }
function category(tid, pg, filter, extend){ return rule.category(tid, pg, filter, extend); }
function detail(vodId){ return rule.detail(vodId); }
function search(wd, quick, pg){ return rule.search(wd, quick, pg); }
function play(flag, episodeKey, flags){ return rule.play(flag, episodeKey, flags); }
`

const drpyPage1Body = `{"code":1,"msg":"数据列表","page":1,"pagecount":2,"limit":"20","total":3,"list":[
 {"vod_id":101,"vod_name":"演示剧甲","vod_pic":"https://img.test/a.jpg","type_name":"国产剧","vod_remarks":"更新至10集","vod_year":"2024"},
 {"vod_id":102,"vod_name":"演示剧乙","vod_pic":"https://img.test/b.jpg","type_name":"动作片","vod_remarks":"全12集"}
]}`

const drpyPage2Body = `{"code":1,"msg":"数据列表","page":2,"pagecount":2,"limit":"20","total":3,"list":[
 {"vod_id":103,"vod_name":"演示剧丙","vod_pic":"https://img.test/c.jpg","type_name":"国产剧","vod_remarks":"全8集"}
]}`

const drpySearchBody = `{"code":1,"msg":"数据列表","page":1,"pagecount":1,"limit":"20","total":1,"list":[
 {"vod_id":101,"vod_name":"演示剧甲","vod_pic":"https://img.test/a.jpg","type_name":"国产剧","vod_remarks":"更新至10集"}
]}`

const drpyDetailBody = `{"code":1,"msg":"数据列表","page":1,"pagecount":1,"limit":"20","total":1,"list":[
 {"vod_id":101,"vod_name":"演示剧甲","vod_pic":"https://img.test/a.jpg","type_name":"国产剧","vod_remarks":"更新至10集",
  "vod_content":"<p>剧情简介&amp;彩蛋</p>",
  "vod_play_from":"ffm3u8$$$mihua",
  "vod_play_url":"第01集$https://cdn.test/a1.m3u8#第02集$https://cdn.test/a2.m3u8$$$第01集$https://cdn.test/b1.m3u8#第02集$https://cdn.test/b2.m3u8"}
]}`

// drpyBrowseFixture 起本地 API 服务，记录请求查询参数与 UA。
type drpyBrowseFixture struct {
	server  *httptest.Server
	queries []string
	agents  []string
}

func newDrpyBrowseFixture(t *testing.T) *drpyBrowseFixture {
	t.Helper()
	fixture := &drpyBrowseFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		fixture.queries = append(fixture.queries, request.URL.RequestURI())
		fixture.agents = append(fixture.agents, request.Header.Get("User-Agent"))
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case query.Get("t") == "1" && query.Get("pg") == "2":
			_, _ = writer.Write([]byte(drpyPage2Body))
		case query.Get("t") == "1":
			_, _ = writer.Write([]byte(drpyPage1Body))
		case query.Get("wd") != "":
			_, _ = writer.Write([]byte(drpySearchBody))
		case query.Get("ids") == "101":
			_, _ = writer.Write([]byte(drpyDetailBody))
		default:
			t.Errorf("意外的脚本请求: %s", request.URL.RequestURI())
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

// importDrpyBrowseEngine 导入 host 指向 fixture 的脚本，返回引擎与站点。
func importDrpyBrowseEngine(t *testing.T) (*nativeEngine, drpySite, *drpyBrowseFixture) {
	t.Helper()
	fixture := newDrpyBrowseFixture(t)
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		forwarded, err := http.NewRequestWithContext(request.Context(), request.Method, request.URL.String(), nil)
		if err != nil {
			return nil, err
		}
		// 带上原始请求头，fixture 才能校验脚本 req 的 UA 等头部。
		forwarded.Header = request.Header.Clone()
		return fixture.server.Client().Do(forwarded)
	})
	script := strings.ReplaceAll(drpyBrowseScript, "__HOST__", fixture.server.URL)
	result, err := engine.importDrpyScript(context.Background(), script)
	if err != nil {
		t.Fatalf("导入脚本失败: %v", err)
	}
	key, _ := result.(map[string]any)["key"].(string)
	if key == "" {
		t.Fatalf("导入结果缺 key: %+v", result)
	}
	site, ok := engine.downloader.drpySite(drpySourceID(key))
	if !ok {
		t.Fatalf("导入后找不到站点 %q", key)
	}
	return engine, site, fixture
}

func TestDrpyFetchCatalogPageCategoryPaging(t *testing.T) {
	engine, site, fixture := importDrpyBrowseEngine(t)
	ctx := context.Background()
	dramas, hasMore, err := engine.downloader.fetchDrpyCatalogPage(ctx, site, 1, "1", "")
	if err != nil {
		t.Fatalf("分类第一页失败: %v", err)
	}
	if len(dramas) != 2 || !hasMore {
		t.Fatalf("第一页应有 2 部剧且还有下一页: %d %v", len(dramas), hasMore)
	}
	first := dramas[0]
	if first.Title != "演示剧甲" || first.SourceID != drpyEncodeSegment("101") {
		t.Fatalf("列表字段错误: %+v", first)
	}
	if first.ID != providerDramaID(drpySourceID(site.Key), drpyEncodeSegment("101")) {
		t.Fatalf("剧集 ID 编码错误: %q", first.ID)
	}
	if first.Cover != "https://img.test/a.jpg" || first.TypeName != "国产剧" {
		t.Fatalf("海报/分类字段错误: %+v", first)
	}
	if first.Episodes != 10 || first.ReleaseStatus != "ongoing" {
		t.Fatalf("集数/状态解析错误: %+v", first)
	}
	// 第二页：末页 hasMore=false。
	dramas, hasMore, err = engine.downloader.fetchDrpyCatalogPage(ctx, site, 2, "1", "")
	if err != nil {
		t.Fatalf("分类第二页失败: %v", err)
	}
	if len(dramas) != 1 || hasMore {
		t.Fatalf("第二页应为 1 部剧且无下一页: %d %v", len(dramas), hasMore)
	}
	// 脚本内 req 走了移动端 UA。
	for _, agent := range fixture.agents {
		if agent != drpyMobileUA {
			t.Fatalf("req 未带移动端 UA: %q", agent)
		}
	}
	// 请求确实带了分页参数。
	if !strings.Contains(strings.Join(fixture.queries, " "), "pg=2") {
		t.Fatalf("缺少 pg=2 请求: %v", fixture.queries)
	}
}

func TestDrpyFetchCatalogPageSearch(t *testing.T) {
	engine, site, fixture := importDrpyBrowseEngine(t)
	dramas, hasMore, err := engine.downloader.fetchDrpyCatalogPage(context.Background(), site, 1, "", "演示")
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(dramas) != 1 || dramas[0].Title != "演示剧甲" || hasMore {
		t.Fatalf("搜索结果异常: %+v hasMore=%v", dramas, hasMore)
	}
	if !strings.Contains(strings.Join(fixture.queries, " "), "wd=") {
		t.Fatalf("搜索未带 wd 参数: %v", fixture.queries)
	}
}

func TestDrpyFetchCategories(t *testing.T) {
	engine, site, _ := importDrpyBrowseEngine(t)
	categories, err := engine.downloader.fetchDrpyCategories(context.Background(), site)
	if err != nil {
		t.Fatalf("home({}) 取分类失败: %v", err)
	}
	if len(categories) != 2 || categories[0].ID != "1" || categories[0].Name != "国产剧" || categories[1].ID != "2" {
		t.Fatalf("分类解析错误: %+v", categories)
	}
}

func TestDrpyFetchDetailChapters(t *testing.T) {
	engine, site, fixture := importDrpyBrowseEngine(t)
	source := drpySourceID(site.Key)
	drama, chapters, err := engine.downloader.fetchDrpyDetail(context.Background(), site, drpyEncodeSegment("101"))
	if err != nil {
		t.Fatalf("详情失败: %v", err)
	}
	if drama.Title != "演示剧甲" || drama.Desc != "剧情简介&彩蛋" {
		t.Fatalf("详情元数据错误: %+v", drama)
	}
	if drama.ID != providerDramaID(source, drpyEncodeSegment("101")) {
		t.Fatalf("详情剧集 ID 错误: %q", drama.ID)
	}
	// 两个播放源各 2 集，共 4 集；集号重复时追记播放源名。
	if len(chapters) != 4 {
		t.Fatalf("应有 4 集，实际 %d: %+v", len(chapters), chapters)
	}
	expectedTitles := []string{"第01集", "第01集·mihua", "第02集", "第02集·mihua"}
	for index, chapter := range chapters {
		if chapter.Title != expectedTitles[index] {
			t.Fatalf("分集 %d 标题错误: %q（期望 %q）", index, chapter.Title, expectedTitles[index])
		}
		if chapter.Source != source {
			t.Fatalf("分集 source 错误: %+v", chapter)
		}
		// 直链保留为 VideoURL（play() 失败时的兜底）。
		if chapter.VideoURL == "" || !strings.HasPrefix(chapter.VideoURL, "https://cdn.test/") {
			t.Fatalf("分集直链丢失: %+v", chapter)
		}
		// 分集 ID 为 flag+episodeKey 的 blob，可反解回播放参数。
		// splitProviderDramaID 对 drpy 返回 (source, "<b64vod>:<b64blob>")，
		// blob 段由调用方再切一刀（M3 播放链路同款用法）。
		_, sourceID, valid := splitProviderDramaID(chapter.ID)
		if !valid {
			t.Fatalf("分集 ID 无法切分: %q", chapter.ID)
		}
		vodPart, episode, found := strings.Cut(sourceID, ":")
		if !found {
			t.Fatalf("分集 ID 缺 blob 段: %q", chapter.ID)
		}
		flag, episodeKey, ok := drpyPartsFromBlob(episode)
		// drpy2 约定：episodeKey 是 $ 后的地址/ID 段（喂给 play），
		// 不是集名；集名只在 Chapter.Title。
		if !ok || !strings.HasPrefix(episodeKey, "https://cdn.test/") {
			t.Fatalf("分集 blob 反解异常: %q -> flag=%q key=%q", chapter.ID, flag, episodeKey)
		}
		if vodPart != drpyEncodeSegment("101") {
			t.Fatalf("分集 vodId 段错误: %q", vodPart)
		}
	}
	// Referer 来自 rule.host。
	if !strings.HasPrefix(chapters[0].Referer, "http://") {
		t.Fatalf("分集 referer 异常: %q", chapters[0].Referer)
	}
	// 详情请求带 ids=101。
	if !strings.Contains(strings.Join(fixture.queries, " "), "ids=101") {
		t.Fatalf("详情请求参数错误: %v", fixture.queries)
	}
}

func TestDrpyNativeCatalogAndDetail(t *testing.T) {
	engine, site, _ := importDrpyBrowseEngine(t)
	source := drpySourceID(site.Key)
	ctx := context.Background()
	// 浏览链路入口：nativeCatalog 分类分页。
	result, err := engine.nativeCatalog(ctx, nativeInput{Action: "catalog", Source: source, Page: 1, Category: "1"})
	if err != nil {
		t.Fatalf("nativeCatalog 失败: %v", err)
	}
	if len(result.Items) != 2 || !result.HasMore {
		t.Fatalf("nativeCatalog 结果异常: %+v", result)
	}
	// 搜索入口。
	result, err = engine.nativeCatalog(ctx, nativeInput{Action: "catalog", Source: source, Page: 1, Query: "演示"})
	if err != nil {
		t.Fatalf("nativeCatalog 搜索失败: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Title != "演示剧甲" {
		t.Fatalf("nativeCatalog 搜索结果异常: %+v", result)
	}
	// 详情入口：nativeDetail 按剧集 ID 反查。
	first := result.Items[0]
	detail, err := engine.nativeDetail(ctx, first)
	if err != nil {
		t.Fatalf("nativeDetail 失败: %v", err)
	}
	payload, ok := detail.(map[string]any)
	if !ok {
		t.Fatalf("nativeDetail 返回类型异常: %T", detail)
	}
	chapters, _ := payload["chapters"].([]Chapter)
	if len(chapters) != 4 {
		t.Fatalf("nativeDetail 分集数异常: %+v", payload)
	}
	if drama, valid := payload["drama"].(nativeDrama); !valid || drama.Title != "演示剧甲" {
		t.Fatalf("nativeDetail 剧集元数据异常: %+v", payload)
	}
}

// drpy 分类即脚本 home() 返回的 type_id，多为数字或短字符串，
// validNativeCategory 靠通用约束兜底（区别于 libretv 的数字校验）。
func TestDrpyValidNativeCategory(t *testing.T) {
	source := drpySourceID("demo")
	if !validNativeCategory(source, "") {
		t.Fatal("空分类（全部）应放行")
	}
	if !validNativeCategory(source, "1") || !validNativeCategory(source, "guochan-ju") {
		t.Fatal("数字与字符串分类都应放行")
	}
	if validNativeCategory(source, strings.Repeat("x", 129)) {
		t.Fatal("超长分类应拒绝")
	}
	if validNativeCategory(source, "a|b") || validNativeCategory(source, "a/b") {
		t.Fatal("含分隔符的分类应拒绝")
	}
	// 非 drpy 源的字符串分类仍走原逻辑（不应被 drpy 分支放行）。
	if validNativeCategory(sourceHongguo, "任意字符串") {
		t.Fatal("drpy 分支不得影响其他站源")
	}
}

func TestDrpyBrowseRejectsRemovedSource(t *testing.T) {
	engine, site, _ := importDrpyBrowseEngine(t)
	source := drpySourceID(site.Key)
	if err := engine.removeDrpySite(site.Key); err != nil {
		t.Fatalf("删除站点失败: %v", err)
	}
	if _, err := engine.nativeCatalog(context.Background(), nativeInput{Action: "catalog", Source: source, Page: 1}); err == nil || !strings.Contains(err.Error(), "未导入或已删除") {
		t.Fatalf("删除后浏览应报未导入错误，得到 %v", err)
	}
	drama := nativeDrama{ID: providerDramaID(source, drpyEncodeSegment("101")), Source: source, Title: "演示剧甲"}
	if _, err := engine.nativeDetail(context.Background(), drama); err == nil || !strings.Contains(err.Error(), "未导入或已删除") {
		t.Fatalf("删除后详情应报未导入错误，得到 %v", err)
	}
}
