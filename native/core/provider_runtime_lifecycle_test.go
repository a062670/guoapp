package core

// 运行期源（LibreTV / drpy / CatVod）的「真实使用路径」回归：
//
//  1. 导入 → 关闭引擎 → 用同一目录重新启动 → 再浏览 / 详情 / 取流；
//  2. 站源「检测」（app_source_health.go）的 5 个步骤全流程。
//
// 这两条路径以前没有测试覆盖，线上先后出现过：
//  - 重启后 drpy.json 丢 engine 字段 → `_ is not defined`
//  - 没有 homeVod 的蜘蛛 → 首页 `data.list` 报错
//  - 检测第 2 步对脚本源报 unsupported provider source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runtimeFixture 一个同时提供 LibreTV 采集接口、drpy 脚本、CatVod 蜘蛛与
// 媒体文件的本地服务。
type runtimeFixture struct {
	server *httptest.Server
	base   string
}

func newRuntimeFixture(t *testing.T) *runtimeFixture {
	t.Helper()
	fixture := &runtimeFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		base := fixture.base
		query := request.URL.Query()
		writeJSON := func(body string) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(body))
		}
		switch request.URL.Path {
		case "/libretv/api.php/provide/vod", "/drpy/api", "/cat/api":
			if query.Get("ac") == "detail" {
				writeJSON(fmt.Sprintf(`{"code":1,"page":1,"pagecount":1,"limit":"20","total":1,"list":[
				 {"vod_id":1,"vod_name":"接口剧","vod_pic":"%s/img/a.jpg","vod_remarks":"全2集",
				  "vod_play_from":"源","vod_play_url":"第01集$%s/media/a.mp4#第02集$%s/media/a.mp4"}]}`,
					base, base, base))
				return
			}
			writeJSON(fmt.Sprintf(`{"code":1,"page":1,"pagecount":1,"limit":"20","total":1,
				"class":[{"type_id":1,"type_name":"电影"}],
				"list":[{"vod_id":1,"vod_name":"接口剧","vod_pic":"%s/img/a.jpg","vod_remarks":"全2集","vod_year":"2024"}]}`, base))
		case "/sub.json":
			writeJSON(`{"sites":[{"key":"catsrc","name":"示例蜘蛛","type":3,"api":"./cat/demo.js"}]}`)
		case "/drpy/fn.js":
			writeJSON(drpyFunctionScript(base))
		case "/drpy/rule.js":
			writeJSON(strings.ReplaceAll(drpyRuleScript, "__HOST__", base))
		case "/cat/demo.js":
			writeJSON(catVodDemoSpider)
		case "/cat/nohomevod.js":
			writeJSON(catVodNoHomeVodSpider(base))
		case "/cat/post.js":
			writeJSON(catVodPostSpider(base))
		case "/cat/post":
			payload, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(payload), `"subcateId":56`) {
				writeJSON(`{"result":{"items":[]}}`)
				return
			}
			writeJSON(fmt.Sprintf(`{"result":{"items":[{"url":"v1","title":"POST剧","image":"%s/img/a.jpg","viewcount":10}]}}`, base))
		case "/cat/jsonplay.js":
			writeJSON(catVodJSONPlaySpider(base))
		case "/cat/lib/cat.js":
			writeJSON(catVodDemoLib)
		case "/api/crumb/list":
			writeJSON(fmt.Sprintf(`{"data":[
			 {"id":101,"title":"规则剧甲","thumbnail":"%s/img/a.jpg","mask":"更新至3集"},
			 {"id":102,"title":"规则剧乙","thumbnail":"%s/img/a.jpg","mask":"全8集"}]}`, base, base))
		case "/api/node/detail":
			writeJSON(fmt.Sprintf(`{"data":{"id":"101","title":"规则剧甲","thumbnail":"%s/img/a.jpg",
				"score":"9.1","description":"简介","new_m3u8_list":[
				{"title":"第01集","url":"%s/media/a.mp4"}]}}`, base, base))
		case "/api/video/search":
			writeJSON(fmt.Sprintf(`{"data":[{"id":201,"title":"搜索结果","thumbnail":"%s/img/a.jpg","mask":"全2集"}]}`, base))
		case "/media/a.mp4", "/a.mp4":
			writer.Header().Set("Content-Type", "video/mp4")
			writer.Header().Set("Accept-Ranges", "bytes")
			if request.Header.Get("Range") != "" {
				writer.WriteHeader(http.StatusPartialContent)
			}
			_, _ = writer.Write([]byte(strings.Repeat("m", 2048)))
		case "/img/a.jpg":
			writer.Header().Set("Content-Type", "image/jpeg")
			_, _ = writer.Write([]byte("jpeg"))
		default:
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte("not found"))
		}
	}))
	fixture.base = fixture.server.URL
	t.Cleanup(fixture.server.Close)
	return fixture
}

