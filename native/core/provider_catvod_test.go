package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

const catVodDemoLib = `
var internalLoad = function (html) {
	return function (selector) {
		return { text: function () { return '演示剧'; }, attr: function () { return '/v1'; } };
	};
};
var lodash = {
	map: function (list, fn) {
		var out = [];
		for (var i = 0; i < list.length; i++) out.push(fn(list[i]));
		return out;
	}
};
// 打包产物加载时会探测 Node 全局：垫片缺失时这一行就会抛错。
var probe = typeof Buffer !== 'undefined' && Buffer.isBuffer(new Uint8Array(1));
var internalUri = lodash.map(['a'], function (item) { return item; });
export{internalLoad as load, lodash as _, internalUri as Uri};
`

const catVodDemoSpider = `import { _, load } from './lib/cat.js';
let key = '演示蜘蛛';
let HOST = 'https://demo.test';
async function init(cfg) {}
async function home(filter) {
	return JSON.stringify({ class: [{ type_id: '1', type_name: '电影' }] });
}
async function homeVod() {
	var $ = load('<div class="it"><a href="/v1">演示剧</a></div>');
	var items = _.map([0], function () {
		return {
			vod_id: 'v1',
			vod_name: $('.it a').text(),
			vod_pic: 'https://img.test/a.jpg',
			vod_remarks: '已完结'
		};
	});
	return JSON.stringify({ page: 1, pagecount: 1, limit: 20, total: 1, list: items });
}
async function category(tid, pg, filter, extend) { return homeVod(); }
async function detail(id) {
	return JSON.stringify({
		list: [{
			vod_id: id,
			vod_name: '演示剧',
			vod_content: '<p>简介</p>',
			vod_play_from: '演示源',
			vod_play_url: '第01集$https://cdn.test/a.mp4'
		}]
	});
}
async function play(flag, id, flags) {
	return JSON.stringify({ parse: 0, url: id, header: { Referer: 'https://demo.test/' } });
}
async function search(wd, quick) { return homeVod(); }
export function __jsEvalReturn() {
	return { init: init, home: home, homeVod: homeVod, category: category, detail: detail, play: play, search: search };
}
`

func TestCatVodScriptDetectionAndTransform(t *testing.T) {
	if !looksLikeCatVodScript(catVodDemoSpider) {
		t.Fatal("应识别为 CatVod 蜘蛛（含 __jsEvalReturn）")
	}
	if looksLikeCatVodScript(`var rule = {title: 'x'};`) {
		t.Fatal("普通 drpy 脚本不应识别为 CatVod")
	}
	if path := catVodLibPath(catVodDemoSpider); path != "./lib/cat.js" {
		t.Fatalf("lib 路径提取错误: %q", path)
	}
	if address := catVodResolveURL("https://cdn.test/cat/demo.js", "./lib/cat.js"); address != "https://cdn.test/cat/lib/cat.js" {
		t.Fatalf("相对解析错误: %q", address)
	}
	if address := catVodResolveURL("https://cdn.test/cat/sub/demo.js", "../lib/cat.js"); address != "https://cdn.test/cat/lib/cat.js" {
		t.Fatalf("上级相对解析错误: %q", address)
	}
	body, bindings := catVodTransformSpider(catVodDemoSpider)
	if strings.Contains(body, "import ") {
		t.Fatal("import 未剥离")
	}
	if strings.Contains(body, "export function") {
		t.Fatalf("export 关键字未处理: %s", body)
	}
	if !strings.Contains(body, "function __jsEvalReturn()") {
		t.Fatal("__jsEvalReturn 入口丢失")
	}
	if strings.Join(bindings, ",") != "_,load" {
		t.Fatalf("import 绑定名错误: %v", bindings)
	}
	lib, err := catVodTransformLib(catVodDemoLib)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lib, "globalThis.__cat = {") {
		t.Fatalf("库未注入全局: %s", lib[len(lib)-200:])
	}
	if name, host := catVodExtractMeta(catVodDemoSpider); name != "演示蜘蛛" || host != "https://demo.test" {
		t.Fatalf("元信息提取错误: %q %q", name, host)
	}
}

