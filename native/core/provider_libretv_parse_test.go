package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- parses 解析与持久化 ----

func TestLibreTVParseRulesExtraction(t *testing.T) {
	body := `{"sites":[{"key":"a","name":"站点A","type":1,"api":"https://api.a.com/provide/vod"}],
	"parses":[
	 {"name":"JSON 解析","type":1,"url":"https://parse.a.com/api","ext":{"flag":["qq"]}},
	 {"name":"本地脚本","type":2,"url":"https://parse.a.com/drpy_parse.js"},
	 {"name":"聚合","type":2,"url":"https://parse.a.com/agg"},
	 {"name":"Web","type":0,"url":"Web"},
	 {"name":"重复","type":1,"url":"https://parse.a.com/api"},
	 {"type":2,"url":"https://parse.b.com/parse.js"}
	]}`
	sites, _, parses, skipped, err := parseLibreTVSubscription(body, "")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(sites) != 1 || len(skipped) != 0 {
		t.Fatalf("站点/跳过结果错误: %d %v", len(sites), skipped)
	}
	// Web 内置嗅探（非 http 地址）静默跳过；重复 URL 去重。
	if len(parses) != 4 {
		t.Fatalf("应提取 4 条解析规则，实际 %d：%+v", len(parses), parses)
	}
	if parses[0].Name != "JSON 解析" || parses[0].Type != 1 || !strings.Contains(string(parses[0].Ext), "qq") {
		t.Fatalf("JSON 解析条目错误: %+v", parses[0])
	}
	if libretvParseKind(parses[0]) != "JSON 接口" {
		t.Fatalf("类型名错误: %s", libretvParseKind(parses[0]))
	}
	// type=2 + .js = 本地脚本扩展；type=2 + 非 .js 属于未知类型。
	if !libretvParseIsScript(parses[1]) || libretvParseKind(parses[1]) != "本地脚本" {
		t.Fatalf("脚本条目识别错误: %+v", parses[1])
	}
	if libretvParseIsScript(parses[2]) || libretvParseKind(parses[2]) != "其他解析" {
		t.Fatalf("非 .js 的 type=2 不应识别为脚本: %+v", parses[2])
	}
	// type 0 / 4 是 WebView 嗅探类。
	if libretvParseKind(libretvParse{Type: 0, URL: "https://a.com/x"}) != "WebView 嗅探" ||
		!libretvParseNeedsWebView(libretvParse{Type: 0}) {
		t.Fatal("type=0 应识别为 WebView 嗅探")
	}
	if libretvParseKind(libretvParse{Type: 4, URL: "https://a.com/x"}) != "WebView 嗅探" ||
		!libretvParseNeedsWebView(libretvParse{Type: 4}) {
		t.Fatal("type=4 应识别为 WebView 嗅探")
	}
	// 缺名称时用主机名。
	if parses[3].Name != "parse.b.com" {
		t.Fatalf("缺省名称错误: %q", parses[3].Name)
	}
}

