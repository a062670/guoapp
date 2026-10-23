package core

// 规则引擎的「真实约定」回归集：这些用例全部来自今天对真实订阅与站点的
// 实测发现，固化成确定性测试，避免以后改引擎时悄悄退化。
//
//   - 海阔 `[firstPg=...]` 第一页地址（低端影视真实规则）
//   - 海阔筛选占位符 {by}/{area}/{year}（樱花动漫 / 哔嘀影视真实规则）
//   - 规则 JSON 带 // 注释（短剧屋真实规则）
//   - 相对规则路径 ./XYQHiker/x.json（XYQ 订阅真实写法）
//   - 站点返回空正文 / 密码页 / 风控时的提示
//   - XBPQ 无跳转详情页 + 多线路、POST 搜索、GBK 编码
//   - 旧版 libretv.json（无 engine 字段、无 rules.json）的迁移兼容

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// TestRuleStripJSONComments 注释剥离：字符串里的 // 不能被误伤。
func TestRuleStripJSONComments(t *testing.T) {
	raw := `{
		// 行注释
		"分类链接": "http://a.com/list/{cateId}", /* 块注释 */
		"标题": "含 // 的标题", // 尾注释
		"url": "https://b.com/x?q=1"
	}`
	var rule map[string]any
	if err := json.Unmarshal([]byte(ruleStripJSONComments(raw)), &rule); err != nil {
		t.Fatalf("剥注释后仍无法解析：%v", err)
	}
	if rule["分类链接"] != "http://a.com/list/{cateId}" {
		t.Fatalf("URL 被误伤：%v", rule["分类链接"])
	}
	if rule["标题"] != "含 // 的标题" {
		t.Fatalf("字符串内容被误伤：%v", rule["标题"])
	}
	// 真实规则（短剧屋）带注释，必须能解析。
	if err := json.Unmarshal([]byte(ruleStripJSONComments(ruleFixtureBody(t, "hiker_duanjuwu.json"))), &rule); err != nil {
		t.Fatalf("真实带注释规则解析失败：%v", err)
	}
}

// TestHikerFirstPageAndFilterPlaceholders 第一页地址与筛选占位符替换。
func TestHikerFirstPageAndFilterPlaceholders(t *testing.T) {
	var requested []string
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		requested = append(requested, request.URL.String())
		return sourceFixtureResponse(request, 200, `<html><body><div class="stui-vodlist"><li><h4><a href="/v/1.html">剧集一</a></h4><span class="pic-text">更新至10集</span></li></div></body></html>`), nil
	})
	site := libretvSite{Key: "ddys", Name: "低端影视", API: "http://127.0.0.1:9/", Type: 1, Engine: ruleEngineHiker}
	rule := map[string]any{
		// 真实规则写法：第一页走 /category/{cateId}.html，后续页带页码 + {by}。
		"分类链接":          "http://127.0.0.1:9/list/{cateId}--------{catePg}---.html[firstPg=http://127.0.0.1:9/category/{cateId}.html]",
		"分类名称":          "电影&剧集",
		"分类名称替换词":       "1&2",
		"分类截取模式":        "1",
		"分类列表数组规则":      ".stui-vodlist&&li",
		"分类片单是否Jsoup写法": "1",
		"分类片单标题":        "h4&&a&&Text",
		"分类片单链接":        "h4&&a&&href",
		"分类片单副标题":       ".pic-text&&Text",
	}
	entry := ruleEntry{Key: "ddys", Engine: ruleEngineHiker, Rule: rule}
	if _, _, err := engine.downloader.fetchHikerCatalogPage(context.Background(), site, entry, 1, "1", ""); err != nil {
		t.Fatalf("第一页失败：%v", err)
	}
	if _, _, err := engine.downloader.fetchHikerCatalogPage(context.Background(), site, entry, 2, "1", ""); err != nil {
		t.Fatalf("第二页失败：%v", err)
	}
	if len(requested) != 2 {
		t.Fatalf("请求次数不符：%v", requested)
	}
	if !strings.Contains(requested[0], "/category/1.html") {
		t.Fatalf("第一页应走 firstPg 地址：%s", requested[0])
	}
	if !strings.Contains(requested[1], "/list/1--------2---.html") {
		t.Fatalf("第二页应走模板地址：%s", requested[1])
	}
	// {by}/{area} 等筛选占位符必须被替换掉，不能把花括号发到站点。
	rule2 := map[string]any{"分类链接": "http://127.0.0.1:9/s/{cateId}/{catePg}?type={class}&year={year}&order={by}&area={area}"}
	if value := hikerBuildURL(ruleText(rule2, "分类链接"), "all", "", 1); strings.ContainsAny(value, "{}") {
		t.Fatalf("筛选占位符未替换：%s", value)
	}
}