// TestCatVodInjectBindingsWithoutRedeclaration 回归：某些 lib/cat.js 变体在
// 顶层用 let / const 声明导出名（export{_ as _}），注入时再写 var 会抛
// 「Identifier '_' has already been declared」。注入必须是赋值式的。
func TestCatVodInjectBindingsWithoutRedeclaration(t *testing.T) {
	for _, declaration := range []string{"let", "const", "var"} {
		lib := declaration + ` _ = function (v) { return 'lib:' + v; };
` + declaration + ` load = function (html) { return html; };
export{_ as _, load as load};
`
		transformed, err := catVodTransformLib(lib)
		if err != nil {
			t.Fatalf("%s: %v", declaration, err)
		}
		program, err := goja.Compile("cat-lib.js", transformed, false)
		if err != nil {
			t.Fatalf("%s: %v", declaration, drpyCompileError(err))
		}
		vm := goja.New()
		host := &drpyHost{ctx: context.Background(), vm: vm, items: map[string]string{}}
		installDrpyHost(vm, host)
		if err := catVodInstallShims(vm); err != nil {
			t.Fatalf("%s: %v", declaration, err)
		}
		if _, err := vm.RunProgram(program); err != nil {
			t.Fatalf("%s: %v", declaration, drpyWrapError(err))
		}
		if err := catVodInjectBindings(vm, []string{"_", "load"}); err != nil {
			t.Fatalf("%s 声明时注入失败: %v", declaration, drpyWrapError(err))
		}
		value, err := vm.RunString(`_('x')`)
		if err != nil {
			t.Fatalf("%s: %v", declaration, drpyWrapError(err))
		}
		if value.Export() != "lib:x" {
			t.Fatalf("%s 声明时绑定值错误: %v", declaration, value.Export())
		}
	}
}

// TestCatVodUndeclaredBindings 库没有同名顶层绑定时，注入要落地成全局，
// 否则蜘蛛里的 `_()` 会 ReferenceError。
func TestCatVodUndeclaredBindings(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(`globalThis.__cat = { _: function (v) { return 'ok:' + v; } };`); err != nil {
		t.Fatal(err)
	}
	if err := catVodInjectBindings(vm, []string{"_", "1bad-name"}); err != nil {
		t.Fatalf("注入失败: %v", drpyWrapError(err))
	}
	value, err := vm.RunString(`_('y')`)
	if err != nil {
		t.Fatalf("未落地为全局: %v", drpyWrapError(err))
	}
	if value.Export() != "ok:y" {
		t.Fatalf("绑定值错误: %v", value.Export())
	}
}

func TestCatVodImportBrowseAndPlay(t *testing.T) {
	fixture := newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		switch path {
		case "/cat/demo.js":
			return 200, catVodDemoSpider
		case "/cat/lib/cat.js":
			return 200, catVodDemoLib
		}
		return 404, ""
	})
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine.downloader.client.Transport = forwardToFixture(fixture)
	spiderURL := fixture.server.URL + "/cat/demo.js"

	result, err := engine.importDrpyScript(context.Background(), spiderURL, "")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if payload := result.(map[string]any); payload["engine"] != catVodEngine {
		t.Fatalf("引擎标记错误: %+v", payload)
	}
	site, ok := engine.downloader.drpy.get("drpy:" + drpyKeyFromScript(catVodDemoSpider))
	if !ok {
		t.Fatal("脚本源未入库")
	}
	if !drpySiteIsCatVod(site) || site.CatLib == "" || site.LibURL != fixture.server.URL+"/cat/lib/cat.js" {
		t.Fatalf("站点字段错误: %+v", site)
	}
	if site.Name != "演示蜘蛛" {
		t.Fatalf("站点名称错误: %q", site.Name)
	}

	ctx := context.Background()
	categories, err := engine.downloader.fetchDrpyCategories(ctx, site)
	if err != nil {
		t.Fatalf("分类失败: %v", err)
	}
	if len(categories) != 1 || categories[0].Name != "电影" {
		t.Fatalf("分类解析错误: %+v", categories)
	}
	dramas, hasMore, err := engine.downloader.fetchDrpyCatalogPage(ctx, site, 1, "", "")
	if err != nil {
		t.Fatalf("首页失败: %v", err)
	}
	if len(dramas) != 1 || dramas[0].Title != "演示剧" {
		t.Fatalf("首页解析错误: %+v", dramas)
	}
	if hasMore {
		t.Fatal("pagecount=1 时不该有下一页")
	}
	dramas, _, err = engine.downloader.fetchDrpyCatalogPage(ctx, site, 1, "1", "关键词")
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(dramas) != 1 {
		t.Fatalf("搜索结果错误: %+v", dramas)
	}

	vodID := dramas[0].SourceID
	drama, chapters, err := engine.downloader.fetchDrpyDetail(ctx, site, vodID)
	if err != nil {
		t.Fatalf("详情失败: %v", err)
	}
	if drama.Title != "演示剧" || len(chapters) != 1 {
		t.Fatalf("详情解析错误: %+v %+v", drama, chapters)
	}
	if !strings.Contains(chapters[0].VideoURL, "a.mp4") {
		t.Fatalf("分集地址错误: %+v", chapters[0])
	}

	// 播放：走 play(flag, id, flags)，返回 {parse:0, url}。
	media, err := engine.downloader.resolveProviderMedia(ctx, Task{
		DramaID: drama.ID,
		Chapter: chapters[0],
	})
	if err != nil {
		t.Fatalf("播放解析失败: %v", err)
	}
	if !strings.HasSuffix(media.URL, "a.mp4") {
		t.Fatalf("播放地址错误: %q", media.URL)
	}
	if media.Referer != "https://demo.test/" {
		t.Fatalf("play 的 header Referer 未生效: %q", media.Referer)
	}
}