// runtimeTransport 把所有出站请求改写到 fixture（保留方法、路径、请求头）。
func runtimeTransport(base string) sourceFixtureTransport {
	return sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		forwarded := request.Clone(request.Context())
		forwarded.URL.Scheme = "http"
		forwarded.URL.Host = strings.TrimPrefix(base, "http://")
		forwarded.RequestURI = ""
		forwarded.Host = ""
		return http.DefaultTransport.RoundTrip(forwarded)
	})
}

// drpyFunctionScript 函数式脚本（有 home / category / detail / play）。
func drpyFunctionScript(base string) string {
	return fmt.Sprintf(`var rule = { title: '函数脚本', host: '%[1]s' };
function init(ext) {}
function home(filter) { return {class: [{type_id: '1', type_name: '电影'}]}; }
async function category(tid, pg, filter, extend) {
    let res = await req('%[1]s/drpy/api?ac=list&pg=' + pg);
    return JSON.parse(res.content);
}
async function detail(id) {
    let res = await req('%[1]s/drpy/api?ac=detail&ids=' + encodeURIComponent(id));
    return JSON.parse(res.content);
}
async function search(wd, quick, pg) {
    let res = await req('%[1]s/drpy/api?ac=list&wd=' + encodeURIComponent(wd));
    return JSON.parse(res.content);
}
function play(flag, key, flags) { return {url: key, jx: 0, parse: 0}; }
`, base)
}

// catVodNoHomeVodSpider 没有 homeVod 的蜘蛛（Bookan 的形态）。
func catVodNoHomeVodSpider(base string) string {
	return fmt.Sprintf(`import { _ } from './lib/cat.js';
let key = '无首页';
let HOST = '%[1]s';
async function init(cfg) {}
async function home(filter) {
	return JSON.stringify({ class: [{ type_id: '1', type_name: '电影' }] });
}
async function category(tid, pg, filter, extend) {
	return JSON.stringify({ page: 1, pagecount: 1, limit: 20, total: 1, list: [
		{ vod_id: 'v1', vod_name: '无首页剧', vod_pic: '%[1]s/img/a.jpg', vod_remarks: '全2集' }] });
}
async function detail(id) {
	return JSON.stringify({ list: [{ vod_id: id, vod_name: '无首页剧',
		vod_play_from: '源', vod_play_url: '第01集$%[1]s/media/a.mp4' }] });
}
async function play(flag, id, flags) { return JSON.stringify({ parse: 0, url: id }); }
export function __jsEvalReturn() {
	return { init: init, home: home, category: category, detail: detail, play: play };
}
`, base)
}

// newRuntimeEngine 在指定目录上建引擎并接上 fixture 传输。
func newRuntimeEngine(t *testing.T, directory, base string) *nativeEngine {
	t.Helper()
	engine, err := newNativeEngine(directory)
	if err != nil {
		t.Fatal(err)
	}
	engine.downloader.client.Transport = runtimeTransport(base)
	engine.downloader.limiter = newRequestLimiter(4, 0)
	engine.downloader.cfg.Retries = 1
	return engine
}