func TestLibreTVParseSelectPersistence(t *testing.T) {
	directory := t.TempDir()
	store := loadLibreTVStore(directory)
	parses := []libretvParse{
		{Name: "脚本", Type: 2, URL: "https://parse.a.com/p.js"},
		{Name: "接口", Type: 1, URL: "https://parse.a.com/api"},
	}
	if err := store.putParses(parses); err != nil {
		t.Fatalf("保存解析规则失败: %v", err)
	}
	if err := store.setSelectedParse("https://parse.a.com/p.js"); err != nil {
		t.Fatalf("选择解析失败: %v", err)
	}
	reloaded := loadLibreTVStore(directory)
	selected, ok := reloaded.selectedParse()
	if !ok || selected.URL != "https://parse.a.com/p.js" {
		t.Fatalf("选择未持久化: %+v %v", selected, ok)
	}
	// 不存在的解析必须拒绝，避免 UI 与存储漂移。
	if err := reloaded.setSelectedParse("https://parse.a.com/missing"); err == nil {
		t.Fatal("选择未导入的解析应当报错")
	}
	// 空串 = 直链/嗅探。
	if err := reloaded.setSelectedParse(""); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadLibreTVStore(directory).selectedParse(); ok {
		t.Fatal("清空选择后不应再返回解析")
	}
	// 选择对应的解析被移除后，加载时自动回到直链/嗅探。
	if err := reloaded.setSelectedParse("https://parse.a.com/api"); err != nil {
		t.Fatal(err)
	}
	// 模拟订阅更新后该解析不再存在（parseUrl 仍指向旧地址）。
	body := `{"sites":[],"parses":[{"name":"其他","type":1,"url":"https://other.com/api"}],"parseUrl":"https://parse.a.com/api"}`
	if err := os.WriteFile(filepath.Join(directory, "libretv.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadLibreTVStore(directory).selectedParse(); ok {
		t.Fatal("解析被移除后应自动回落到直链/嗅探")
	}
}

// ---- 播放解析：直链 / JSON 接口 / 本地脚本 / 嗅探 ----

// putLibreTVTestSite 注册测试站点：resolveProviderMedia 只在站点已导入时
// 才走 LibreTV 分支。
func putLibreTVTestSite(t *testing.T, downloader *Downloader, fixture *libretvFixture) {
	t.Helper()
	if _, err := downloader.libretv.put(libretvSite{Key: "k", Name: "N", API: fixture.server.URL + "/provide/vod"}); err != nil {
		t.Fatal(err)
	}
}

func libretvParseTestDownloader(t *testing.T, fixture *libretvFixture) *Downloader {
	t.Helper()
	downloader := libretvTestDownloader(t, fixture)
	downloader.drpy = loadDrpyStore(downloader.cfg.dataDir)
	return downloader
}

func TestLibreTVResolveDirectMediaSkipsParse(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		if strings.HasSuffix(path, ".js") {
			t.Fatalf("直链不应触发解析脚本: %s", path)
		}
		return 200, ""
	})
	downloader := libretvParseTestDownloader(t, fixture)
	putLibreTVTestSite(t, downloader, fixture)
	if err := downloader.libretv.putParses([]libretvParse{{Name: "脚本", Type: 2, URL: fixture.server.URL + "/p.js"}}); err != nil {
		t.Fatal(err)
	}
	if err := downloader.libretv.setSelectedParse(fixture.server.URL + "/p.js"); err != nil {
		t.Fatal(err)
	}
	task := Task{
		DramaID: "libretv:k:1",
		Chapter: Chapter{ID: "libretv:k:1:0-1", Source: "libretv:k",
			VideoURL: "https://cdn.example.com/direct.mp4", Referer: fixture.server.URL + "/"},
	}
	media, err := downloader.resolveProviderMedia(context.Background(), task)
	if err != nil {
		t.Fatalf("直链解析失败: %v", err)
	}
	if media.URL != "https://cdn.example.com/direct.mp4" {
		t.Fatalf("直链被改写: %q", media.URL)
	}
}

func TestLibreTVResolveJSONParse(t *testing.T) {
	var requested string
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		if path != "/parse" {
			t.Errorf("意外的请求: %s", path)
		}
		requested = query["url"]
		return 200, `{"code":200,"url":"https://cdn.example.com/from-api.mp4","header":{"Referer":"https://cdn.example.com/"}}`
	})
	downloader := libretvParseTestDownloader(t, fixture)
	putLibreTVTestSite(t, downloader, fixture)
	parseURL := fixture.server.URL + "/parse"
	if err := downloader.libretv.putParses([]libretvParse{{Name: "接口", Type: 1, URL: parseURL}}); err != nil {
		t.Fatal(err)
	}
	if err := downloader.libretv.setSelectedParse(parseURL); err != nil {
		t.Fatal(err)
	}
	task := Task{
		DramaID: "libretv:k:1",
		Chapter: Chapter{ID: "libretv:k:1:0-1", Source: "libretv:k", VideoURL: "https://page.example.com/watch/1"},
	}
	media, err := downloader.resolveProviderMedia(context.Background(), task)
	if err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	if media.URL != "https://cdn.example.com/from-api.mp4" {
		t.Fatalf("播放地址错误: %q", media.URL)
	}
	if media.Referer != "https://cdn.example.com/" {
		t.Fatalf("header Referer 未生效: %q", media.Referer)
	}
	if requested != "https://page.example.com/watch/1" {
		t.Fatalf("解析接口未收到原始播放页: %q", requested)
	}
}

