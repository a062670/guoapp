package core

// 海阔视界（XYQHiker / csp_XYQHiker）规则引擎。
//
// 背景：海阔视界是另一个 TVBox 写源工具，规则是 51 个中文字段的 JSON
// （分类 / 搜索 / 播放三段各自独立配置），原实现是 Java Spider
// （XYQHiker.jar），本文件用 Go 重写解释器。
//
// 与 XBPQ 的关键差异：海阔的截取规则是 **CSS 选择器链**（`.list-con&&.items`、
// `mip-search-video&&video-src`），与 drpy 的 pdfh/pdfa 方言同宗，所以
// HTML 模式直接复用 drpy 的选择器引擎（drpySelectAll / drpySelectValue，
// 同包零成本）；JSON 模式复用 ruleJSONPath。
//
// 两种模式的开关是「XX截取模式」：0 = JSON，1 = HTML。
// JSON 模式常见的套路是接口返回 jsonp（`jsonp({...});`），先用
// 「分类Json数据二次截取」把 `jsonp(` 与 `);` 之间的 JSON 抠出来，
// 再用「分类列表数组规则」的 JSON 路径导航。
//
// 取值字段（标题/链接/图片/副标题）的写法三种：
//   - JSON 键名：`name` / `video_id`（JSON 模式下按路径取值）；
//   - CSS 选择器链：`.title&&Text` / `mip-search-video&&video-src`
//     （HTML 模式，末段是取值方式，同 drpy 规则）；
//   - 字面量：`'立即播放'`（单引号包裹，直接返回引号里的内容）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// hikerRule 海阔规则里与抓取相关的字段（字段名与规则 JSON 的中文键一一对应）。
type hikerRule struct {
	// 站点
	Charset string // 网页编码格式
	Headers string // 请求头参数：User-Agent$手机#Referer$...
	// 分类
	CateURL        string // 分类链接（含 {cateId} {catePg}）
	CateNames      string // 分类名称（& 分隔）
	CateValues     string // 分类名称替换词（& 分隔，对应 {cateId}）
	CateMode       string // 分类截取模式：0=JSON / 1=HTML
	CateCut        string // 分类Json数据二次截取
	CateArray      string // 分类列表数组规则
	CateTitle      string // 分类片单标题
	CateLink       string // 分类片单链接
	CatePic        string // 分类片单图片
	CateSub        string // 分类片单副标题
	CateLinkPrefix string // 分类片单链接加前缀
	CateLinkSuffix string // 分类片单链接加后缀
	// 搜索
	SearchURL        string // 搜索链接
	SearchHeaders    string // 搜索请求头参数
	SearchPostData   string // POST 请求数据
	SearchMode       string // 搜索截取模式
	SearchCut        string // 搜索数据二次截取（部分规则写法）
	SearchArray      string // 搜索列表数组规则
	SearchTitle      string // 搜索片单标题
	SearchLink       string // 搜索片单链接
	SearchPic        string // 搜索片单图片
	SearchSub        string // 搜索片单副标题
	SearchLinkPrefix string // 搜索片单链接加前缀
	SearchLinkSuffix string // 搜索片单链接加后缀
	// 播放
	DirectPlay   string // 链接是否直接播放（2 = 是）
	DirectPrefix string // 直接播放链接加前缀
	DirectSuffix string // 直接播放链接加后缀
	PlayScope    string // 播放列表数组规则（详情页里的播放区范围）
	Episodes     string // 选集列表数组规则
	EpTitle      string // 选集标题
	EpLink       string // 选集链接
	EpReverse    string // 是否反选集序列
	EpPrefix     string // 选集链接加前缀
	EpSuffix     string // 选集链接加后缀
	SniffWords   string // 手动嗅探视频链接关键词
}

