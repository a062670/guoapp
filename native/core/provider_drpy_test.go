package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// drpyScriptFixture 真实形态的 drpy2 脚本（节选）：var rule + 标准函数签名。
const drpyScriptFixture = `var rule = {
    title: '演示影视',
    host: 'https://demo.test',
    url: '/index.php/api/vod',
    类型: '影视',
    headers: {'User-Agent': 'MOBILE_UA'},
    timeout: 5000,
    limit: 20,
    play_parse: false,
    lazy: function(){ return ''; },
    double: false,
    推荐: function(){ },
    一级: function(){ },
    二级: function(){ },
    搜索: function(){ },
    play: function(){ },
    home: function(filter){
        return {class: [{type_id: '1', type_name: '国产剧'}, {type_id: '2', type_name: '动作片'}]};
    },
    category: function(tid, pg, filter, extend){
        return {list: [], page: pg, pagecount: 1, limit: 20, total: 0};
    },
    detail: function(vodId){
        return {list: [{vod_id: vodId, vod_name: '演示剧'}]};
    },
    search: function(wd, quick, pg){
        return {list: [], page: 1, pagecount: 1, limit: 20, total: 0};
    }
};
function init(ext){
    console.log('init ok');
}
function home(filter){
    return rule.home(filter);
}
function category(tid, pg, filter, extend){
    return rule.category(tid, pg, filter, extend);
}
function detail(vodId){
    return rule.detail(vodId);
}
function play(flag, episodeKey, flags){
    return {url: 'https://video.test/1.m3u8', jx: 0, parse: 0};
}
function search(wd, quick, pg){
    return rule.search(wd, quick, pg);
}
`

func drpyEngine(t *testing.T) *nativeEngine {
	t.Helper()
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, ".js") {
			return sourceFixtureResponse(request, 200, drpyScriptFixture), nil
		}
		return nil, errors.New("M1 管理面测试仅放行 .js 脚本请求")
	})
	return engine
}

func TestDrpyImportScriptTextPersistsAndLists(t *testing.T) {
	engine := drpyEngine(t)
	result, err := engine.importDrpyScript(context.Background(), drpyScriptFixture, "")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	data, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("导入结果类型异常: %T", result)
	}
	key, _ := data["key"].(string)
	if key == "" {
		t.Fatalf("导入结果缺 key: %+v", data)
	}
	if data["name"] != "演示影视" {
		t.Fatalf("导入结果 name=%v", data["name"])
	}
	if !data["added"].(bool) {
		t.Fatal("首次导入应标记 added=true")
	}
	source := drpySourceID(key)
	if _, ok := engine.downloader.drpySite(source); !ok {
		t.Fatalf("导入后 store 里找不到 %s", source)
	}
	// 脚正文落盘。
	scriptPath := filepath.Join(engine.directory, drpyScriptsDir, key+".js")
	body, err := os.ReadFile(scriptPath)
	if err != nil || !strings.Contains(string(body), "var rule") {
		t.Fatalf("脚本正文落盘异常: %v", err)
	}
	// drpy.json 落盘并可重载。
	reloaded := loadDrpyStore(engine.directory)
	site, ok := reloaded.get(source)
	if !ok || site.Name != "演示影视" || site.Title != "演示影视" {
		t.Fatalf("重载失败: %+v %v", site, ok)
	}
	if site.File != drpyScriptsDir+"/"+key+".js" {
		t.Fatalf("重载 file=%q", site.File)
	}
	// 列表接口返回条目。
	list := engine.drpySiteList()
	if len(list) != 1 || list[0]["key"] != key {
		t.Fatalf("drpySiteList 异常: %+v", list)
	}
	// 同 key 覆盖导入标记 added=false。
	result, err = engine.importDrpyScript(context.Background(), drpyScriptFixture, "")
	if err != nil {
		t.Fatalf("重复导入失败: %v", err)
	}
	if data, ok := result.(map[string]any); ok && data["added"].(bool) {
		t.Fatal("重复导入应标记 added=false")
	}
}

func TestDrpyImportScriptURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, ".js") {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/javascript")
		_, _ = writer.Write([]byte(drpyScriptFixture))
	}))
	defer server.Close()
	engine := drpyEngine(t)
	result, err := engine.importDrpyScript(context.Background(), server.URL+"/lib/drpy.js", "")
	if err != nil {
		t.Fatalf("URL 导入失败: %v", err)
	}
	data := result.(map[string]any)
	key := data["key"].(string)
	site, ok := engine.downloader.drpySite(drpySourceID(key))
	if !ok || site.ScriptURL != server.URL+"/lib/drpy.js" {
		t.Fatalf("ScriptURL 记录异常: %+v", site)
	}
}