func TestLibreTVResolveScriptParse(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		if path == "/p.js" {
			return 200, `function parse(url){ return {url: 'https://cdn.example.com/from-script.mp4', jx: 0, header: {Referer: 'https://cdn.example.com/'}}; }`
		}
		t.Errorf("意外的请求: %s", path)
		return 404, ""
	})
	downloader := libretvParseTestDownloader(t, fixture)
	putLibreTVTestSite(t, downloader, fixture)
	parseURL := fixture.server.URL + "/p.js"
	if err := downloader.libretv.putParses([]libretvParse{{Name: "脚本", Type: 2, URL: parseURL}}); err != nil {
		t.Fatal(err)
	}
	if err := downloader.libretv.setSelectedParse(parseURL); err != nil {
		t.Fatal(err)
	}
	task := Task{
		DramaID: "libretv:k:1",
		Chapter: Chapter{ID: "libretv:k:1:0-1", Source: "libretv:k", VideoURL: "https://page.example.com/watch/1"},
	}
	media, err := downloader.resolveProviderMedia(context.Background(), task)
	if err != nil {
		t.Fatalf("脚本解析失败: %v", err)
	}
	if media.URL != "https://cdn.example.com/from-script.mp4" {
		t.Fatalf("播放地址错误: %q", media.URL)
	}
	if media.Referer != "https://cdn.example.com/" {
		t.Fatalf("脚本 header Referer 未生效: %q", media.Referer)
	}
	// 脚本已缓存：再次解析不应重复下载。
	before := len(fixture.requests)
	if _, err := downloader.resolveProviderMedia(context.Background(), task); err != nil {
		t.Fatalf("二次解析失败: %v", err)
	}
	for _, request := range fixture.requests[before:] {
		if strings.Contains(request, "/p.js") {
			t.Fatalf("脚本被重复下载: %s", request)
		}
	}
}

func TestLibreTVResolveScriptParseJXSniff(t *testing.T) {
	var pageBody string
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		switch path {
		case "/p.js":
			// jx=1：返回的是解析页，需要嗅探。
			return 200, `function parse(url){ return {url: '/page', jx: 1}; }`
		case "/page":
			return 200, pageBody
		}
		return 404, ""
	})
	pageBody = `<html><script>var config = {"url":"` + fixture.server.URL + `/sniffed.mp4"};</script></html>`
	downloader := libretvParseTestDownloader(t, fixture)
	putLibreTVTestSite(t, downloader, fixture)
	parseURL := fixture.server.URL + "/p.js"
	if err := downloader.libretv.putParses([]libretvParse{{Name: "脚本", Type: 2, URL: parseURL}}); err != nil {
		t.Fatal(err)
	}
	if err := downloader.libretv.setSelectedParse(parseURL); err != nil {
		t.Fatal(err)
	}
	task := Task{
		DramaID: "libretv:k:1",
		Chapter: Chapter{ID: "libretv:k:1:0-1", Source: "libretv:k", VideoURL: "https://page.example.com/watch/1"},
	}
	media, err := downloader.resolveProviderMedia(context.Background(), task)
	if err != nil {
		t.Fatalf("jx 嗅探失败: %v", err)
	}
	if !strings.HasSuffix(media.URL, "/sniffed.mp4") {
		t.Fatalf("嗅探结果错误: %q", media.URL)
	}
}