// hikerParseRule 规则 JSON → 结构体。
func hikerParseRule(rule map[string]any) hikerRule {
	text := func(keys ...string) string {
		for _, key := range keys {
			if value := ruleText(rule, key); value != "" {
				return value
			}
		}
		return ""
	}
	return hikerRule{
		Charset:          text("网页编码格式"),
		Headers:          text("请求头参数"),
		CateURL:          text("分类链接"),
		CateNames:        text("分类名称"),
		CateValues:       text("分类名称替换词"),
		CateMode:         text("分类截取模式"),
		CateCut:          text("分类Json数据二次截取"),
		CateArray:        text("分类列表数组规则"),
		CateTitle:        text("分类片单标题"),
		CateLink:         text("分类片单链接"),
		CatePic:          text("分类片单图片"),
		CateSub:          text("分类片单副标题"),
		CateLinkPrefix:   text("分类片单链接加前缀"),
		CateLinkSuffix:   text("分类片单链接加后缀"),
		SearchURL:        text("搜索链接"),
		SearchHeaders:    text("搜索请求头参数"),
		SearchPostData:   text("POST请求数据"),
		SearchMode:       text("搜索截取模式"),
		SearchCut:        text("搜索数据二次截取"),
		SearchArray:      text("搜索列表数组规则"),
		SearchTitle:      text("搜索片单标题"),
		SearchLink:       text("搜索片单链接"),
		SearchPic:        text("搜索片单图片"),
		SearchSub:        text("搜索片单副标题"),
		SearchLinkPrefix: text("搜索片单链接加前缀"),
		SearchLinkSuffix: text("搜索片单链接加后缀"),
		DirectPlay:       text("链接是否直接播放"),
		DirectPrefix:     text("直接播放链接加前缀"),
		DirectSuffix:     text("直接播放链接加后缀"),
		PlayScope:        text("播放列表数组规则"),
		Episodes:         text("选集列表数组规则"),
		EpTitle:          text("选集标题"),
		EpLink:           text("选集链接"),
		EpReverse:        text("是否反选集序列"),
		EpPrefix:         text("选集链接加前缀"),
		EpSuffix:         text("选集链接加后缀"),
		SniffWords:       text("手动嗅探视频链接关键词"),
	}
}

// fetchHikerCategories 分类列表：分类名称 + 分类名称替换词 直出（零网络）。
func (d *Downloader) fetchHikerCategories(site libretvSite, entry ruleEntry) ([]nativeCategory, error) {
	rule := hikerParseRule(entry.Rule)
	names := strings.Split(rule.CateNames, "&")
	out := make([]nativeCategory, 0, len(names))
	for index, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			// 分类链接为空时用「全部」占位（部分规则没有分类配置）。
			continue
		}
		// ID 用序号（同 XBPQ：真实分类值可能是中文，序号稳定且通过校验）。
		out = append(out, nativeCategory{ID: fmt.Sprintf("%d", index+1), Name: name})
	}
	return out, nil
}

// hikerCategoryValue 分类 ID（序号）→ 规则里的真实分类值。
func hikerCategoryValue(rule hikerRule, category string) string {
	values := strings.Split(rule.CateValues, "&")
	position := ruleAtoi(category)
	if position >= 1 && position <= len(values) {
		return strings.TrimSpace(values[position-1])
	}
	if len(values) > 0 && strings.TrimSpace(values[0]) != "" {
		return strings.TrimSpace(values[0])
	}
	return strings.TrimSpace(category)
}

// hikerHeaders 规则请求头 → map（海阔与 XBPQ 的 Key$Value#Key2$Value2 同格式）。
func hikerHeaders(rule hikerRule, search bool) map[string]string {
	spec := rule.Headers
	if search {
		spec = firstNonEmpty(rule.SearchHeaders, rule.Headers)
	}
	return ruleParseHeaders(spec)
}