// TestHikerSiteFailureMessages 站点异常时的提示必须可区分：
// 空正文 ≠ 密码页 ≠ 规则失效。
func TestHikerSiteFailureMessages(t *testing.T) {
	rule := map[string]any{
		"分类链接":     "http://127.0.0.1:9/list/{cateId}/{catePg}",
		"分类名称":     "电影",
		"分类名称替换词":  "1",
		"分类截取模式":   "1",
		"分类列表数组规则": ".stod-list&&li",
		"分类片单标题":   "a&&Text",
		"分类片单链接":   "a&&href",
	}
	entry := ruleEntry{Key: "s", Engine: ruleEngineHiker, Rule: rule}
	site := libretvSite{Key: "s", Name: "站点", API: "http://127.0.0.1:9/", Type: 1, Engine: ruleEngineHiker}

	// 空正文（反爬 / 需要 JS）。
	empty := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		return sourceFixtureResponse(request, 200, "   "), nil
	})
	if _, _, err := empty.downloader.fetchHikerCatalogPage(context.Background(), site, entry, 1, "1", ""); err == nil || !strings.Contains(err.Error(), "空内容") {
		t.Fatalf("空正文提示不符：%v", err)
	}
	// 密码页：能取到 HTML 但没有列表结构。
	password := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		return sourceFixtureResponse(request, 200, `<html><body><div class="password-container"><input class="password-input"></div></body></html>`), nil
	})
	if _, _, err := password.downloader.fetchHikerCatalogPage(context.Background(), site, entry, 1, "1", ""); err == nil || !strings.Contains(err.Error(), "没有从分类页解析到片单") {
		t.Fatalf("密码页提示不符：%v", err)
	}
}

// TestXBPQNoJumpDetailMultiLine XBPQ 详情页直接带多线路播放列表（无跳转）。
func TestXBPQNoJumpDetailMultiLine(t *testing.T) {
	listBody := `<html><body><div class="globalPicList"><ul class="resize_list"><li><a href="/vod-detail-id-1.html" title="测试剧"><img data-echo="https://img.example.com/1.jpg"><span class="sTit">测试剧</span></a></li></ul></div></body></html>`
	detailBody := `<html><head><title>测试剧</title></head><body>
<div class="lines"><span class="line">线路一</span><span class="line">线路二</span></div>
<div class="play"><a href="/play/1-1-1.html">第01集</a><a href="/play/1-1-2.html">第02集</a></div>
<div class="play"><a href="/play/1-2-1.html">第01集</a></div>
</body></html>`
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Path, "vod-list-id-"):
			return sourceFixtureResponse(request, 200, listBody), nil
		case strings.HasPrefix(request.URL.Path, "/vod-detail-id-"):
			return sourceFixtureResponse(request, 200, detailBody), nil
		}
		return sourceFixtureResponse(request, 404, "not found"), nil
	})
	rule := map[string]any{
		"主页url": "http://127.0.0.1:9",
		"分类url": "http://127.0.0.1:9/vod-list-id-{cateId}-pg-{catePg}-order--by-{by}-class-{class}-year-{year}-letter--area-{area}-lang-.html",
		"分类":    "电影$1#电视剧$2",
		"二次截取":  "globalPicList&&</body>",
		"数组":    "<li>&&</li>",
		"标题":    `title="&&"`,
		"图片":    `data-echo="&&"`,
		"链接":    `href="&&"`,
		"线路数组":  `class="line">&&</span>`,
		"播放数组":  `class="play">&&</div>`,
		"播放列表":  "<a&&</a>",
		"播放标题":  ">&&<",
		"播放链接":  `href="&&"`,
	}
	entry := ruleEntry{Key: "multi", Engine: ruleEngineXBPQ, Rule: rule}
	site := libretvSite{Key: "multi", Name: "多线路站", API: "http://127.0.0.1:9", Type: 1, Engine: ruleEngineXBPQ}
	items, _, err := engine.downloader.fetchXBPQCatalogPage(context.Background(), site, entry, 1, "1", "")
	if err != nil || len(items) != 1 {
		t.Fatalf("列表失败：%v %+v", err, items)
	}
	_, chapters, err := engine.downloader.fetchXBPQDetail(context.Background(), site, entry, items[0].SourceID)
	if err != nil {
		t.Fatalf("详情失败：%v", err)
	}
	if len(chapters) != 3 {
		t.Fatalf("多线路分集数不符（应 3）：%+v", chapters)
	}
	// 相对地址必须按详情页补全。
	for _, chapter := range chapters {
		if !strings.HasPrefix(chapter.VideoURL, "http://127.0.0.1:9/play/") {
			t.Fatalf("分集地址未补全：%q", chapter.VideoURL)
		}
	}
}