func TestLibreTVResolveDefaultSniff(t *testing.T) {
	var pageBody string
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		if path == "/watch/1" {
			return 200, pageBody
		}
		return 404, ""
	})
	pageBody = `<html><script>var player = {"video":"` + fixture.server.URL + `/direct-sniffed.mp4"};</script></html>`
	downloader := libretvParseTestDownloader(t, fixture)
	putLibreTVTestSite(t, downloader, fixture)
	task := Task{
		DramaID: "libretv:k:1",
		Chapter: Chapter{ID: "libretv:k:1:0-1", Source: "libretv:k", VideoURL: fixture.server.URL + "/watch/1"},
	}
	media, err := downloader.resolveProviderMedia(context.Background(), task)
	if err != nil {
		t.Fatalf("默认嗅探失败: %v", err)
	}
	if !strings.HasSuffix(media.URL, "/direct-sniffed.mp4") {
		t.Fatalf("默认嗅探结果错误: %q", media.URL)
	}
}

// TestLibreTVParseType4AndAppYsV2 type=4（T4/CatVod/drpy-node 服务器）与
// csp_AppYsV2（苹果CMS 二开 /v1.vod）响应结构都与 MacCMS 同构，可直接
// 按 JSON 采集站导入；只有首页地址的 csp_ 条目不能当接口。
func TestLibreTVParseType4AndAppYsV2(t *testing.T) {
	body := `{"sites":[
	 {"key":"t4","name":"T4 服务器","type":4,"api":"https://t4.example.com/vod"},
	 {"key":"appys","name":"苹果二开","type":3,"api":"csp_AppYsV2",
	  "ext":"http://www.fuju2024.cc:8013/ruifenglb_api.php/v1.vod"},
	 {"key":"vod1","name":"vod 路径","type":3,"api":"csp_AppYsV2","ext":"https://vod.example.com/api"},
	 {"key":"home","name":"只有首页","type":3,"api":"csp_Jpys","ext":"https://www.czzyvideo.com/"}
	]}`
	sites, candidates, _, skipped, err := parseLibreTVSubscription(body, "")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(sites) != 3 {
		t.Fatalf("应导入 3 个采集站点，实际 %d：%+v", len(sites), sites)
	}
	for _, site := range sites {
		if site.Type != 1 {
			t.Fatalf("T4 / AppYsV2 应按 JSON 采集站导入: %+v", site)
		}
	}
	if sites[0].API != "https://t4.example.com/vod" {
		t.Fatalf("type=4 应直接用 api: %+v", sites[0])
	}
	if sites[1].API != "http://www.fuju2024.cc:8013/ruifenglb_api.php/v1.vod" {
		t.Fatalf("csp_AppYsV2 应取 ext 的接口地址: %+v", sites[1])
	}
	if len(candidates) != 0 {
		t.Fatalf("这些条目不应产生 drpy 候选: %+v", candidates)
	}
	// 只有首页地址的 csp_ 条目跳过，并点名是哪个 Java Spider。
	if len(skipped) != 1 || !strings.Contains(skipped[0], "csp_Jpys") {
		t.Fatalf("首页条目应跳过并点名 Spider: %+v", skipped)
	}
}