// hikerBuildURL 链接占位符替换 + 第一页特例 + 相对地址补全。
//
// 海阔约定：`分类链接` 末尾可带 `[firstPg=另一个地址]`，表示第一页与
// 后续页地址不同（很多站点首页没有页码段，真实规则「低端影视」就是
// `.../list/{cateId}--------{catePg}---.html[firstPg=.../category/{cateId}.html]`）。
func hikerBuildURL(template, cateID, keyword string, page int) string {
	address := strings.TrimSpace(template)
	if address == "" {
		return ""
	}
	// 抽出第一页地址（方括号里 key=value，可能出现在末尾）。
	firstPage := ""
	if open := strings.Index(address, "[firstPg="); open >= 0 {
		if end := strings.Index(address[open:], "]"); end > 0 {
			firstPage = strings.TrimSpace(address[open+len("[firstPg=") : open+end])
			address = strings.TrimSpace(address[:open] + address[open+end+1:])
		}
	}
	if page <= 1 && firstPage != "" {
		address = firstPage
	}
	address = strings.ReplaceAll(address, "{cateId}", cateID)
	address = strings.ReplaceAll(address, "{catePg}", fmt.Sprintf("%d", page))
	address = strings.ReplaceAll(address, "{wd}", url.QueryEscape(keyword))
	address = strings.ReplaceAll(address, "{pg}", fmt.Sprintf("%d", page))
	// 筛选占位符（海阔规则常见 {by}/{area}/{class}/{year}/{lang}/{letter}）：
	// v1 没有筛选面板，按站点「不限」形态填空——排序默认时间，其余留空。
	address = strings.ReplaceAll(address, "{by}", "time")
	address = strings.ReplaceAll(address, "{area}", "")
	address = strings.ReplaceAll(address, "{class}", "")
	address = strings.ReplaceAll(address, "{year}", "")
	address = strings.ReplaceAll(address, "{lang}", "")
	address = strings.ReplaceAll(address, "{letter}", "")
	return address
}

// hikerFetch 抓页并按规则声明转码。
func (d *Downloader) hikerFetch(ctx context.Context, target string, headers map[string]string, referer, charset string) (string, error) {
	fetchCtx := withUntrustedTargets(ctx)
	if agent := headers["User-Agent"]; agent != "" {
		fetchCtx = context.WithValue(fetchCtx, providerTextUserAgentKey{}, agent)
	}
	body, err := d.fetchProviderText(fetchCtx, target, referer)
	if err != nil {
		return "", err
	}
	return ruleDecodeGBK(body, charset), nil
}

