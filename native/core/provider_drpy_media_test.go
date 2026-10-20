package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// drpyMediaScript 播放测试脚本：detail 返回两个分集——第 01 集是媒体直链
// （VideoURL 兜底路径），第 02 集是解析页地址（jx/parse 路径）。
// __HOST__ / __PLAY_BODY__ 由测试注入。
const drpyMediaScript = `var rule = {
    title: '演示影视',
    host: '__HOST__',
    headers: {'User-Agent': MOBILE_UA},
    一级: function(){},
    二级: function(){},
    搜索: function(){},
    home: function(filter){ return {class: []}; },
    category: function(tid, pg, filter, extend){ return {list: [], page: 1, pagecount: 1, limit: 20, total: 0}; },
    detail: function(vodId){
        return {list: [{vod_id: vodId, vod_name: '演示剧甲',
            vod_play_from: 'ffm3u8',
            vod_play_url: '第01集$https://cdn.test/a1.m3u8#第02集$https://jx.test/page?ep=2'}]};
    },
__PLAY_BODY__
};
function init(ext){}
function home(filter){ return rule.home(filter); }
function category(tid, pg, filter, extend){ return rule.category(tid, pg, filter, extend); }
function detail(vodId){ return rule.detail(vodId); }
function search(wd, quick, pg){ return {list: []}; }
// 顶层包装：host 调的是全局函数；rule 上没实现时返回 undefined，
// 让 drpyCallPlay 回退到另一个入口（与真实 drpy 脚本的双层写法一致）。
function play(flag, episodeKey, flags){
    if (typeof rule.play !== 'function') { return undefined; }
    return rule.play(flag, episodeKey, flags);
}
function lazy(flag, episodeKey, flags){
    if (typeof rule.lazy !== 'function') { return undefined; }
    return rule.lazy(flag, episodeKey, flags);
}
`

// drpyMediaEngine 起 API+播放页 fixture，导入注入了 play/lazy 实现的脚本。
func drpyMediaEngine(t *testing.T, playBody string) (*nativeEngine, drpySite, Drama, []Chapter) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, ".m3u8"):
			_, _ = writer.Write([]byte("#EXTM3U\n#EXTINF:4,\nsegment.ts\n#EXT-X-ENDLIST"))
		case request.URL.Path == "/page":
			// 解析页：播放器配置字段里藏 m3u8（JSON 转义形态）。
			_, _ = writer.Write([]byte(`<html><body><div id="player"></div><script>
				var config = {"url":"https:\/\/cdn.test\/sniffed.m3u8","jx":0};
			</script></body></html>`))
		case request.URL.Path == "/blank":
			_, _ = writer.Write([]byte(`<html><body>没有播放地址</body></html>`))
		default:
			t.Errorf("意外的 fixture 请求: %s", request.URL.RequestURI())
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		// 改道到本地 fixture，保留路径与查询参数（cdn.test/jx.test 等
		// 外部域名的请求都落到同一个 fixture 上）。
		forwarded, err := http.NewRequestWithContext(request.Context(), request.Method, server.URL+request.URL.RequestURI(), nil)
		if err != nil {
			return nil, err
		}
		forwarded.Header = request.Header.Clone()
		return server.Client().Do(forwarded)
	})
	script := strings.ReplaceAll(drpyMediaScript, "__HOST__", server.URL)
	script = strings.ReplaceAll(script, "__PLAY_BODY__", playBody)
	if _, err := engine.importDrpyScript(context.Background(), script); err != nil {
		t.Fatalf("导入脚本失败: %v", err)
	}
	list := engine.drpySiteList()
	if len(list) != 1 {
		t.Fatalf("导入后应有 1 个站点: %+v", list)
	}
	site, ok := engine.downloader.drpySite(drpySourceID(list[0]["key"].(string)))
	if !ok {
		t.Fatal("找不到导入的站点")
	}
	drama, chapters, err := engine.downloader.fetchDrpyDetail(context.Background(), site, drpyEncodeSegment("101"))
	if err != nil {
		t.Fatalf("拉详情失败: %v", err)
	}
	if len(chapters) != 2 {
		t.Fatalf("应有 2 集，实际 %d", len(chapters))
	}
	return engine, site, drama, chapters
}

func drpyMediaTask(drama Drama, chapter Chapter, index int) Task {
	return Task{DramaID: drama.ID, DramaTitle: drama.Title, Chapter: chapter, Index: index, Total: 2}
}