func TestDrpyImportRejectsInvalidScripts(t *testing.T) {
	engine := drpyEngine(t)
	cases := []struct {
		name   string
		input  string
		expect string
	}{
		{"空内容", "   ", "请输入 drpy 脚本内容或脚本地址"},
		{"缺 rule", "function home(filter){ return {}; }", "未找到 drpy 脚本"},
		{"语法错误", "var rule = { title: '坏';", "脚本语法错误"},
	}
	for _, item := range cases {
		if _, err := engine.importDrpyScript(context.Background(), item.input, ""); err == nil {
			t.Fatalf("%s：应当被拒绝", item.name)
		} else if !strings.Contains(err.Error(), item.expect) {
			t.Fatalf("%s：错误信息应为包含 %q，得到 %q", item.name, item.expect, err.Error())
		}
	}
	// 超大脚本：入口处先按体积拒绝。
	huge := "var rule = { title: 'x', host: 'y' };\n// " + strings.Repeat("a", drpyScriptMaxBytes)
	if _, err := engine.importDrpyScript(context.Background(), huge, ""); err == nil || !strings.Contains(err.Error(), "内容过大") {
		t.Fatalf("超大脚本应被拒绝，得到 %v", err)
	}
	// 空输入。
	if _, err := engine.importDrpyScript(context.Background(), "", ""); err == nil {
		t.Fatal("空输入应当被拒绝")
	}
}

func TestDrpyRemoveSiteClearsCaches(t *testing.T) {
	engine := drpyEngine(t)
	result, err := engine.importDrpyScript(context.Background(), drpyScriptFixture, "")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	key := result.(map[string]any)["key"].(string)
	source := drpySourceID(key)
	// 造目录缓存与任务记录（categoryOptions / sourceRecords 为懒初始化 map）。
	if engine.categoryOptions == nil {
		engine.categoryOptions = map[string][]nativeCategory{}
	}
	if engine.sourceRecords == nil {
		engine.sourceRecords = map[string]nativeSourceRecord{}
	}
	engine.catalogs[source] = []nativeDrama{{ID: source + ":MQ", Source: source, Title: "演示剧"}}
	engine.catalogs[source+"|1"] = []nativeDrama{{ID: source + ":Mg", Source: source, Title: "分类缓存"}}
	engine.catalogStates[source] = nativeCatalogState{Page: 1}
	engine.categoryOptions[source] = []nativeCategory{{ID: "1", Name: "国产剧"}}
	engine.sourceRecords[source] = nativeSourceRecord{Operation: "update", Completed: 1}
	if err := engine.removeDrpySite(key); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, ok := engine.downloader.drpySite(source); ok {
		t.Fatal("删除后 store 仍存在")
	}
	if len(engine.catalogs) != 0 || len(engine.catalogStates) != 0 || len(engine.categoryOptions) != 0 || len(engine.sourceRecords) != 0 {
		t.Fatalf("删除后缓存未清空: catalogs=%d states=%d options=%d records=%d", len(engine.catalogs), len(engine.catalogStates), len(engine.categoryOptions), len(engine.sourceRecords))
	}
	scriptPath := filepath.Join(engine.directory, drpyScriptsDir, key+".js")
	if _, err := os.Stat(scriptPath); !os.IsNotExist(err) {
		t.Fatal("删除后脚本文件仍存在")
	}
	// 重复删除报明确错误。
	if err := engine.removeDrpySite(key); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("重复删除错误异常: %v", err)
	}
	// 无效 key。
	if err := engine.removeDrpySite("bad key!"); err == nil {
		t.Fatal("无效 key 应当被拒绝")
	}
}