// fetchHikerCatalogPage 分类页 / 搜索结果页。
func (d *Downloader) fetchHikerCatalogPage(ctx context.Context, site libretvSite, entry ruleEntry, page int, category, query string) ([]Drama, bool, error) {
	rule := hikerParseRule(entry.Rule)
	if page < 1 {
		page = 1
	}
	var (
		target, mode                                  string
		cut, array                                    string
		title, link, pic, sub, linkPrefix, linkSuffix string
		headers                                       map[string]string
		postBody                                      string
	)
	if query != "" {
		if strings.TrimSpace(rule.SearchURL) == "" {
			return nil, false, errors.New("该站源不支持搜索")
		}
		headers = hikerHeaders(rule, true)
		address := hikerSplitPost(rule.SearchURL)
		target = hikerBuildURL(address, "", query, page)
		postBody = strings.ReplaceAll(rule.SearchPostData, "{wd}", query)
		mode = firstNonEmpty(rule.SearchMode, rule.CateMode)
		cut = firstNonEmpty(rule.SearchCut, rule.CateCut)
		array = firstNonEmpty(rule.SearchArray, rule.CateArray)
		title = firstNonEmpty(rule.SearchTitle, rule.CateTitle)
		link = firstNonEmpty(rule.SearchLink, rule.CateLink)
		pic = firstNonEmpty(rule.SearchPic, rule.CatePic)
		sub = firstNonEmpty(rule.SearchSub, rule.CateSub)
		linkPrefix = firstNonEmpty(rule.SearchLinkPrefix, rule.CateLinkPrefix)
		linkSuffix = firstNonEmpty(rule.SearchLinkSuffix, rule.CateLinkSuffix)
	} else {
		if strings.TrimSpace(rule.CateURL) == "" {
			return nil, false, errors.New("该站源没有配置分类链接")
		}
		headers = hikerHeaders(rule, false)
		target = hikerBuildURL(rule.CateURL, hikerCategoryValue(rule, category), "", page)
		mode = rule.CateMode
		cut = rule.CateCut
		array = rule.CateArray
		title, link, pic, sub = rule.CateTitle, rule.CateLink, rule.CatePic, rule.CateSub
		linkPrefix, linkSuffix = rule.CateLinkPrefix, rule.CateLinkSuffix
	}
	if target == "" {
		return nil, false, errors.New("分类链接无效")
	}
	referer := libretvSiteReferer(site)
	var (
		body string
		err  error
	)
	if postBody != "" {
		// POST 搜索同样要按规则编码转码（与 GET 路径一致）。
		var posted string
		posted, err = rulePostText(ctx, d, target, postBody, headers, referer)
		body = ruleDecodeGBK(posted, rule.Charset)
	} else {
		body, err = d.hikerFetch(ctx, target, headers, referer, rule.Charset)
	}
	if err != nil {
		return nil, false, fmt.Errorf("站点请求失败：%w", publicError(err))
	}
	if strings.TrimSpace(body) == "" {
		// 真实站点常见：返回 200 但正文为空（反爬 / 需要 JS 渲染 / 整站挂掉）。
		// 与「规则失效」区分开，避免用户误以为要更新规则。
		return nil, false, errors.New("站点返回空内容（可能触发反爬或需要浏览器环境）")
	}
	dramas := hikerDramasFromBody(site, body, mode, cut, array, title, link, pic, sub, linkPrefix, linkSuffix, referer)
	if len(dramas) == 0 {
		return nil, false, errors.New("没有从分类页解析到片单，规则可能已失效")
	}
	return dramas, true, nil
}

// hikerDramasFromBody 页面 → 片单列表。
//
// mode "0" 走 JSON（先做二次截取再按路径导航），"1" 走 HTML（CSS 链）。
// 具体判定还留了一层兜底：模式没写时，内容能解析成 JSON 就按 JSON 处理。
func hikerDramasFromBody(site libretvSite, body, mode, cut, array, title, link, pic, sub, linkPrefix, linkSuffix, referer string) []Drama {
	source := libretvSourceID(site.Key)
	scope := body
	if strings.TrimSpace(cut) != "" {
		scope = hikerCutJSONP(body, cut)
	}
	jsonMode := strings.TrimSpace(mode) == "0"
	if strings.TrimSpace(mode) == "" {
		var probe any
		jsonMode = json.Unmarshal([]byte(strings.TrimSpace(scope)), &probe) == nil
	}
	var dramas []Drama
	if jsonMode {
		var document any
		if json.Unmarshal([]byte(strings.TrimSpace(scope)), &document) != nil {
			return nil
		}
		items, _ := ruleJSONPath(document, array).([]any)
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			field := func(path string) string { return ruleJSONText(ruleJSONPath(entry, path)) }
			name := field(title)
			address := ruleResolveURL(referer, strings.TrimSpace(linkPrefix)+field(link)+strings.TrimSpace(linkSuffix))
			if name == "" || address == "" {
				continue
			}
			sourceID := drpyEncodeSegment(address)
			remark := field(sub)
			dramas = append(dramas, Drama{
				ID: providerDramaID(source, sourceID), Source: source, SourceID: sourceID,
				Title: name, Name: name,
				Cover:    ruleResolveURL(referer, field(pic)),
				Remark:   remark,
				Episodes: episodeIndex(remark, 0),
			})
		}
		return dramas
	}
	// HTML 模式：复用 drpy 选择器。
	root := drpyParseHTML(scope)
	if root == nil {
		return nil
	}
	for _, node := range drpySelectAll(root, drpySegments(array)) {
		name := hikerValue(node, title)
		address := ruleResolveURL(referer, strings.TrimSpace(linkPrefix)+hikerValue(node, link)+strings.TrimSpace(linkSuffix))
		if name == "" || address == "" {
			continue
		}
		sourceID := drpyEncodeSegment(address)
		remark := hikerValue(node, sub)
		dramas = append(dramas, Drama{
			ID: providerDramaID(source, sourceID), Source: source, SourceID: sourceID,
			Title: name, Name: name,
			Cover:    ruleResolveURL(referer, hikerValue(node, pic)),
			Remark:   remark,
			Episodes: episodeIndex(remark, 0),
		})
	}
	return dramas
}