// importRuntimeSource 按来源类型导入，返回完整源 ID。
func importRuntimeSource(t *testing.T, engine *nativeEngine, kind, address string) string {
	t.Helper()
	switch kind {
	case "libretv":
		result, err := engine.importLibreTVSubscription(context.Background(), address)
		if err != nil {
			t.Fatalf("导入 LibreTV 失败: %v", err)
		}
		added := result.(map[string]any)["added"].([]map[string]any)
		if len(added) == 0 {
			t.Fatal("没有导入任何站点")
		}
		return sourceLibreTVPrefix + added[0]["key"].(string)
	default:
		result, err := engine.importDrpyScript(context.Background(), address)
		if err != nil {
			t.Fatalf("导入脚本失败: %v", err)
		}
		return sourceDrpyPrefix + result.(map[string]any)["key"].(string)
	}
}

// TestRuntimeSourcesSurviveRestart 导入 → 重启（同一目录重新建引擎）→
// 分类 / 首页 / 分类页 / 详情 / 取流 全链路。
func TestRuntimeSourcesSurviveRestart(t *testing.T) {
	fixture := newRuntimeFixture(t)
	cases := []struct {
		name string
		kind string
		path string
	}{
		{name: "libretv", kind: "libretv", path: "/libretv/api.php/provide/vod"},
		{name: "drpy-function", kind: "drpy", path: "/drpy/fn.js"},
		{name: "drpy-rule", kind: "drpy", path: "/drpy/rule.js"},
		{name: "catvod", kind: "drpy", path: "/cat/demo.js"},
		{name: "catvod-no-homevod", kind: "drpy", path: "/cat/nohomevod.js"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			first := newRuntimeEngine(t, directory, fixture.base)
			source := importRuntimeSource(t, first, testCase.kind, fixture.base+testCase.path)
			first.downloads.close()

			// 模拟重启：同一目录重新建引擎，所有状态只能来自磁盘。
			second := newRuntimeEngine(t, directory, fixture.base)
			t.Cleanup(second.downloads.close)
			ctx := context.Background()

			// 1) 入口（首页，空分类）。
			home, err := second.nativeCatalog(ctx, nativeInput{Source: source, Page: 1, Force: true})
			if err != nil {
				t.Fatalf("重启后首页失败: %v", err)
			}
			if len(home.Items) == 0 {
				t.Fatal("重启后首页为空")
			}

			// 2) 分类列表。
			categories, err := second.nativeCategories(ctx, source, true)
			if err != nil {
				t.Fatalf("重启后分类失败: %v", err)
			}
			if len(categories) == 0 {
				t.Fatal("重启后分类为空")
			}
			categoryID := ""
			for _, category := range categories {
				if category.ID != "" {
					categoryID = category.ID
					break
				}
			}

			// 3) 分类页（有分类 ID 的路径）。
			if categoryID != "" {
				page, err := second.nativeCatalog(ctx, nativeInput{
					Source: source, Category: categoryID, Page: 1, Force: true,
				})
				if err != nil {
					t.Fatalf("重启后分类页失败: %v", err)
				}
				if len(page.Items) == 0 {
					t.Fatalf("分类 %q 没有内容", categoryID)
				}
			}

			// 4) 详情 + 分集。
			payload, err := second.nativeDetail(ctx, home.Items[0])
			if err != nil {
				t.Fatalf("重启后详情失败: %v", err)
			}
			detail, ok := payload.(map[string]any)
			if !ok {
				t.Fatalf("详情返回类型异常: %T", payload)
			}
			chapters, ok := detail["chapters"].([]Chapter)
			if !ok || len(chapters) == 0 {
				t.Fatalf("详情没有分集: %+v", detail["chapters"])
			}

			// 5) 取流。
			media, err := second.downloader.resolveProviderMedia(ctx, Task{
				DramaID: home.Items[0].ID, Chapter: chapters[0],
			})
			if err != nil {
				t.Fatalf("重启后取流失败: %v", err)
			}
			if !strings.HasSuffix(media.URL, "a.mp4") {
				t.Fatalf("播放地址异常: %q", media.URL)
			}
		})
	}
}