func TestDrpyStoreSkipsMissingScriptFile(t *testing.T) {
	directory := t.TempDir()
	store := newDrpyStore(directory)
	site := drpySite{Key: "ghost", Name: "幽灵源", File: drpyScriptsDir + "/ghost.js"}
	if err := os.MkdirAll(filepath.Join(directory, drpyScriptsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, filepath.FromSlash(site.File)), []byte(drpyScriptFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.put(site); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	// 手工删掉脚本文件，重载时应跳过该条目。
	if err := os.Remove(filepath.Join(directory, filepath.FromSlash(site.File))); err != nil {
		t.Fatal(err)
	}
	reloaded := loadDrpyStore(directory)
	if _, ok := reloaded.get("drpy:ghost"); ok {
		t.Fatal("脚本文件缺失的条目不应保留")
	}
}

func TestDrpySourceIDHelpers(t *testing.T) {
	if drpyKeyFromSource("drpy:abc-123") != "abc-123" {
		t.Fatal("drpyKeyFromSource 解析异常")
	}
	if drpyKeyFromSource("libretv:abc") != "" {
		t.Fatal("非 drpy 源应返回空 key")
	}
	if drpyKeyFromSource("drpy:") != "" {
		t.Fatal("空 key 应返回空串")
	}
	if !isHuangguoProviderSource("drpy:abc") {
		t.Fatal("drpy:<key> 应被认定为有效片源家族")
	}
	if !isHuangguoProviderSource("drpy") {
		t.Fatal("家族名 drpy 应被认定")
	}
	if !nativeSourceAvailable("drpy:abc") {
		t.Fatal("drpy 源不受编译开关限制")
	}
}

func TestDrpyIDRoundTrip(t *testing.T) {
	vodIDs := []string{"101", "https://video.test/x?vid=1#frag", "a:b:c", "剧集-1"}
	for _, vodID := range vodIDs {
		dramaID := drpySourceID("site1") + ":" + drpyEncodeSegment(vodID)
		source, encoded, ok := splitProviderDramaID(dramaID)
		if !ok || source != "drpy:site1" || encoded != drpyEncodeSegment(vodID) {
			t.Fatalf("剧集 ID 切分失败: %q → source=%q encoded=%q ok=%v", dramaID, source, encoded, ok)
		}
		// split 只切分；解码由调用方负责。
		decoded, valid := drpyDecodeSegment(encoded)
		if !valid || decoded != vodID {
			t.Fatalf("剧集 ID 解码往返失败: %q → %q", dramaID, decoded)
		}
	}
	// 分集 ID：两段 b64（RawURL 无冒号，Cut 兼容）。
	blob := drpyBlobFromParts("线路1", "第01集$https://video.test/1.m3u8")
	chapterID := drpySourceID("site1") + ":" + drpyEncodeSegment("https://video.test/x?vid=1") + ":" + blob
	source, rest, ok := strings.Cut(strings.TrimPrefix(chapterID, sourceDrpyPrefix), ":")
	if !ok || source != "site1" {
		t.Fatalf("分集 ID 前缀切分异常: %q", chapterID)
	}
	sourceID, blobPart, found := strings.Cut(rest, ":")
	if !found || blobPart != blob {
		t.Fatalf("分集 ID 两段切分异常: rest=%q", rest)
	}
	vodID, ok := drpyDecodeSegment(sourceID)
	if !ok || vodID != "https://video.test/x?vid=1" {
		t.Fatalf("vodId 反解失败: %q", sourceID)
	}
	flag, episodeKey, ok := drpyPartsFromBlob(blobPart)
	if !ok || flag != "线路1" || episodeKey != "第01集$https://video.test/1.m3u8" {
		t.Fatalf("分集 blob 反解失败: flag=%q key=%q", flag, episodeKey)
	}
	// splitProviderDramaID 对分集 ID 返回 source 与「b64(vodId):b64(blob)」。
	family, sourceIDOut, valid := splitProviderDramaID(chapterID)
	if !valid || family != "drpy:site1" || sourceIDOut != drpyEncodeSegment("https://video.test/x?vid=1")+":"+blob {
		t.Fatalf("分集 ID 家族切分异常: family=%q sourceID=%q", family, sourceIDOut)
	}
	// 畸形 ID。
	if _, _, ok := splitProviderDramaID("drpy:site1"); ok {
		t.Fatal("缺 vodId 的 ID 不应有效")
	}
	if _, _, ok := splitProviderDramaID("drpy:site1:" + drpyEncodeSegment("")); ok {
		t.Fatal("空 vodId 不应有效")
	}
}

func TestDrpyDispatchActions(t *testing.T) {
	directory := t.TempDir()
	if _, err := nativeDispatch(nativeInput{Action: "initialize", Directory: directory}); err != nil {
		t.Fatalf("initialize 失败: %v", err)
	}
	t.Cleanup(func() {
		nativeState.Lock()
		nativeState.engine = nil
		nativeState.Unlock()
	})
	nativeState.Lock()
	engine := nativeState.engine
	nativeState.Unlock()
	if engine == nil {
		t.Fatal("initialize 后引擎未就绪")
	}
	// 导入走 dispatch。
	importRaw, err := json.Marshal(nativeInput{Action: "drpyImport", Subscription: drpyScriptFixture})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nativeDispatch(mustDecodeNativeInput(t, importRaw)); err != nil {
		t.Fatalf("drpyImport dispatch 失败: %v", err)
	}
	sites := engine.drpySiteList()
	if len(sites) != 1 {
		t.Fatalf("dispatch 导入后应有 1 个站点: %+v", sites)
	}
	key := sites[0]["key"].(string)
	// 列表走 dispatch。
	listRaw, err := json.Marshal(nativeInput{Action: "drpySites"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := nativeDispatch(mustDecodeNativeInput(t, listRaw))
	if err != nil {
		t.Fatalf("drpySites dispatch 失败: %v", err)
	}
	if sites := result.(map[string]any)["sites"].([]map[string]any); len(sites) != 1 {
		t.Fatalf("drpySites dispatch 返回异常: %+v", sites)
	}
	// 删除走 dispatch。
	removeRaw, err := json.Marshal(nativeInput{Action: "drpyRemove", DrpyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nativeDispatch(mustDecodeNativeInput(t, removeRaw)); err != nil {
		t.Fatalf("drpyRemove dispatch 失败: %v", err)
	}
	if sites := engine.drpySiteList(); len(sites) != 0 {
		t.Fatalf("删除后仍有站点: %+v", sites)
	}
}

func mustDecodeNativeInput(t *testing.T, raw []byte) nativeInput {
	t.Helper()
	var input nativeInput
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	return input
}