// hikerCutJSONP 二次截取：`jsonp(&&);` 表示取 jsonp( 与 ); 之间的内容。
// 没有 && 时按「整段就是要的 JSON」处理（"Base64" 等特殊值 v1 不支持）。
func hikerCutJSONP(body, cut string) string {
	cut = strings.TrimSpace(cut)
	if cut == "" || cut == "Base64" {
		return body
	}
	spec := xbpqParseField(cut)
	if len(spec.markers) == 0 {
		return body
	}
	start := xbpqFindBefore(body, spec.markers[0])
	if start < 0 {
		return body
	}
	rest := body[start:]
	if len(spec.markers) > 1 && spec.markers[1] != "" {
		if end := strings.Index(rest, spec.markers[1]); end >= 0 {
			rest = rest[:end]
		}
	}
	return strings.TrimSpace(rest)
}

// hikerValue 在节点内按规则取值：
//   - 单引号字面量（'立即播放'）直接返回引号内容；
//   - 其余按「选择器链 && 取值方式」解析。取值方式除了 drpy 白名单里的
//     Text/href/src，还包含带连字符的任意属性名（video-src、data-echo、
//     mip-img 的 src 等）——海阔规则大量使用自定义属性，drpy 的固定
//     白名单不够用，这里用「含连字符即视为属性」补齐。
func hikerValue(node *html.Node, spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" || node == nil {
		return ""
	}
	if len(spec) >= 2 && strings.HasPrefix(spec, "'") && strings.HasSuffix(spec, "'") {
		return strings.TrimSpace(spec[1 : len(spec)-1])
	}
	for _, alternative := range strings.Split(spec, "||") {
		segments := drpySegments(alternative)
		if len(segments) == 0 {
			continue
		}
		last := segments[len(segments)-1]
		if want := hikerWantToken(last); want != "" {
			nodes := drpySelectAll(node, segments[:len(segments)-1])
			if len(nodes) == 0 {
				continue
			}
			if value := drpyNodeValue(nodes[0], want); value != "" {
				return value
			}
			continue
		}
		// 末段也是选择器：取第一个命中节点的文本。
		nodes := drpySelectAll(node, segments)
		if len(nodes) == 0 {
			continue
		}
		if value := drpyNodeValue(nodes[0], "Text"); value != "" {
			return value
		}
	}
	return ""
}

// hikerWantToken 判断末段是不是「取值方式」：drpy 白名单或带连字符的属性名。
func hikerWantToken(sel drpySelector) string {
	if sel.want != "" {
		return sel.want
	}
	if strings.Contains(sel.tag, "-") {
		return sel.tag
	}
	return ""
}

// hikerSplitPost 搜索链接的 POST 约定：`地址;post` 或 `地址;post;参数`。
// 是否是 POST 由 POST请求数据 是否非空决定，这里只把地址摘出来。
func hikerSplitPost(spec string) string {
	spec = strings.TrimSpace(spec)
	if index := strings.Index(spec, ";post"); index >= 0 {
		return strings.TrimSpace(spec[:index])
	}
	return spec
}