// TestRuntimeSourceHealthCheck 站源「检测」的 5 个步骤全流程。
func TestRuntimeSourceHealthCheck(t *testing.T) {
	fixture := newRuntimeFixture(t)
	cases := []struct {
		name string
		kind string
		path string
	}{
		{name: "libretv", kind: "libretv", path: "/libretv/api.php/provide/vod"},
		{name: "drpy-function", kind: "drpy", path: "/drpy/fn.js"},
		{name: "drpy-rule", kind: "drpy", path: "/drpy/rule.js"},
		{name: "catvod", kind: "drpy", path: "/cat/demo.js"},
		{name: "catvod-no-homevod", kind: "drpy", path: "/cat/nohomevod.js"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			engine := newRuntimeEngine(t, t.TempDir(), fixture.base)
			t.Cleanup(engine.downloads.close)
			source := importRuntimeSource(t, engine, testCase.kind, fixture.base+testCase.path)

			err := engine.checkSource(context.Background(), source, nativeDrama{}, true)
			health := engine.sourceStatus(source).Health
			if health == nil {
				t.Fatal("没有生成检测报告")
			}
			if err != nil || health.State != "ok" || len(health.Steps) != 5 {
				t.Fatalf("检测未通过（%d 步）：err=%v state=%s steps=%+v",
					len(health.Steps), err, health.State, health.Steps)
			}
			for _, step := range health.Steps {
				if step.State != "ok" {
					t.Fatalf("步骤 %s 失败：%s", step.Name, step.Message)
				}
			}
		})
	}
}

// TestRuntimeSourceHealthCheckReportsFailureStep 失败时要停在具体步骤，
// 并带上可读原因（而不是只报 unsupported provider source）。
func TestRuntimeSourceHealthCheckReportsFailureStep(t *testing.T) {
	fixture := newRuntimeFixture(t)
	engine := newRuntimeEngine(t, t.TempDir(), fixture.base)
	t.Cleanup(engine.downloads.close)
	source := importRuntimeSource(t, engine, "drpy", fixture.base+"/drpy/rule.js")

	// 删掉站点：检测应该在第一步给出「未导入」而不是崩溃。
	if err := engine.removeDrpySite(drpyKeyFromSource(source)); err != nil {
		t.Fatal(err)
	}
	err := engine.checkSource(context.Background(), source, nativeDrama{}, true)
	health := engine.sourceStatus(source).Health
	if err == nil || health == nil || health.State != "failed" || len(health.Steps) != 1 {
		t.Fatalf("删除后检测行为异常: err=%v health=%+v", err, health)
	}
	if !strings.Contains(health.Steps[0].Message, "未导入") && !strings.Contains(health.Steps[0].Message, "删除") {
		t.Fatalf("失败原因不够具体: %s", health.Steps[0].Message)
	}
}

var _ = errors.New

// TestMixedSubscriptionCandidateImportThenBrowse 混合订阅 → 候选 → 导入 →
// 重启 → 浏览：用户从「0.json 一键导入」到真正能看的完整链路。
func TestMixedSubscriptionCandidateImportThenBrowse(t *testing.T) {
	fixture := newRuntimeFixture(t)
	engine := newRuntimeEngine(t, t.TempDir(), fixture.base)
	t.Cleanup(engine.downloads.close)

	result, err := engine.importLibreTVSubscription(context.Background(), fixture.base+"/sub.json")
	if err != nil {
		t.Fatalf("订阅导入失败: %v", err)
	}
	payload := result.(map[string]any)
	candidates, _ := payload["drpy"].([]map[string]any)
	if len(candidates) != 1 {
		t.Fatalf("应提取 1 个脚本候选: %+v", payload)
	}
	address, _ := candidates[0]["api"].(string)
	if !strings.HasSuffix(address, "/cat/demo.js") {
		t.Fatalf("候选地址未按订阅地址补全: %q", address)
	}

	imported, err := engine.importDrpyScript(context.Background(), address)
	if err != nil {
		t.Fatalf("候选导入失败: %v", err)
	}
	source := sourceDrpyPrefix + imported.(map[string]any)["key"].(string)

	// 重启后再浏览：候选导入的站点同样要能活过重启。
	engine.downloads.close()
	restarted := newRuntimeEngine(t, engine.directory, fixture.base)
	t.Cleanup(restarted.downloads.close)
	page, err := restarted.nativeCatalog(context.Background(), nativeInput{Source: source, Page: 1, Force: true})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("重启后浏览失败: err=%v items=%d", err, len(page.Items))
	}
}