func TestLibreTVParseDrpyCandidatesPreferExt(t *testing.T) {
	// TVBox 的 drpy 站点：api 是运行时库（ESM，无法编译），ext 才是站点脚本。
	body := `{"sites":[
	 {"key":"jp","name":"荐片P2P1","type":3,
	  "api":"https://cdn.example.com/drpy2.min.js",
	  "ext":"https://cdn.example.com/jianpian.js"},
	 {"key":"node","name":"drpy-node 站点","type":3,
	  "api":"https://cdn.example.com/site-script.js"},
	 {"key":"csp","name":"csp 规则","type":3,
	  "api":"csp_XYQ","ext":{"url":"https://cdn.example.com/rule.json"}},
	 {"key":"json","name":"JSON 扩展","type":3,
	  "api":"https://cdn.example.com/drpy.min.js",
	  "ext":"https://cdn.example.com/ext.json"}
	]}`
	_, candidates, _, skipped, err := parseLibreTVSubscription(body, "")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("应提取 2 个 drpy 候选，实际 %d：%+v", len(candidates), candidates)
	}
	if candidates[0].Name != "荐片P2P1" || candidates[0].API != "https://cdn.example.com/jianpian.js" {
		t.Fatalf("应优先使用 ext 里的站点脚本: %+v", candidates[0])
	}
	if candidates[1].API != "https://cdn.example.com/site-script.js" {
		t.Fatalf("无 ext 时应回退 api: %+v", candidates[1])
	}
	// 非脚本 ext 的条目回退到 api；api 也不是 .js（或只是运行时库）时带原因跳过。
	if len(skipped) != 2 {
		t.Fatalf("应跳过 2 个不支持条目: %+v", skipped)
	}
	if !strings.Contains(skipped[0], "csp_") {
		t.Fatalf("csp_ 条目原因不明: %s", skipped[0])
	}
	if !strings.Contains(skipped[1], "运行时库") {
		t.Fatalf("仅提供运行时库的条目原因不明: %s", skipped[1])
	}
}

// TestLibreTVUnsupportedReasonByShape type=3 里混着多种形态，跳过原因要
// 能区分 csp_ 规则、jar 包、只有运行时库等具体情况。
func TestLibreTVUnsupportedReasonByShape(t *testing.T) {
	cases := []struct {
		entry  libretvConfigSite
		expect string
	}{
		{libretvConfigSite{API: "csp_XBPQ"}, "csp_"},
		{libretvConfigSite{API: "csp_XYQ", Jar: "https://a.com/spider.jar"}, "jar"},
		{libretvConfigSite{API: "https://a.com/drpy.min.js"}, "运行时库"},
		{libretvConfigSite{API: "https://a.com/site.js", Ext: json.RawMessage(`"https://a.com/rule.json"`)}, "ext 不是 drpy 脚本"},
		{libretvConfigSite{API: "https://a.com/vod"}, "type="},
	}
	for _, item := range cases {
		if reason := libretvUnsupportedReason(item.entry); !strings.Contains(reason, item.expect) {
			t.Fatalf("原因说明不符 %q（%s）", item.expect, reason)
		}
	}
}

func TestDrpyRejectsESModuleScript(t *testing.T) {
	script := `import cheerio from "./cheerio.min.js";
var rule = {title: '荐片', host: 'http://api.example.com'};
`
	err := validateDrpyScript(script)
	if err == nil || !strings.Contains(err.Error(), "ES Module") {
		t.Fatalf("ES Module 应给出明确提示: %v", err)
	}
	// 普通 drpy 脚本不受影响。
	ok := `var rule = {title: '普通', host: 'http://api.example.com'};`
	if err := validateDrpyScript(ok); err != nil {
		t.Fatalf("普通脚本不应被拒绝: %v", err)
	}
	// 字符串里出现 import 关键字（非行首）不算 ESM。
	inline := `var rule = {title: '内联', host: 'http://api.example.com', note: 'use import carefully'};`
	if err := validateDrpyScript(inline); err != nil {
		t.Fatalf("行内 import 字面量不应误判: %v", err)
	}
}