// TestCatVodRealLibSample 用真实的 lib/cat.js（约 480KB 打包产物）回归：
// 转换 + 垫片 + 加载。样本文件不存在时跳过。
func TestCatVodRealLibSample(t *testing.T) {
	raw, err := os.ReadFile("/tmp/catlib2.js")
	if err != nil {
		t.Skip("没有真实样本，跳过")
	}
	transformed, err := catVodTransformLib(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	program, err := goja.Compile("cat-lib.js", transformed, false)
	if err != nil {
		t.Fatalf("转换后无法编译：%v", drpyCompileError(err))
	}
	vm := goja.New()
	host := &drpyHost{ctx: context.Background(), vm: vm, items: map[string]string{}}
	installDrpyHost(vm, host)
	if err := catVodInstallShims(vm); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := vm.RunProgram(program)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("真实 cat.js 执行失败：%v", drpyWrapError(err))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("真实 cat.js 加载超时")
	}
	if _, ok := vm.Get("__cat").Export().(map[string]any); !ok {
		t.Fatal("__cat 未导出")
	}
	value, err := vm.RunString(`__cat.load('<a class="t" href="/x">标题</a>')('.t').attr('href')`)
	if err != nil {
		t.Fatalf("cheerio 冒烟失败：%v", drpyWrapError(err))
	}
	if value.Export() != "/x" {
		t.Fatalf("cheerio 结果错误: %v", value.Export())
	}
}

// TestCatVodStoreRoundTripKeepsEngine 回归：站点记录落盘再读回时必须保留
// Engine / CatLib / Bindings，否则重启后 CatVod 站点退回 drpy 路径，
// 表现为「_ is not defined」（import 绑定没人注入、lib 也没加载）。
func TestCatVodStoreRoundTripKeepsEngine(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, drpyScriptsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, drpyScriptsDir, "catkey.js"), []byte("function home(){}\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	store := newDrpyStore(directory)
	if _, err := store.put(drpySite{
		Key:       "catkey",
		Name:      "视聚场",
		File:      drpyScriptsDir + "/catkey.js",
		Host:      "http://api.cntv.cn",
		Engine:    catVodEngine,
		CatLib:    "catlib/abc.js",
		Bindings:  "_",
		LibURL:    "https://cdn.test/cat/lib/cat.js",
		ScriptURL: "https://cdn.test/cat/cntv_open.js",
	}); err != nil {
		t.Fatal(err)
	}
	reloaded := loadDrpyStore(directory)
	site, ok := reloaded.get("drpy:catkey")
	if !ok {
		t.Fatal("站点未读回")
	}
	if !drpySiteIsCatVod(site) {
		t.Fatalf("重启后丢失 CatVod 标记: %+v", site)
	}
	if site.CatLib != "catlib/abc.js" || site.Bindings != "_" || site.LibURL != "https://cdn.test/cat/lib/cat.js" {
		t.Fatalf("重启后丢失 CatVod 字段: %+v", site)
	}
	if site.ScriptURL != "https://cdn.test/cat/cntv_open.js" || site.Host != "http://api.cntv.cn" {
		t.Fatalf("既有字段被破坏: %+v", site)
	}
}