// fetchHikerDetail 详情页：直接播放（单集）或按选集规则抓分集。
func (d *Downloader) fetchHikerDetail(ctx context.Context, site libretvSite, entry ruleEntry, vodID string) (Drama, []Chapter, error) {
	rule := hikerParseRule(entry.Rule)
	headers := hikerHeaders(rule, false)
	referer := libretvSiteReferer(site)
	detailURL, decoded := drpyDecodeSegment(vodID)
	if !decoded {
		detailURL = ""
	}
	if !isProviderHTTPMediaURL(detailURL) {
		detailURL = ruleResolveURL(referer, detailURL)
	}
	if !isProviderHTTPMediaURL(detailURL) {
		return Drama{}, nil, errors.New("详情地址无效，请刷新列表后重试")
	}
	source := libretvSourceID(site.Key)
	// 「链接是否直接播放」= 1：列表链接本身就是视频地址（直播 / 直链站）；
	// = 2：选集链接是直链（不需要嗅探），但详情页仍要抓选集列表——
	// 兔小贝这类规则就是 2，选集地址在 mip-search-video 的 video-src 上。
	if strings.TrimSpace(rule.DirectPlay) == "1" {
		address := strings.TrimSuffix(strings.TrimSpace(rule.DirectPrefix)+detailURL+strings.TrimSpace(rule.DirectSuffix), "#isVideo=true#")
		address = strings.TrimSuffix(address, "#isVideo=true")
		drama := Drama{ID: providerDramaID(source, vodID), Source: source, SourceID: vodID, Title: "在线播放", Name: "在线播放"}
		return drama, ruleChapters(source, vodID, detailURL, []ruleLine{{
			Title:    "默认",
			Episodes: []ruleEpisode{{Title: "第01集", URL: address}},
		}}), nil
	}
	body, err := d.hikerFetch(ctx, detailURL, headers, referer, rule.Charset)
	if err != nil {
		return Drama{}, nil, fmt.Errorf("详情页请求失败：%w", publicError(err))
	}
	title := drpyPDFH(body, "title&&Text")
	if title == "" {
		title = detailURL
	}
	drama := Drama{ID: providerDramaID(source, vodID), Source: source, SourceID: vodID, Title: title, Name: title}
	episodes := hikerEpisodes(body, rule, detailURL)
	chapters := ruleChapters(source, vodID, detailURL, []ruleLine{{Title: "默认", Episodes: episodes}})
	if len(chapters) == 0 {
		// 兜底：详情页即播放页，交给播放链路嗅探。
		chapters = ruleChapters(source, vodID, detailURL, []ruleLine{{
			Title: "默认", Episodes: []ruleEpisode{{Title: "第01集", URL: detailURL}},
		}})
	}
	return drama, chapters, nil
}

// hikerEpisodes 详情页 → 分集（播放列表数组规则 → 选集列表数组规则 → 标题/链接）。
func hikerEpisodes(body string, rule hikerRule, pageURL string) []ruleEpisode {
	scopeText := body
	if strings.TrimSpace(rule.PlayScope) != "" {
		if value := drpyPDFH(body, rule.PlayScope); value != "" {
			scopeText = value
		}
	}
	root := drpyParseHTML(scopeText)
	if root == nil {
		return nil
	}
	var episodes []ruleEpisode
	for _, node := range drpySelectAll(root, drpySegments(rule.Episodes)) {
		title := hikerValue(node, rule.EpTitle)
		address := hikerValue(node, rule.EpLink)
		address = ruleResolveURL(pageURL, strings.TrimSpace(rule.EpPrefix)+address+strings.TrimSpace(rule.EpSuffix))
		address = strings.TrimSuffix(address, "#isVideo=true#")
		if address == "" {
			continue
		}
		episodes = append(episodes, ruleEpisode{Title: title, URL: address})
	}
	if strings.TrimSpace(rule.EpReverse) == "1" {
		for i, j := 0, len(episodes)-1; i < j; i, j = i+1, j-1 {
			episodes[i], episodes[j] = episodes[j], episodes[i]
		}
	}
	return episodes
}