// TestXBPQPostSearchAndGBK POST 搜索 + gb2312 页面转码。
func TestXBPQPostSearchAndGBK(t *testing.T) {
	gkbList := `<html><body><div class="globalPicList"><ul><li><a href="/vod-detail-id-9.html" title="庆余年">庆余年</a></li></ul></div></body></html>`
	encoded, err := simplifiedchinese.GBK.NewEncoder().String(gkbList)
	if err != nil {
		t.Fatal(err)
	}
	var method, body string
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.RequestURI(), "search") {
			method = request.Method
			buffer := make([]byte, request.ContentLength)
			if request.Body != nil {
				_, _ = request.Body.Read(buffer)
			}
			body = string(buffer)
			return sourceFixtureResponse(request, 200, encoded), nil
		}
		return sourceFixtureResponse(request, 200, encoded), nil
	})
	rule := map[string]any{
		"主页url": "http://127.0.0.1:9",
		"编码":    "gb2312",
		"分类url": "http://127.0.0.1:9/vod-list-id-{cateId}-{catePg}.html",
		"分类":    "电影$1",
		"二次截取":  "globalPicList&&</body>",
		"数组":    "<li>&&</li>",
		"标题":    `title="&&"`,
		"链接":    `href="&&"`,
		"搜索url": "http://127.0.0.1:9/search;post;wd={wd}",
		"搜索数组":  "<li>&&</li>",
		"搜索标题":  `title="&&"`,
		"搜索链接":  `href="&&"`,
	}
	entry := ruleEntry{Key: "gbk", Engine: ruleEngineXBPQ, Rule: rule}
	site := libretvSite{Key: "gbk", Name: "GBK站", API: "http://127.0.0.1:9", Type: 1, Engine: ruleEngineXBPQ}
	items, _, err := engine.downloader.fetchXBPQCatalogPage(context.Background(), site, entry, 1, "1", "庆余年")
	if err != nil {
		t.Fatalf("POST 搜索失败：%v", err)
	}
	if method != http.MethodPost {
		t.Fatalf("应使用 POST：%s", method)
	}
	if !strings.Contains(body, "wd=") {
		t.Fatalf("POST 体不含搜索词：%q", body)
	}
	if len(items) != 1 || items[0].Title != "庆余年" {
		t.Fatalf("GBK 转码失败：%+v", items)
	}
	// 分类页同样走 GBK 解码。
	if items, _, err := engine.downloader.fetchXBPQCatalogPage(context.Background(), site, entry, 1, "1", ""); err != nil || len(items) != 1 || items[0].Title != "庆余年" {
		t.Fatalf("分类页 GBK 解码失败：%v %+v", err, items)
	}
}

// TestLibreTVLegacyStoreCompatibility 旧版本地库（无 engine 字段 / 无 rules.json）
// 必须按普通采集站正常加载，不能因为新字段缺失被丢弃。
func TestLibreTVLegacyStoreCompatibility(t *testing.T) {
	directory := t.TempDir()
	legacy := `{"sites":[{"key":"old","name":"旧采集站","api":"https://old.example.com/api.php/provide/vod","type":1}]}`
	if err := os.WriteFile(filepath.Join(directory, "libretv.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	stack := loadLibreTVStore(directory)
	site, ok := stack.get("libretv:old")
	if !ok {
		t.Fatal("旧版站点被丢弃")
	}
	if site.Engine != "" {
		t.Fatalf("旧版站点不应有引擎标签：%q", site.Engine)
	}
	// 没有 rules.json 时规则表为空，且不报错。
	rules := loadRuleStore(directory)
	if len(rules.list()) != 0 {
		t.Fatal("空规则表应无条目")
	}
}
