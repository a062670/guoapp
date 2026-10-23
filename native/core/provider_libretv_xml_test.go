package core

import (
	"context"
	"math/big"
	"strings"
	"testing"
)

// libretvBase58Encode 测试用编码：与 bs58 一致，供构造订阅文本。
func libretvBase58Encode(raw []byte) string {
	value := new(big.Int).SetBytes(raw)
	radix := big.NewInt(58)
	var reversed []byte
	for value.Sign() > 0 {
		mod := new(big.Int)
		value.DivMod(value, radix, mod)
		reversed = append(reversed, libretvBase58Alphabet[mod.Int64()])
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	leading := 0
	for leading < len(raw) && raw[leading] == 0 {
		leading++
	}
	return strings.Repeat("1", leading) + string(reversed)
}

const libretvXMLListBody = `<?xml version="1.0" encoding="utf-8"?><rss version="5.1"><list page="1" pagecount="3" pagesize="20" recordcount="45">
<video><last>2026-10-19 10:00:00</last><id>201</id><tid>13</tid><name><![CDATA[XML测试剧]]></name><type>国产剧</type><pic>https://img.example.com/x.jpg</pic><lang>国语</lang><area>内地</area><year>2025</year><state>1</state><note><![CDATA[更新至第8集]]></note><actor><![CDATA[张三,李四]]></actor><director><![CDATA[王五]]></director><score>9.1</score></video>
<video><id>202</id><tid>6</tid><name><![CDATA[XML完结剧]]></name><type>动作片</type><note><![CDATA[全12集]]></note></video>
</list><class><ty id="13">国产剧</ty><ty id="6">动作片</ty></class></rss>`

const libretvXMLDetailBody = `<?xml version="1.0" encoding="utf-8"?><rss version="5.1"><list page="1" pagecount="1" pagesize="20" recordcount="1">
<video><last>2026-10-19 09:30:00</last><id>201</id><tid>13</tid><name><![CDATA[XML测试剧]]></name><type>国产剧</type><pic>https://img.example.com/x.jpg</pic><lang>国语</lang><area>内地</area><year>2025</year><note><![CDATA[更新至第2集]]></note>
<actor><![CDATA[张三,李四]]></actor><director><![CDATA[王五]]></director><score>9.1</score><tag><![CDATA[悬疑,犯罪]]></tag>
<des><![CDATA[<p>XML 简介&amp;说明</p>]]></des>
<dl><dd flag="ffm3u8"><![CDATA[第01集$https://cdn.example.com/x1.m3u8#第02集$https://cdn.example.com/x2.m3u8]]></dd>
<dd flag="mihua"><![CDATA[第01集$https://cdn.example.com/y1.m3u8#第02集$https://cdn.example.com/y2.m3u8]]></dd></dl>
</video></list></rss>`

// libretvXMLFixture 按 ids 参数返回详情，否则返回列表（含 class）。
func libretvXMLFixture(t *testing.T) *libretvFixture {
	t.Helper()
	return newLibreTVFixture(t, func(path string, query map[string]string) (int, string) {
		if query["ids"] != "" {
			return 200, libretvXMLDetailBody
		}
		return 200, libretvXMLListBody
	})
}

func TestLibreTVBase58Decode(t *testing.T) {
	config := `{"sites":[{"key":"xml","name":"XML站","type":2,"api":"https://example.com/api.php/provide/vod/at/xml/"}]}`
	encoded := libretvBase58Encode([]byte(config))
	if !looksLikeLibreTVBase58(encoded) {
		t.Fatalf("编码结果应被识别为 Base58：%q", encoded)
	}
	decoded, err := libretvBase58Decode(encoded)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if string(decoded) != config {
		t.Fatalf("解码内容不一致:\n%s\n%s", decoded, config)
	}
	// 订阅文件常见换行，解码前应被忽略。
	wrapped := encoded[:24] + "\r\n" + encoded[24:] + "\n"
	if decoded, err := libretvBase58Decode(wrapped); err != nil || string(decoded) != config {
		t.Fatalf("带换行的 Base58 解码失败: %v", err)
	}
	// 非法字符必须报错，而不是静默产出垃圾。
	if _, err := libretvBase58Decode("0OIl" + encoded); err == nil {
		t.Fatal("含非法 Base58 字符应当报错")
	}
	if _, err := libretvBase58Decode("   "); err == nil {
		t.Fatal("空内容应当报错")
	}
	if looksLikeLibreTVBase58("https://example.com/config.json") {
		t.Fatal("URL 不应被识别为 Base58")
	}
}

func TestLibreTVParseSubscriptionSupportsXMLType(t *testing.T) {
	body := `{"sites":[
	 {"key":"json","name":"JSON站","type":1,"api":"https://a.example.com/api.php/provide/vod"},
	 {"key":"xml","name":"XML站","type":2,"api":"https://b.example.com/api.php/provide/vod/at/xml/"},
	 {"key":"drpy","name":"脚本站","type":3,"api":"https://c.example.com/api.js"}
	]}`
	parsed, err := parseLibreTVSubscription(body, "")
	var sites []libretvSite
	var drpyCandidates []drpyCandidate
	var _ []libretvParse
	var skipped []string
	if err == nil {
		sites, drpyCandidates, _, skipped = parsed.Sites, parsed.DrpyCandidates, parsed.Parses, parsed.Skipped
	}
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(sites) != 2 {
		t.Fatalf("应导入 2 个站点: %+v", sites)
	}
	if sites[0].Type != 1 || sites[1].Type != 2 {
		t.Fatalf("站点类型错误: %+v", sites)
	}
	// type=3 的 .js 脚本源收成 drpy 候选（M4），不再计入 skipped。
	if len(drpyCandidates) != 1 || drpyCandidates[0].API != "https://c.example.com/api.js" {
		t.Fatalf("type=3 .js 应提取为 drpy 候选: %+v", drpyCandidates)
	}
	if len(skipped) != 0 {
		t.Fatalf("合法 .js 候选不应被跳过: %+v", skipped)
	}
}

func TestLibreTVFetchXMLCatalogAndCategories(t *testing.T) {
	fixture := libretvXMLFixture(t)
	downloader := libretvTestDownloader(t, fixture)
	site := libretvSite{Key: "xml", Name: "XML站", API: fixture.server.URL + "/api.php/provide/vod/at/xml/", Type: 2}

	dramas, hasMore, err := downloader.fetchLibreTVCatalogPage(context.Background(), site, 1, "", "")
	if err != nil {
		t.Fatalf("XML 列表失败: %v", err)
	}
	if len(dramas) != 2 || dramas[0].ID != "libretv:xml:201" || dramas[0].Title != "XML测试剧" {
		t.Fatalf("XML 列表解析错误: %+v", dramas)
	}
	if dramas[0].Episodes != 8 || dramas[0].ReleaseStatus != "ongoing" {
		t.Fatalf("集数/状态错误: %+v", dramas[0])
	}
	if dramas[0].Score != "9.1" || dramas[0].Actor != "张三,李四" || dramas[0].Director != "王五" {
		t.Fatalf("评分/主演/导演错误: %+v", dramas[0])
	}
	if dramas[0].Cover != "https://img.example.com/x.jpg" || dramas[0].TypeName != "国产剧" {
		t.Fatalf("海报/分类错误: %+v", dramas[0])
	}
	if dramas[1].Episodes != 12 || dramas[1].ReleaseStatus != "finished" {
		t.Fatalf("完结状态错误: %+v", dramas[1])
	}
	if !hasMore {
		t.Fatal("page=1 < pagecount=3，应当还有下一页")
	}

	categories, err := downloader.fetchLibreTVCategories(context.Background(), site)
	if err != nil {
		t.Fatalf("XML 分类失败: %v", err)
	}
	if len(categories) != 2 || categories[0].ID != "13" || categories[1].Name != "动作片" {
		t.Fatalf("XML 分类解析错误: %+v", categories)
	}
}

func TestLibreTVFetchXMLDetailMultiPlaySources(t *testing.T) {
	fixture := libretvXMLFixture(t)
	downloader := libretvTestDownloader(t, fixture)
	site := libretvSite{Key: "xml", Name: "XML站", API: fixture.server.URL + "/api.php/provide/vod/at/xml/", Type: 2}

	drama, chapters, err := downloader.fetchLibreTVDetail(context.Background(), site, "201")
	if err != nil {
		t.Fatalf("XML 详情失败: %v", err)
	}
	// des 里的 HTML 标签与实体应被清理。
	if drama.Desc != "XML 简介&说明" {
		t.Fatalf("简介解析错误: %q", drama.Desc)
	}
	if drama.Score != "9.1" || drama.Actor != "张三,李四" || drama.Director != "王五" {
		t.Fatalf("详情元数据错误: %+v", drama)
	}
	// XML 的 <last> 映射更新时间为展示格式，<tag> 并入标签。
	if drama.UpdateTime != "2026-10-19 09:30" {
		t.Fatalf("XML 更新时间映射错误: %q", drama.UpdateTime)
	}
	if strings.Join(drama.Tags, "|") != "悬疑|犯罪|内地|国语|2025" {
		t.Fatalf("XML 标签合并错误: %v", drama.Tags)
	}
	if len(chapters) != 4 {
		t.Fatalf("两个播放源各 2 集，应有 4 集，实际 %d", len(chapters))
	}
	expected := []string{"第01集", "第01集·mihua", "第02集", "第02集·mihua"}
	for index, chapter := range chapters {
		if chapter.Title != expected[index] {
			t.Fatalf("分集 %d 标题错误: %q（期望 %q）", index, chapter.Title, expected[index])
		}
		if chapter.Source != "libretv:xml" || !strings.HasPrefix(chapter.Referer, "http") {
			t.Fatalf("分集来源/Referer 错误: %+v", chapter)
		}
	}
}

func TestLibreTVXMLFallbackWhenTypeUnmarked(t *testing.T) {
	fixture := libretvXMLFixture(t)
	downloader := libretvTestDownloader(t, fixture)
	// 类型标成 JSON（裸地址导入时的默认判定），但接口实际返回 XML：
	// 解析失败后应按响应格式自动回退。
	site := libretvSite{Key: "xml", Name: "XML站", API: fixture.server.URL + "/api.php/provide/vod", Type: 1}
	dramas, _, err := downloader.fetchLibreTVCatalogPage(context.Background(), site, 1, "", "")
	if err != nil {
		t.Fatalf("XML 回退失败: %v", err)
	}
	if len(dramas) != 2 {
		t.Fatalf("回退后应解析 2 部剧: %+v", dramas)
	}
}

func TestLibreTVImportBase58Subscription(t *testing.T) {
	fixture := libretvXMLFixture(t)
	engine := sourceFixtureEngine(t, forwardToFixture(fixture))
	engine.downloader.libretv = loadLibreTVStore(t.TempDir())

	config := `{"sites":[{"key":"xml","name":"XML站","type":2,"api":"` + fixture.server.URL + `/api.php/provide/vod/at/xml/"}]}`
	encoded := libretvBase58Encode([]byte(config))
	// 模拟 LunaTV-config 导出的订阅文本：单行长串，可能带尾部换行。
	encoded = encoded[:50] + "\n" + encoded[50:] + "\n"

	result, err := engine.importLibreTVSubscription(context.Background(), encoded)
	if err != nil {
		t.Fatalf("Base58 订阅导入失败: %v", err)
	}
	payload := result.(map[string]any)
	added, _ := payload["added"].([]map[string]any)
	if len(added) != 1 || added[0]["key"] != "xml" {
		t.Fatalf("Base58 导入站点错误: %+v", added)
	}
	if siteType, _ := added[0]["type"].(int); siteType != 2 {
		t.Fatalf("导入后站点类型应为 XML(2): %+v", added[0])
	}
	if verified, _ := payload["verified"].(int); verified != 1 {
		t.Fatalf("Base58 导入探测未通过: %+v", payload)
	}
	// 站点列表应把 type 一并透出给 Dart 侧展示。
	sites := engine.libretvSiteList()
	if len(sites) != 1 {
		t.Fatalf("站点列表错误: %+v", sites)
	}
	if siteType, _ := sites[0]["type"].(int); siteType != 2 {
		t.Fatalf("站点列表缺少类型: %+v", sites[0])
	}
}

func TestLibreTVImportBareXMLAPIURL(t *testing.T) {
	fixture := libretvXMLFixture(t)
	engine := sourceFixtureEngine(t, forwardToFixture(fixture))
	engine.downloader.libretv = loadLibreTVStore(t.TempDir())

	api := fixture.server.URL + "/api.php/provide/vod/at/xml/"
	result, err := engine.importLibreTVSubscription(context.Background(), api)
	if err != nil {
		t.Fatalf("裸 XML 接口导入失败: %v", err)
	}
	payload := result.(map[string]any)
	added, _ := payload["added"].([]map[string]any)
	if len(added) != 1 {
		t.Fatalf("应导入单站点: %+v", added)
	}
	if siteType, _ := added[0]["type"].(int); siteType != 2 {
		t.Fatalf("at/xml 地址应识别为 type=2: %+v", added[0])
	}
}

func TestLibreTVSiteStoreKeepsType(t *testing.T) {
	directory := t.TempDir()
	store := loadLibreTVStore(directory)
	if _, err := store.put(libretvSite{Key: "xml", Name: "XML站", API: "https://example.com/at/xml/", Type: 2}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	// 重新加载后类型仍然保留（旧文件没有 type 字段时按 JSON 处理）。
	reloaded := loadLibreTVStore(directory)
	site, ok := reloaded.get("libretv:xml")
	if !ok || site.Type != 2 {
		t.Fatalf("重载后类型丢失: %+v", site)
	}
}