// TestRuntimeSourceImportTwiceUpdatesInPlace 同一个源导入两次：不重复建站点，
// 且第二次之后仍能正常浏览（脚本更新场景）。
func TestRuntimeSourceImportTwiceUpdatesInPlace(t *testing.T) {
	fixture := newRuntimeFixture(t)
	engine := newRuntimeEngine(t, t.TempDir(), fixture.base)
	t.Cleanup(engine.downloads.close)

	first := importRuntimeSource(t, engine, "drpy", fixture.base+"/cat/demo.js")
	second := importRuntimeSource(t, engine, "drpy", fixture.base+"/cat/demo.js")
	if first != second {
		t.Fatalf("重复导入生成了两个站点: %s / %s", first, second)
	}
	if sites := engine.downloader.drpy.list(); len(sites) != 1 {
		t.Fatalf("重复导入后站点数应为 1，实际 %d", len(sites))
	}
	page, err := engine.nativeCatalog(context.Background(), nativeInput{Source: first, Page: 1, Force: true})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("重复导入后浏览失败: err=%v", err)
	}
}

// TestCatVodMissingLibReportsClearError 运行时库文件被清掉时，要提示重新
// 导入，而不是抛 JS 层的 _ is not defined。
func TestCatVodMissingLibReportsClearError(t *testing.T) {
	fixture := newRuntimeFixture(t)
	directory := t.TempDir()
	engine := newRuntimeEngine(t, directory, fixture.base)
	t.Cleanup(engine.downloads.close)
	source := importRuntimeSource(t, engine, "drpy", fixture.base+"/cat/demo.js")

	site, ok := engine.downloader.drpy.get(source)
	if !ok || site.CatLib == "" {
		t.Fatalf("站点缺少 catLib: %+v", site)
	}
	if err := os.Remove(filepath.Join(directory, filepath.FromSlash(site.CatLib))); err != nil {
		t.Fatal(err)
	}
	_, err := engine.nativeCatalog(context.Background(), nativeInput{Source: source, Page: 1, Force: true})
	if err == nil || !strings.Contains(err.Error(), "重新导入") {
		t.Fatalf("缺少运行时库时应提示重新导入，实际: %v", err)
	}
}

// catVodJSONPlaySpider 分集地址是不透明 ID，播放地址只能由 play 返回的
// JSON 字符串给出（CatVod 的 play 返回 JSON 字符串，不是对象）。
func catVodJSONPlaySpider(base string) string {
	return fmt.Sprintf(`import { _ } from './lib/cat.js';
let key = 'JSON播放';
let HOST = '%[1]s';
async function init(cfg) {}
async function home(filter) { return JSON.stringify({ class: [{ type_id: '1', type_name: '电影' }] }); }
async function homeVod() {
	return JSON.stringify({ page: 1, pagecount: 1, limit: 20, total: 1, list: [
		{ vod_id: 'v1', vod_name: 'JSON播放剧', vod_remarks: '全1集' }] });
}
async function category(tid, pg, filter, extend) { return homeVod(); }
async function detail(id) {
	return JSON.stringify({ list: [{ vod_id: id, vod_name: 'JSON播放剧',
		vod_play_from: '源', vod_play_url: '第01集$opaque-token-1' }] });
}
async function play(flag, id, flags) {
	return JSON.stringify({ parse: 0, jx: 0, url: '%[1]s/media/a.mp4', header: { Referer: '%[1]s/' } });
}
export function __jsEvalReturn() {
	return { init: init, home: home, homeVod: homeVod, category: category, detail: detail, play: play };
}
`, base)
}