func TestDrpyResolveMediaDirectLink(t *testing.T) {
	engine, _, drama, chapters := drpyMediaEngine(t, `
    play: function(flag, episodeKey, flags){
        return {url: episodeKey, jx: 0, parse: 0};
    },`)
	media, err := engine.downloader.resolveProviderMedia(context.Background(), drpyMediaTask(drama, chapters[0], 0))
	if err != nil {
		t.Fatalf("直链路解析失败: %v", err)
	}
	// play() 原样返回 $ 后段；m3u8 会顺带拉播放列表（fixture 转发后
	// finalURL 主机是本地，只校验路径后缀）。
	if !strings.HasSuffix(media.URL, "/a1.m3u8") {
		t.Fatalf("播放地址错误: %q", media.URL)
	}
	if media.Playlist == "" || !strings.Contains(media.Playlist, "#EXTM3U") {
		t.Fatalf("m3u8 播放列表未拉取: %q", media.Playlist)
	}
	if media.Referer == "" {
		t.Fatal("Referer 不应为空")
	}
}

func TestDrpyResolveMediaPlainStringResult(t *testing.T) {
	engine, _, drama, chapters := drpyMediaEngine(t, `
    play: function(flag, episodeKey, flags){
        return 'https://cdn.test/plain.mp4';
    },`)
	media, err := engine.downloader.resolveProviderMedia(context.Background(), drpyMediaTask(drama, chapters[0], 0))
	if err != nil {
		t.Fatalf("字符串返回解析失败: %v", err)
	}
	if media.URL != "https://cdn.test/plain.mp4" || media.Playlist != "" {
		t.Fatalf("mp4 直链不应拉播放列表: %+v", media)
	}
}

func TestDrpyResolveMediaJxSniff(t *testing.T) {
	engine, _, drama, chapters := drpyMediaEngine(t, `
    play: function(flag, episodeKey, flags){
        return {url: 'https://jx.test/page?ep=2', jx: 1, parse: 0};
    },`)
	// 第 02 集的 episodeKey 就是解析页地址。
	media, err := engine.downloader.resolveProviderMedia(context.Background(), drpyMediaTask(drama, chapters[1], 1))
	if err != nil {
		t.Fatalf("jx=1 嗅探失败: %v", err)
	}
	if media.URL != "https://cdn.test/sniffed.m3u8" && !strings.HasSuffix(media.URL, "/sniffed.m3u8") {
		t.Fatalf("嗅探结果错误: %q", media.URL)
	}
	if media.Playlist == "" {
		t.Fatal("嗅探到的 m3u8 应拉取播放列表")
	}
}

func TestDrpyResolveMediaParseSniffFailure(t *testing.T) {
	engine, _, drama, chapters := drpyMediaEngine(t, `
    play: function(flag, episodeKey, flags){
        return {url: 'https://jx.test/blank', jx: 0, parse: 1};
    },`)
	_, err := engine.downloader.resolveProviderMedia(context.Background(), drpyMediaTask(drama, chapters[1], 1))
	if err == nil || !strings.Contains(err.Error(), "解析播放页失败") {
		t.Fatalf("嗅探失败应报错，得到 %v", err)
	}
}

func TestDrpyResolveMediaLazyGlobalInput(t *testing.T) {
	engine, _, drama, chapters := drpyMediaEngine(t, `
    lazy: function(flag, episodeKey, flags){
        globalThis.input = {url: 'https://cdn.test/lazy.m3u8', jx: 0, parse: 0};
    },`)
	media, err := engine.downloader.resolveProviderMedia(context.Background(), drpyMediaTask(drama, chapters[0], 0))
	if err != nil {
		t.Fatalf("lazy + 全局 input 解析失败: %v", err)
	}
	if !strings.HasSuffix(media.URL, "/lazy.m3u8") {
		t.Fatalf("lazy 结果错误: %q", media.URL)
	}
}

func TestDrpyResolveMediaFallsBackToCachedLink(t *testing.T) {
	engine, _, drama, chapters := drpyMediaEngine(t, `
    play: function(flag, episodeKey, flags){
        throw new Error('play boom');
    },`)
	// play 炸了，但第 01 集自带媒体直链，应退化播放。
	media, err := engine.downloader.resolveProviderMedia(context.Background(), drpyMediaTask(drama, chapters[0], 0))
	if err != nil {
		t.Fatalf("直链兜底失败: %v", err)
	}
	if !strings.HasSuffix(media.URL, "/a1.m3u8") {
		t.Fatalf("兜底地址错误: %q", media.URL)
	}
	// 第 02 集没有直链，应透出脚本错误。
	if _, err := engine.downloader.resolveProviderMedia(context.Background(), drpyMediaTask(drama, chapters[1], 1)); err == nil || !strings.Contains(err.Error(), "play boom") {
		t.Fatalf("无兜底时应透出脚本错误，得到 %v", err)
	}
}

func TestDrpyResolveMediaHeaderReferer(t *testing.T) {
	engine, _, drama, chapters := drpyMediaEngine(t, `
    play: function(flag, episodeKey, flags){
        return {url: 'https://cdn.test/header.mp4', jx: 0, parse: 0,
            header: {Referer: 'https://referer.test/', 'User-Agent': 'custom'}};
    },`)
	media, err := engine.downloader.resolveProviderMedia(context.Background(), drpyMediaTask(drama, chapters[0], 0))
	if err != nil {
		t.Fatalf("header 解析失败: %v", err)
	}
	if media.Referer != "https://referer.test/" {
		t.Fatalf("应取 play 返回的 Referer: %q", media.Referer)
	}
}