func TestLibreTVResolveWebViewParseNeedsDart(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		return 404, ""
	})
	downloader := libretvParseTestDownloader(t, fixture)
	putLibreTVTestSite(t, downloader, fixture)
	// type=0 / 4：Go 不实现浏览器，必须由 Dart 侧 WebView 嗅探后替换直链。
	for _, parseType := range []int{0, 4} {
		parseURL := fmt.Sprintf("%s/parse-%d", fixture.server.URL, parseType)
		if err := downloader.libretv.putParses([]libretvParse{{Name: "Web", Type: parseType, URL: parseURL}}); err != nil {
			t.Fatal(err)
		}
		if err := downloader.libretv.setSelectedParse(parseURL); err != nil {
			t.Fatal(err)
		}
		task := Task{
			DramaID: "libretv:k:1",
			Chapter: Chapter{ID: "libretv:k:1:0-1", Source: "libretv:k", VideoURL: "https://page.example.com/watch/1"},
		}
		_, err := downloader.resolveProviderMedia(context.Background(), task)
		if err == nil || !strings.Contains(err.Error(), "WebView") {
			t.Fatalf("type=%d 应提示需要 WebView: %v", parseType, err)
		}
	}
}

func TestLibreTVResolveUnsupportedParseKind(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		return 404, ""
	})
	downloader := libretvParseTestDownloader(t, fixture)
	putLibreTVTestSite(t, downloader, fixture)
	// type=2 但 url 不是 .js（标准 TVBox 聚合解析）：明确报「暂不支持」。
	parseURL := fixture.server.URL + "/agg"
	if err := downloader.libretv.putParses([]libretvParse{{Name: "聚合", Type: 2, URL: parseURL}}); err != nil {
		t.Fatal(err)
	}
	if err := downloader.libretv.setSelectedParse(parseURL); err != nil {
		t.Fatal(err)
	}
	task := Task{
		DramaID: "libretv:k:1",
		Chapter: Chapter{ID: "libretv:k:1:0-1", Source: "libretv:k", VideoURL: "https://page.example.com/watch/1"},
	}
	_, err := downloader.resolveProviderMedia(context.Background(), task)
	if err == nil || !strings.Contains(err.Error(), "暂不支持") {
		t.Fatalf("不支持的解析类型应明确报错: %v", err)
	}
}

func TestLibreTVResolveJSONParseMessage(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		return 200, `{"code":403,"msg":"解析失败：需要会员"}`
	})
	downloader := libretvParseTestDownloader(t, fixture)
	putLibreTVTestSite(t, downloader, fixture)
	parseURL := fixture.server.URL + "/parse"
	if err := downloader.libretv.putParses([]libretvParse{{Name: "接口", Type: 1, URL: parseURL}}); err != nil {
		t.Fatal(err)
	}
	if err := downloader.libretv.setSelectedParse(parseURL); err != nil {
		t.Fatal(err)
	}
	task := Task{
		DramaID: "libretv:k:1",
		Chapter: Chapter{ID: "libretv:k:1:0-1", Source: "libretv:k", VideoURL: "https://page.example.com/watch/1"},
	}
	_, err := downloader.resolveProviderMedia(context.Background(), task)
	if err == nil || !strings.Contains(err.Error(), "需要会员") {
		t.Fatalf("接口错误信息未透出: %v", err)
	}
}

func TestLibreTVParseJSONResponseShapes(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`https://cdn.test/a.mp4`, "https://cdn.test/a.mp4"},
		{`{"url":"https://cdn.test/b.mp4"}`, "https://cdn.test/b.mp4"},
		{`{"playUrl":"https://cdn.test/c.mp4"}`, "https://cdn.test/c.mp4"},
		{`{"data":{"url":"https://cdn.test/d.mp4"}}`, "https://cdn.test/d.mp4"},
	}
	for _, item := range cases {
		got, _, err := libretvParseJSONResponse(item.body)
		if err != nil || got != item.want {
			t.Fatalf("响应解析错误 %s → %q, %v", item.body, got, err)
		}
	}
	if _, _, err := libretvParseJSONResponse(`{"code":500,"msg":"坏了"}`); err == nil || !strings.Contains(err.Error(), "坏了") {
		t.Fatalf("错误消息未透出: %v", err)
	}
}