// TestCatVodPlayJSONString 回归：play 返回 JSON 字符串时必须解析出 url，
// 不能整段当成播放地址（Bookan 等有声书源就是这样，章节地址是 .m4a，
// 走不到「章节自带直链」的兜底）。
func TestCatVodPlayJSONString(t *testing.T) {
	fixture := newRuntimeFixture(t)
	engine := newRuntimeEngine(t, t.TempDir(), fixture.base)
	t.Cleanup(engine.downloads.close)
	source := importRuntimeSource(t, engine, "drpy", fixture.base+"/cat/jsonplay.js")

	page, err := engine.nativeCatalog(context.Background(), nativeInput{Source: source, Page: 1, Force: true})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("首页失败: err=%v", err)
	}
	payload, err := engine.nativeDetail(context.Background(), page.Items[0])
	if err != nil {
		t.Fatalf("详情失败: %v", err)
	}
	chapters, _ := payload.(map[string]any)["chapters"].([]Chapter)
	if len(chapters) != 1 {
		t.Fatalf("分集异常: %+v", chapters)
	}
	if chapters[0].VideoURL != "" {
		t.Fatalf("opaque 分集不应带直链: %q", chapters[0].VideoURL)
	}
	media, err := engine.downloader.resolveProviderMedia(context.Background(), Task{
		DramaID: page.Items[0].ID, Chapter: chapters[0],
	})
	if err != nil {
		t.Fatalf("play 返回 JSON 字符串时取流失败: %v", err)
	}
	if !strings.HasSuffix(media.URL, "/media/a.mp4") {
		t.Fatalf("播放地址错误: %q", media.URL)
	}
	if media.Referer != fixture.base+"/" {
		t.Fatalf("play 的 header.Referer 未生效: %q", media.Referer)
	}
}

// TestAudioMediaExtensions .m4a / .mp3 这类音频地址也应视为可播放。
func TestAudioMediaExtensions(t *testing.T) {
	for _, address := range []string{
		"https://cdn.test/a.m4a",
		"https://cdn.test/a.mp3?token=1",
		"https://cdn.test/b.m3u8",
		"https://cdn.test/c.mp4",
	} {
		if !drpyIsDirectMediaURL(address) {
			t.Fatalf("应视为可播放: %s", address)
		}
	}
	if drpyIsDirectMediaURL("https://cdn.test/watch/1") {
		t.Fatal("页面地址不应视为直链")
	}
}

// catVodPostSpider CatVod 常见的 POST JSON 写法：data + postType，而不是 body。
func catVodPostSpider(base string) string {
	return fmt.Sprintf(`import { _ } from './lib/cat.js';
let key = 'POST蜘蛛';
let HOST = '%[1]s';
async function init(cfg) {}
async function home(filter) { return JSON.stringify({ class: [{ type_id: '1', type_name: '电影' }] }); }
async function homeVod() {
	let res = await req(HOST + '/cat/post', { method: 'post', data: { subcateId: 56, p: 1 }, postType: 'json' });
	let jo = JSON.parse(res.content).result;
	let videos = (jo.items || []).map(function (it) {
		return { vod_id: it.url, vod_name: it.title, vod_pic: it.image, vod_remarks: '👀' + it.viewcount };
	});
	return JSON.stringify({ list: videos });
}
async function category(tid, pg, filter, extend) { return homeVod(); }
async function detail(id) {
	return JSON.stringify({ list: [{ vod_id: id, vod_name: 'POST剧', vod_play_from: '源',
		vod_play_url: '第01集$%[1]s/media/a.mp4' }] });
}
async function play(flag, id, flags) { return JSON.stringify({ parse: 0, url: id }); }
export function __jsEvalReturn() {
	return { init: init, home: home, homeVod: homeVod, category: category, detail: detail, play: play };
}
`, base)
}

// TestCatVodPostDataBody CatVod 的 data + postType:'json' 必须真的发出去
// （贝乐虎就是靠这个 POST 拿首页；之前发空体导致 items undefined）。
func TestCatVodPostDataBody(t *testing.T) {
	fixture := newRuntimeFixture(t)
	engine := newRuntimeEngine(t, t.TempDir(), fixture.base)
	t.Cleanup(engine.downloads.close)
	source := importRuntimeSource(t, engine, "drpy", fixture.base+"/cat/post.js")

	page, err := engine.nativeCatalog(context.Background(), nativeInput{Source: source, Page: 1, Force: true})
	if err != nil {
		t.Fatalf("POST 首页失败: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Title != "POST剧" {
		t.Fatalf("POST 请求体没有正确发送: %+v", page.Items)
	}
}