func TestDrpyResolveMediaAfterSiteRemoval(t *testing.T) {
	engine, site, drama, chapters := drpyMediaEngine(t, `
    play: function(flag, episodeKey, flags){
        return {url: episodeKey, jx: 0, parse: 0};
    },`)
	if err := engine.removeDrpySite(site.Key); err != nil {
		t.Fatalf("删除站点失败: %v", err)
	}
	// 删除站点后，自带媒体直链的分集仍可播放（缓存直链兜底）。
	media, err := engine.downloader.resolveProviderMedia(context.Background(), drpyMediaTask(drama, chapters[0], 0))
	if err != nil {
		t.Fatalf("删除后直链分集应仍可播放: %v", err)
	}
	if !strings.HasSuffix(media.URL, "/a1.m3u8") {
		t.Fatalf("兜底地址错误: %q", media.URL)
	}
	// 没有直链的分集走默认链路，报无效播放地址。
	if _, err := engine.downloader.resolveProviderMedia(context.Background(), drpyMediaTask(drama, chapters[1], 1)); err == nil || !strings.Contains(err.Error(), "未返回有效播放地址") {
		t.Fatalf("删除后非直链分集应报无效播放地址，得到 %v", err)
	}
}

func TestDrpyPlayResultHelpers(t *testing.T) {
	// 字符串与对象两种返回形态。
	if result := drpyPlayResultFrom("https://a.test/x.mp4"); result.url != "https://a.test/x.mp4" {
		t.Fatalf("字符串形态异常: %+v", result)
	}
	result := drpyPlayResultFrom(map[string]any{"url": "https://a.test/x.m3u8", "jx": "1", "parse": 0, "msg": "x"})
	if result.url != "https://a.test/x.m3u8" || result.jx != 1 || result.parse != 0 || result.msg != "x" {
		t.Fatalf("对象形态异常: %+v", result)
	}
	// goja Export() 对 JS 整数返回 int64（jx/parse 的常见形态）。
	if result := drpyPlayResultFrom(map[string]any{"url": "u", "jx": int64(1), "parse": int64(1)}); result.jx != 1 || result.parse != 1 {
		t.Fatalf("int64 形态异常: %+v", result)
	}
	if result := drpyPlayResultFrom(42); result.url != "" {
		t.Fatalf("未知形态应返回空: %+v", result)
	}
	// header：对象、JSON 字符串、非法字符串、缺省。
	header := drpyHeaderMap(map[string]any{"Referer": "https://r.test/", "skip": nil})
	if drpyHeaderValue(header, "Referer") != "https://r.test/" {
		t.Fatalf("header 对象解析异常: %+v", header)
	}
	if header = drpyHeaderMap(`{"referer":"https://r2.test/"}`); drpyHeaderValue(header, "referer") != "https://r2.test/" {
		t.Fatalf("JSON 字符串 header 解析异常: %+v", header)
	}
	if drpyHeaderMap("not-json") != nil || drpyHeaderMap(nil) != nil {
		t.Fatal("非法 header 应返回 nil")
	}
	if drpyHeaderValue(nil, "Referer") != "" {
		t.Fatal("空 header 应返回空串")
	}
}

func TestDrpyIsDirectMediaURL(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://a.test/x.m3u8", true},
		{"https://a.test/x.mp4?token=1", true},
		{"https://a.test/x.M3U8", true},
		{"https://a.test/page?id=1", false},
		{"https://jx.test/parse?url=x", false},
		{"not-a-url", false},
		{"ftp://a.test/x.mp4", false},
	}
	for _, item := range cases {
		if got := drpyIsDirectMediaURL(item.raw); got != item.want {
			t.Fatalf("drpyIsDirectMediaURL(%q)=%v，期望 %v", item.raw, got, item.want)
		}
	}
}

func TestDrpyUnescapeMediaURL(t *testing.T) {
	if got := drpyUnescapeMediaURL(`https:\/\/cdn.test\/x.m3u8`); got != "https://cdn.test/x.m3u8" {
		t.Fatalf("JSON 转义未还原: %q", got)
	}
	if got := drpyUnescapeMediaURL(`https://a.test/x.m3u8?a=1\u0026b=2`); got != "https://a.test/x.m3u8?a=1&b=2" {
		t.Fatalf("\\u 转义未还原: %q", got)
	}
	if got := drpyUnescapeMediaURL("  https://a.test/x.mp4  "); got != "https://a.test/x.mp4" {
		t.Fatalf("空白未修剪: %q", got)
	}
}
