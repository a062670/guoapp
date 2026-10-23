package core

// XBPQ（小白配置）规则引擎：csp_XBPQ 片源的纯 Go 实现。
//
// 背景：XBPQ 是 TVBox 生态里流行的「写源」格式——不用写代码，用一段
// JSON 描述站点结构，靠「截取」语法从 HTML/JSON 里取数据。原实现是
// Java Spider（XBPQ.jar），本文件用 Go 重写解释器，不依赖 JVM。
//
// 规则字段（中文键）见 xbpqRule；截取语义（社区文档 + 真实规则实证）：
//
//   - 截取 = 「前标记&&后标记」：取两者之间的内容；单个前标记表示以它
//     切分/取其后全部；`$$` 与 `&&` 等价（调试助手写法）；
//   - 前标记里的 `*` 是通配符（非贪婪，一个字段最多一个），
//     例 `<a*>&&` 匹配 `<a href="...">` 这种开标签；
//   - `$ # & * [ ]` 用作连接符，表示本义时写 `\&` 转义；
//   - 字段末尾的中括号是指令：`[不包含:首页#资讯]`（数组过滤，
//     纯数字的 token 是 1 起的序号，`9-11` 是区间）、`[包含:xxx]`、
//     `[替换:旧>>新#旧2>>空]`（`空` = 删除，`(*)` 匹配括号内容，
//     替换侧可用 `<序号>`）；
//   - JSON 模式：文档本身是 JSON 时，数组/字段写 JSON 路径
//     （data.list[1].name / list[] / list[3,]），见 ruleJSONPath；
//   - `{cateId} {catePg} {wd}` 等占位符在拼地址时替换。
//
// 与 drpy 的关系：XBPQ 的选择器方言与 drpy 的 pdfh/pdfa 同宗（&& 链 +
// || 备选），但 XBPQ 的「前标记&&后标记」是纯字符串截取（不是 CSS），
// 所以这里自研 marker 截取，不复用 drpySelectAll。
//
// 分集播放地址的三段式约定（与采集站一致）：第 1 段是线路名、第 2 段是
// 集名 $ 播放地址、段间 # 分隔——XBPQ 的播放列表本来就是按线路分块，
// 这里按块收集后交给 ruleChapters 统一整理。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// xbpqRule XBPQ 规则的静态字段（只取与浏览/详情/搜索相关的键；
// 筛选、图片代理、短视频等展示类字段暂不参与抓取）。
type xbpqRule struct {
	SiteName      string // 站名
	HomeURL       string // 主页url
	Charset       string // 编码：UTF-8 / gb2312
	Headers       string // 请求头：Key$Value#Key2$Value2（UA 可写 手机/MOBILE_UA）
	SearchHeaders string // 搜索请求头（空时用 Headers）
	// 分类
	Categories  string // 分类：名称$值#名称2$值2（也支持 & 分隔的海阔写法）
	CategoryURL string // 分类url：含 {cateId} {catePg} 等占位符
	// 分类页截取
	SecondCut  string // 二次截取（先收窄页面范围）
	Array      string // 数组
	Title      string // 标题
	Picture    string // 图片
	Link       string // 链接
	Subtitle   string // 副标题
	LinkPrefix string // 链接前缀
	LinkSuffix string // 链接后缀
	// 详情字段
	Year     string // 影片年代
	Area     string // 影片地区
	TypeName string // 影片类型
	Status   string // 状态
	Director string // 导演
	Actor    string // 主演
	Content  string // 简介
	// 线路
	LineSecondCut string // 线路二次截取
	LineArray     string // 线路数组
	LineTitle     string // 线路标题
	// 播放
	PlaySecondCut  string // 播放二次截取
	PlayArray      string // 播放数组（每线路一个块）
	PlayList       string // 播放列表（块内每集）
	PlayTitle      string // 播放标题
	PlayLink       string // 播放链接
	PlayLinkPrefix string // 播放链接前缀
	PlayLinkSuffix string // 播放链接后缀
	// 跳转（详情页不是播放页时，先跳转再取播放列表）
	JumpSecondCut string // 跳转二次截取
	JumpArray     string // 跳转数组
	JumpLink      string // 跳转链接
	// 搜索
	SearchURL        string // 搜索url（可带 ;post; 后缀表示 POST）
	SearchHeaders2   string // 备用：部分规则写「搜索请求头参数」
	SearchSecondCut  string // 搜索二次截取
	SearchArray      string // 搜索数组
	SearchTitle      string // 搜索标题
	SearchPicture    string // 搜索图片
	SearchLink       string // 搜索链接
	SearchSubtitle   string // 搜索副标题
	SearchLinkPrefix string // 搜索链接前缀
	SearchLinkSuffix string // 搜索链接后缀
	// 播放形态
	DirectPlay string // 直接播放：1 = 列表链接即播放地址
	// 嗅探（v1 只做提示，嗅探由播放链路统一处理）
	SniffWords  string // 嗅探词
	FilterWords string // 过滤词
}

// xbpqParseRule 规则 JSON → 结构体。所有字段都是可选的（XBPQ 文档：
// 「除分类url外，大部分可以省略」），缺字段时对应能力自然为空。
func xbpqParseRule(rule map[string]any) xbpqRule {
	text := func(keys ...string) string {
		for _, key := range keys {
			if value := ruleText(rule, key); value != "" {
				return value
			}
		}
		return ""
	}
	return xbpqRule{
		SiteName:         text("站名"),
		HomeURL:          text("主页url"),
		Charset:          text("编码"),
		Headers:          text("请求头"),
		SearchHeaders:    text("搜索请求头", "搜索请求头参数"),
		Categories:       text("分类"),
		CategoryURL:      text("分类url"),
		SecondCut:        text("二次截取"),
		Array:            text("数组"),
		Title:            text("标题"),
		Picture:          text("图片"),
		Link:             text("链接"),
		Subtitle:         text("副标题"),
		LinkPrefix:       text("链接前缀"),
		LinkSuffix:       text("链接后缀"),
		Year:             text("影片年代"),
		Area:             text("影片地区"),
		TypeName:         text("影片类型"),
		Status:           text("状态"),
		Director:         text("导演"),
		Actor:            text("主演"),
		Content:          text("简介"),
		LineSecondCut:    text("线路二次截取"),
		LineArray:        text("线路数组"),
		LineTitle:        text("线路标题"),
		PlaySecondCut:    text("播放二次截取"),
		PlayArray:        text("播放数组"),
		PlayList:         text("播放列表"),
		PlayTitle:        text("播放标题"),
		PlayLink:         text("播放链接"),
		PlayLinkPrefix:   text("播放链接前缀"),
		PlayLinkSuffix:   text("播放链接后缀"),
		JumpSecondCut:    text("跳转二次截取"),
		JumpArray:        text("跳转数组"),
		JumpLink:         text("跳转链接"),
		SearchURL:        text("搜索url"),
		SearchSecondCut:  text("搜索二次截取"),
		SearchArray:      text("搜索数组"),
		SearchTitle:      text("搜索标题"),
		SearchPicture:    text("搜索图片"),
		SearchLink:       text("搜索链接"),
		SearchSubtitle:   text("搜索副标题"),
		SearchLinkPrefix: text("搜索链接前缀"),
		SearchLinkSuffix: text("搜索链接后缀"),
		DirectPlay:       text("直接播放"),
		SniffWords:       text("嗅探词"),
		FilterWords:      text("过滤词"),
	}
}

// xbpqSpecSet 截取字段是否真的配置了（空串 = 未配置，表示「整页」）。
func xbpqSpecSet(spec string) bool {
	return strings.TrimSpace(spec) != ""
}

// xbpqField 一条截取字段：标记段 + 末尾指令。
type xbpqField struct {
	markers []string // 前标记 / 后标记（单元素表示「以它切分」）
	exclude string   // [不包含:...] 的内容
	include string   // [包含:...] 的内容
	replace string   // [替换:...] 的内容
}

// xbpqParseField 解析截取字段：先摘出末尾的指令，
// 再把剩余部分按 && / $$ 切成标记段（处理反斜杠转义）。
func xbpqParseField(spec string) xbpqField {
	spec = strings.TrimSpace(spec)
	field := xbpqField{}
	for {
		trimmed := strings.TrimSpace(spec)
		if !strings.HasSuffix(trimmed, "]") {
			break
		}
		open := strings.LastIndex(trimmed, "[")
		if open < 0 {
			break
		}
		inner := trimmed[open+1 : len(trimmed)-1]
		key, value, found := strings.Cut(inner, ":")
		if !found {
			break
		}
		matched := true
		switch strings.TrimSpace(key) {
		case "不包含":
			field.exclude = value
		case "包含":
			field.include = value
		case "替换":
			field.replace = value
		case "排序":
			// 排序指令只影响展示顺序，v1 忽略（不打乱源站顺序）。
		default:
			matched = false
		}
		if !matched {
			break
		}
		spec = trimmed[:open]
	}
	field.markers = xbpqSplitPattern(spec)
	return field
}

// xbpqSplitPattern 按 && 或 $$ 切分标记段；`\X` 转义为字面量 X。
// 注意：`[替换:...]` 里的 `$$$` 属于指令内容，在 xbpqParseField 里
// 已被摘走，不会走到这里。
func xbpqSplitPattern(pattern string) []string {
	var parts []string
	var current strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\\' && i+1 < len(pattern) {
			current.WriteByte(pattern[i+1])
			i++
			continue
		}
		if c == '&' && i+1 < len(pattern) && pattern[i+1] == '&' {
			parts = append(parts, current.String())
			current.Reset()
			i++
			continue
		}
		if c == '$' && i+1 < len(pattern) && pattern[i+1] == '$' {
			parts = append(parts, current.String())
			current.Reset()
			i++
			continue
		}
		current.WriteByte(c)
	}
	parts = append(parts, current.String())
	// 去掉纯空白的尾段（"<li>&&</li>" 切成 2 段；"<li>&&" 切成 2 段，
	// 第二段为空 = 取标记后全部）。
	for len(parts) > 0 && len(parts) > 1 && parts[len(parts)-1] == "" && len(parts) > 2 {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// xbpqFindBefore 在 text 里定位前标记，返回内容起始位置。
// 前标记可含一个 `*` 通配（非贪婪）：拆成前缀/后缀，先找前缀，
// 再从该位置往后找后缀；找不到返回 -1。
func xbpqFindBefore(text, before string) int {
	if before == "" {
		return 0
	}
	if star := strings.Index(before, "*"); star >= 0 {
		prefix, suffix := before[:star], before[star+1:]
		start := strings.Index(text, prefix)
		if start < 0 {
			return -1
		}
		if suffix == "" {
			return start + len(prefix)
		}
		rest := text[start+len(prefix):]
		end := strings.Index(rest, suffix)
		if end < 0 {
			return -1
		}
		return start + len(prefix) + end + len(suffix)
	}
	start := strings.Index(text, before)
	if start < 0 {
		return -1
	}
	return start + len(before)
}

// xbpqCut 按字段截取第一个匹配值（前标记与后标记之间），并应用替换指令。
// 只有前标记时取「标记之后全部内容」。
func xbpqCut(text string, field xbpqField) string {
	if len(field.markers) == 0 || text == "" {
		return ""
	}
	// 空规则（字段没配）必须返回空，不能退化成「整段文本」——
	// 否则图片 / 副标题会变成整块 HTML。
	if field.markers[0] == "" && len(field.markers) == 1 {
		return ""
	}
	before := field.markers[0]
	after := ""
	if len(field.markers) > 1 {
		after = field.markers[1]
	}
	start := xbpqFindBefore(text, before)
	if start < 0 {
		return ""
	}
	rest := text[start:]
	if after != "" {
		end := strings.Index(rest, after)
		if end < 0 {
			return ""
		}
		rest = rest[:end]
	}
	return xbpqApplyReplace(strings.TrimSpace(rest), field.replace, 1)
}

// xbpqCutAll 按字段截取全部匹配值（数组语义）：
//   - 前后标记成对出现：每对取一段（如 `<li>&&</li>`）；
//   - 只有前标记：以它切分（每段从标记后到下一次标记前）；
//   - 最后按 [不包含] / [包含] 过滤，按 [替换] 逐个替换。
func xbpqCutAll(text string, field xbpqField) []string {
	if len(field.markers) == 0 || text == "" {
		return nil
	}
	if field.markers[0] == "" && len(field.markers) == 1 {
		return nil
	}
	before := field.markers[0]
	after := ""
	if len(field.markers) > 1 {
		after = field.markers[1]
	}
	var items []string
	rest := text
	if after == "" {
		// 单标记：按它切分。
		first := true
		for {
			start := xbpqFindBefore(rest, before)
			if start < 0 {
				if !first && strings.TrimSpace(rest) != "" {
					items = append(items, rest)
				}
				break
			}
			if !first {
				items = append(items, rest[:start])
			}
			first = false
			rest = rest[start:]
		}
	} else {
		for {
			start := xbpqFindBefore(rest, before)
			if start < 0 {
				break
			}
			rest = rest[start:]
			end := strings.Index(rest, after)
			if end < 0 {
				break
			}
			items = append(items, rest[:end])
			rest = rest[end+len(after):]
		}
	}
	return xbpqFilterItems(items, field)
}

// xbpqFilterItems 应用 [不包含] / [包含] / [替换]。
// 不包含的 token 全为数字或区间（1 / 9-11）时按序号过滤（1 起），
// 否则按关键词过滤——与 XBPQ 文档的两种用法一致。
func xbpqFilterItems(items []string, field xbpqField) []string {
	if len(items) == 0 {
		return nil
	}
	if field.exclude != "" {
		tokens := xbpqSplitDirective(field.exclude, '#')
		indexMode := len(tokens) > 0
		for _, token := range tokens {
			if !xbpqIndexToken(token) {
				indexMode = false
				break
			}
		}
		if indexMode {
			drop := map[int]bool{}
			for _, token := range tokens {
				if from, to, found := strings.Cut(token, "-"); found {
					start, end := ruleAtoi(from), ruleAtoi(to)
					for i := start; i <= end && i > 0; i++ {
						drop[i] = true
					}
					continue
				}
				if position := ruleAtoi(token); position > 0 {
					drop[position] = true
				}
			}
			filtered := make([]string, 0, len(items))
			for index, item := range items {
				if !drop[index+1] {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		} else {
			filtered := make([]string, 0, len(items))
			for _, item := range items {
				matched := false
				for _, token := range tokens {
					if token != "" && strings.Contains(item, token) {
						matched = true
						break
					}
				}
				if !matched {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		}
	}
	if field.include != "" {
		tokens := xbpqSplitDirective(field.include, '#')
		filtered := make([]string, 0, len(items))
		for _, item := range items {
			for _, token := range tokens {
				if token != "" && strings.Contains(item, token) {
					filtered = append(filtered, item)
					break
				}
			}
		}
		items = filtered
	}
	if field.replace != "" {
		for index, item := range items {
			items[index] = xbpqApplyReplace(item, field.replace, index+1)
		}
	}
	return items
}

// xbpqIndexToken 判断 token 是否为序号（1 或 9-11）。
func xbpqIndexToken(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	for _, part := range strings.Split(token, "-") {
		part = strings.TrimSpace(part)
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// xbpqSplitDirective 指令内容切分：按 sep 切，但 `\sep` 表示本义。
func xbpqSplitDirective(text string, sep byte) []string {
	var parts []string
	var current strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\\' && i+1 < len(text) {
			current.WriteByte(text[i+1])
			i++
			continue
		}
		if c == sep {
			parts = append(parts, current.String())
			current.Reset()
			continue
		}
		current.WriteByte(c)
	}
	parts = append(parts, current.String())
	return parts
}

// xbpqApplyReplace 应用 [替换:旧>>新#旧2>>空] 指令。
//   - `空` 表示删除；`<序号>` 替换为当前序号（数组里是 1 起的下标）；
//   - 旧串里的 `*` 是通配（翻译成正则 .*?），其余字符按字面量；
//   - 新串里的 `*` 表示「取截取结果」（v1 按空处理，常见规则用不到）。
func xbpqApplyReplace(value, spec string, sequence int) string {
	if spec == "" || value == "" {
		return value
	}
	for _, entry := range xbpqSplitDirective(spec, '#') {
		old, replacement, found := strings.Cut(entry, ">>")
		if !found {
			continue
		}
		old = strings.TrimSpace(old)
		replacement = strings.TrimSpace(replacement)
		if replacement == "空" {
			replacement = ""
		}
		if replacement == "*" {
			replacement = ""
		}
		replacement = strings.ReplaceAll(replacement, "<序号>", strconv.Itoa(sequence))
		if old == "" {
			continue
		}
		if strings.Contains(old, "*") {
			pattern := regexp.QuoteMeta(old)
			pattern = strings.ReplaceAll(pattern, `\*`, `.*?`)
			if compiled, err := regexp.Compile(pattern); err == nil {
				value = compiled.ReplaceAllString(value, replacement)
			}
			continue
		}
		value = strings.ReplaceAll(value, old, replacement)
	}
	return value
}

// xbpqSplitCategories 分类字段切分：名称$值#名称2$值2（也支持海阔写法
// 名称&名称2 + 分类值 1&2，两个字段分开给）。
func xbpqSplitCategories(raw string) [][2]string {
	var out [][2]string
	for _, entry := range strings.Split(raw, "#") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, value, found := strings.Cut(entry, "$")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if name == "" {
			continue
		}
		if !found {
			value = name
		}
		out = append(out, [2]string{name, value})
	}
	return out
}

// xbpqBuildURL 拼请求地址：占位符替换 + 相对地址补全。
//
// 筛选占位符（{area} {by} {class} {year} {lang} {letter}）在 v1 没有
// 筛选 UI，按 macCMS 的「不限」形态填空：{by} 默认 time（排序默认值），
// 其余留空（实测 vod-list / show 两种地址形态下留空都能返回全部数据）。
func xbpqBuildURL(rule xbpqRule, base, template, cateID, keyword string, page int) string {
	address := strings.TrimSpace(template)
	if address == "" {
		return ""
	}
	if base == "" {
		base = rule.HomeURL
	}
	address = strings.ReplaceAll(address, "{cateId}", cateID)
	address = strings.ReplaceAll(address, "{catePg}", strconv.Itoa(page))
	address = strings.ReplaceAll(address, "{pg}", strconv.Itoa(page))
	address = strings.ReplaceAll(address, "{wd}", url.QueryEscape(keyword))
	address = strings.ReplaceAll(address, "{keyword}", url.QueryEscape(keyword))
	address = strings.ReplaceAll(address, "{area}", "")
	address = strings.ReplaceAll(address, "{by}", "time")
	address = strings.ReplaceAll(address, "{class}", "")
	address = strings.ReplaceAll(address, "{year}", "")
	address = strings.ReplaceAll(address, "{lang}", "")
	address = strings.ReplaceAll(address, "{letter}", "")
	return ruleResolveURL(base, address)
}

// fetchXBPQPage 拉取规则页：应用规则请求头（UA 宏）与 Referer，
// 按需做 gb2312 → UTF-8 转码。
func (d *Downloader) fetchXBPQPage(ctx context.Context, target string, headers map[string]string, referer string, charset string) (string, error) {
	if referer == "" {
		referer = target
	}
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

// xbpqRuleFor 规则 + 站点 → 请求上下文（headers / referer）。
func xbpqContext(rule xbpqRule, site libretvSite) (map[string]string, string) {
	headers := ruleParseHeaders(rule.Headers)
	referer := libretvSiteReferer(site)
	if referer == "" {
		referer = rule.HomeURL
	}
	return headers, referer
}

// fetchXBPQCategories 分类列表：规则 分类 字段直出（零网络请求）。
//
// ID 用 1 起的序号而不是规则里的真实分类值：站点分类值可能是中文
// （「电影」「动漫」），而原生目录的分类 ID 校验要求可安全传输——
// 序号既稳定又天然通过校验，真实分类值在 catalog 里按序号回查。
func (d *Downloader) fetchXBPQCategories(site libretvSite, entry ruleEntry) ([]nativeCategory, error) {
	rule := xbpqParseRule(entry.Rule)
	list := xbpqSplitCategories(rule.Categories)
	out := make([]nativeCategory, 0, len(list))
	for index, item := range list {
		out = append(out, nativeCategory{ID: strconv.Itoa(index + 1), Name: item[0]})
	}
	return out, nil
}

// xbpqCategoryValue 分类 ID（序号）→ 规则里的真实分类值。
func xbpqCategoryValue(rule xbpqRule, category string) string {
	list := xbpqSplitCategories(rule.Categories)
	if len(list) == 0 {
		return strings.TrimSpace(category)
	}
	position := ruleAtoi(category)
	if position >= 1 && position <= len(list) {
		return list[position-1][1]
	}
	// 全部 / 非法 ID：落到第一个分类（XBPQ 站点没有「全部」页）。
	return list[0][1]
}

// fetchXBPQCatalogPage 分类页 / 搜索结果页。
func (d *Downloader) fetchXBPQCatalogPage(ctx context.Context, site libretvSite, entry ruleEntry, page int, category, query string) ([]Drama, bool, error) {
	rule := xbpqParseRule(entry.Rule)
	headers, referer := xbpqContext(rule, site)
	if page < 1 {
		page = 1
	}
	var (
		target                                                             string
		scopeSpec                                                          xbpqField
		arraySpec                                                          xbpqField
		titleSpec, picSpec, linkSpec, subtitleSpec, linkPrefix, linkSuffix string
	)
	if query != "" {
		if strings.TrimSpace(rule.SearchURL) == "" {
			return nil, false, errors.New("该站源不支持搜索")
		}
		headers = firstNonNilHeaders(ruleParseHeaders(rule.SearchHeaders), headers)
		address, postBody := xbpqSplitPost(rule.SearchURL)
		target = xbpqBuildURL(rule, referer, address, xbpqCategoryValue(rule, ""), query, page)
		scopeSpec = xbpqParseField(firstNonEmpty(rule.SearchSecondCut, rule.SecondCut))
		arraySpec = xbpqParseField(firstNonEmpty(rule.SearchArray, rule.Array))
		titleSpec = firstNonEmpty(rule.SearchTitle, rule.Title)
		picSpec = firstNonEmpty(rule.SearchPicture, rule.Picture)
		linkSpec = firstNonEmpty(rule.SearchLink, rule.Link)
		subtitleSpec = firstNonEmpty(rule.SearchSubtitle, rule.Subtitle)
		linkPrefix = firstNonEmpty(rule.SearchLinkPrefix, rule.LinkPrefix)
		linkSuffix = firstNonEmpty(rule.SearchLinkSuffix, rule.LinkSuffix)
		if postBody != "" {
			body, err := d.fetchXBPQPost(ctx, target, postBody, headers, referer, rule.Charset)
			if err != nil {
				return nil, false, fmt.Errorf("搜索失败：%w", publicError(err))
			}
			return xbpqDramasFromPage(site, rule, body, scopeSpec, arraySpec, titleSpec, picSpec, linkSpec, subtitleSpec, linkPrefix, linkSuffix, referer)
		}
	} else {
		if strings.TrimSpace(rule.CategoryURL) == "" {
			return nil, false, errors.New("该站源没有配置分类地址")
		}
		target = xbpqBuildURL(rule, referer, rule.CategoryURL, xbpqCategoryValue(rule, category), "", page)
		scopeSpec = xbpqParseField(rule.SecondCut)
		arraySpec = xbpqParseField(rule.Array)
		titleSpec, picSpec, linkSpec = rule.Title, rule.Picture, rule.Link
		subtitleSpec = rule.Subtitle
		linkPrefix, linkSuffix = rule.LinkPrefix, rule.LinkSuffix
	}
	if target == "" {
		return nil, false, errors.New("分类地址无效")
	}
	body, err := d.fetchXBPQPage(ctx, target, headers, referer, rule.Charset)
	if err != nil {
		return nil, false, fmt.Errorf("站点请求失败：%w", publicError(err))
	}
	if strings.TrimSpace(body) == "" {
		return nil, false, errors.New("站点返回空内容（可能触发反爬或需要浏览器环境）")
	}
	return xbpqDramasFromPage(site, rule, body, scopeSpec, arraySpec, titleSpec, picSpec, linkSpec, subtitleSpec, linkPrefix, linkSuffix, referer)
}

// xbpqDramasFromPage 页面文本 → 剧集列表（HTML 截取 / JSON 路径两种模式）。
//
// JSON 模式判定：页面本身能被 json.Unmarshal 解析且数组规则不含 `&&`
// （&& 是截取语法，出现在数组规则里一定不是 JSON 路径）。
func xbpqDramasFromPage(site libretvSite, rule xbpqRule, body string, scopeSpec, arraySpec xbpqField, titleSpec, picSpec, linkSpec, subtitleSpec, linkPrefix, linkSuffix, referer string) ([]Drama, bool, error) {
	scope := body
	// 只有真正配置了二次截取才收窄范围（空字段表示整页）。
	if len(scopeSpec.markers) > 0 && scopeSpec.markers[0] != "" {
		scope = xbpqCut(body, scopeSpec)
	}
	source := libretvSourceID(site.Key)
	if scope == "" {
		return nil, false, errors.New("页面结构变化，二次截取没有命中")
	}
	var dramas []Drama
	// JSON 模式：数组规则是 JSON 路径（不含 && 标记），且内容本身是 JSON。
	trimmedScope := strings.TrimSpace(scope)
	jsonLike := strings.HasPrefix(trimmedScope, "{") || strings.HasPrefix(trimmedScope, "[")
	if !strings.Contains(arraySpec.markers0(), "&&") && jsonLike {
		if items := xbpqJSONItems(scope, arraySpec.markers0()); items != nil {
			for _, item := range items {
				if drama, ok := xbpqJSONDrama(source, referer, item, titleSpec, picSpec, linkSpec, subtitleSpec, linkPrefix, linkSuffix); ok {
					dramas = append(dramas, drama)
				}
			}
			return dramas, len(dramas) > 0, nil
		}
	}
	// HTML 截取模式。
	items := xbpqCutAll(scope, arraySpec)
	for _, item := range items {
		title := xbpqCut(item, xbpqParseField(titleSpec))
		if title == "" {
			continue
		}
		picture := xbpqCut(item, xbpqParseField(picSpec))
		link := xbpqCut(item, xbpqParseField(linkSpec))
		subtitle := xbpqCut(item, xbpqParseField(subtitleSpec))
		link = ruleResolveURL(referer, strings.TrimSpace(linkPrefix)+link+strings.TrimSpace(linkSuffix))
		if link == "" {
			continue
		}
		sourceID := drpyEncodeSegment(link)
		dramas = append(dramas, Drama{
			ID:            providerDramaID(source, sourceID),
			Source:        source,
			SourceID:      sourceID,
			Title:         title,
			Name:          title,
			Cover:         ruleResolveURL(referer, picture),
			Remark:        subtitle,
			Episodes:      episodeIndex(subtitle, 0),
			ReleaseStatus: libretvRemarksStatus(subtitle),
		})
	}
	if len(dramas) == 0 {
		return nil, false, errors.New("没有从分类页解析到剧集，规则可能已失效")
	}
	// XBPQ 页面没有可靠的翻页信息：有数据就认为还有下一页（深度到上限为止）。
	return dramas, len(items) > 0, nil
}

// xbpqJSONItems 从 JSON 文本里按路径取数组；路径无效返回 nil。
func xbpqJSONItems(scope, path string) []any {
	var document any
	if json.Unmarshal([]byte(scope), &document) != nil {
		return nil
	}
	value := ruleJSONPath(document, path)
	if value == nil && path != "" {
		// 路径里没写 [] 时按数组本身处理（常见于「数组」只写父级键）。
		value = ruleJSONPath(document, path+"[]")
	}
	if items, ok := value.([]any); ok {
		return items
	}
	return nil
}

// xbpqJSONDrama JSON 模式的列表项 → Drama。
func xbpqJSONDrama(source, referer string, item any, titleSpec, picSpec, linkSpec, subtitleSpec, linkPrefix, linkSuffix string) (Drama, bool) {
	entry, ok := item.(map[string]any)
	if !ok {
		return Drama{}, false
	}
	field := func(path string) string { return ruleJSONText(ruleJSONPath(entry, path)) }
	title := field(titleSpec)
	if title == "" {
		return Drama{}, false
	}
	link := field(linkSpec)
	link = ruleResolveURL(referer, strings.TrimSpace(linkPrefix)+link+strings.TrimSpace(linkSuffix))
	if link == "" {
		return Drama{}, false
	}
	picture := field(picSpec)
	subtitle := field(subtitleSpec)
	sourceID := drpyEncodeSegment(link)
	return Drama{
		ID:            providerDramaID(source, sourceID),
		Source:        source,
		SourceID:      sourceID,
		Title:         title,
		Name:          title,
		Cover:         ruleResolveURL(referer, picture),
		Remark:        subtitle,
		Episodes:      episodeIndex(subtitle, 0),
		ReleaseStatus: libretvRemarksStatus(subtitle),
	}, true
}

// markers0 数组规则的原始标记串（JSON 路径模式下就是路径本身）。
func (field xbpqField) markers0() string {
	if len(field.markers) == 0 {
		return ""
	}
	return field.markers[0]
}

// fetchXBPQDetail 详情页：元数据 + 线路/播放列表。
//
// vodID 是 base64 后的详情地址（catalog 里由链接字段生成）；
// 详情页取不到播放列表时退化为「单集指向详情页」，交给播放链路的
// 嗅探兜底（很多站点的详情页本身就是播放页）。
func (d *Downloader) fetchXBPQDetail(ctx context.Context, site libretvSite, entry ruleEntry, vodID string) (Drama, []Chapter, error) {
	rule := xbpqParseRule(entry.Rule)
	headers, referer := xbpqContext(rule, site)
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
	body, err := d.fetchXBPQPage(ctx, detailURL, headers, referer, rule.Charset)
	if err != nil {
		return Drama{}, nil, fmt.Errorf("详情页请求失败：%w", publicError(err))
	}
	source := libretvSourceID(site.Key)
	title := strings.TrimSpace(xbpqCut(body, xbpqParseField("<title>&&</title>")))
	// 详情元数据（页面级字段）。字段截取到的常是带链接的 HTML 片段
	// （「导演：<a>名字</a>」），统一去标签后再进展示层。
	meta := func(spec string) string { return libretvHTMLText(xbpqCut(body, xbpqParseField(spec))) }
	remark := firstNonEmpty(meta(rule.Status), meta(rule.Year))
	drama := Drama{
		ID:            providerDramaID(source, vodID),
		Source:        source,
		SourceID:      vodID,
		Title:         title,
		Name:          title,
		TypeName:      meta(rule.TypeName),
		Remark:        remark,
		Score:         "",
		Actor:         meta(rule.Actor),
		Director:      meta(rule.Director),
		OnlineDate:    meta(rule.Year),
		Desc:          meta(rule.Content),
		ReleaseStatus: libretvRemarksStatus(remark),
	}
	lines := d.xbpqPlayLines(ctx, rule, site, body, detailURL, headers, referer)
	chapters := ruleChapters(source, vodID, detailURL, lines)
	if len(chapters) == 0 {
		// 兜底：详情页本身就是播放页的站点，给单集让播放链路嗅探。
		chapters = ruleChapters(source, vodID, detailURL, []ruleLine{{
			Title:    "默认",
			Episodes: []ruleEpisode{{Title: "第01集", URL: detailURL}},
		}})
	}
	return drama, chapters, nil
}

// xbpqPlayLines 抓播放线路与分集：优先走「跳转数组/跳转链接」
// （详情页只放跳转入口，播放列表在跳转后的播放页），否则在当前
// 详情页里按 线路/播放 字段截取。
func (d *Downloader) xbpqPlayLines(ctx context.Context, rule xbpqRule, site libretvSite, body, detailURL string, headers map[string]string, referer string) []ruleLine {
	// 跳转模式：详情页里有 N 个跳转入口，每个入口是一个播放页。
	if strings.TrimSpace(rule.JumpArray) != "" && strings.TrimSpace(rule.JumpLink) != "" {
		jumpScope := body
		if xbpqSpecSet(rule.JumpSecondCut) {
			jumpScope = xbpqCut(body, xbpqParseField(rule.JumpSecondCut))
		}
		links := xbpqCutAll(jumpScope, xbpqParseField(rule.JumpArray))
		var lines []ruleLine
		for index, item := range links {
			address := ruleResolveURL(detailURL, xbpqCut(item, xbpqParseField(rule.JumpLink)))
			if !isProviderHTTPMediaURL(address) {
				continue
			}
			pageBody, err := d.fetchXBPQPage(ctx, address, headers, referer, rule.Charset)
			if err != nil {
				continue
			}
			pageLines := xbpqPlayLinesFromPage(rule, pageBody, address)
			if len(pageLines) == 0 {
				pageLines = []ruleLine{{Title: fmt.Sprintf("线路%d", index+1), Episodes: []ruleEpisode{{Title: "第01集", URL: address}}}}
			}
			for lineIndex := range pageLines {
				if strings.TrimSpace(pageLines[lineIndex].Title) == "" {
					pageLines[lineIndex].Title = fmt.Sprintf("线路%d", index+1)
				}
			}
			lines = append(lines, pageLines...)
		}
		return lines
	}
	return xbpqPlayLinesFromPage(rule, body, detailURL)
}

// xbpqPlayLinesFromPage 单个页面里的线路/分集截取。
//
// 结构约定：线路数组 给出每条线路的块（可选），播放数组 给出每条线路的
// 播放列表块，播放列表 在块内切分每一集，播放标题/播放链接 在集内取值。
// 线路数组缺省时按「单线路」处理；播放数组缺省时整页当一个块。
func xbpqPlayLinesFromPage(rule xbpqRule, body, pageURL string) []ruleLine {
	scope := body
	if spec := firstNonEmpty(rule.PlaySecondCut, rule.LineSecondCut); xbpqSpecSet(spec) {
		scope = xbpqCut(body, xbpqParseField(spec))
	}
	if scope == "" {
		return nil
	}
	lineTitles := xbpqCutAll(scope, xbpqParseField(rule.LineArray))
	playBlocks := xbpqCutAll(scope, xbpqParseField(rule.PlayArray))
	if len(playBlocks) == 0 {
		// 播放数组没写：整页当一个播放块（不少简单站点如此）。
		playBlocks = []string{scope}
	}
	var lines []ruleLine
	for index, block := range playBlocks {
		line := ruleLine{}
		if index < len(lineTitles) {
			line.Title = xbpqCut(lineTitles[index], xbpqParseField(rule.LineTitle))
		}
		line.Episodes = xbpqEpisodesFromBlock(rule, block, pageURL)
		if len(line.Episodes) == 0 {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// xbpqEpisodesFromBlock 单个播放块 → 分集列表。
func xbpqEpisodesFromBlock(rule xbpqRule, block, pageURL string) []ruleEpisode {
	var episodes []ruleEpisode
	for _, item := range xbpqCutAll(block, xbpqParseField(rule.PlayList)) {
		title := xbpqCut(item, xbpqParseField(rule.PlayTitle))
		address := xbpqCut(item, xbpqParseField(rule.PlayLink))
		address = ruleResolveURL(pageURL, strings.TrimSpace(rule.PlayLinkPrefix)+address+strings.TrimSpace(rule.PlayLinkSuffix))
		if title == "" && address == "" {
			continue
		}
		episodes = append(episodes, ruleEpisode{Title: title, URL: address})
	}
	return episodes
}

// xbpqSplitPost 搜索地址的 POST 约定：`地址;post;键=值&键2=值2`。
// 返回 (地址, POST 体)；没有 ;post; 时 POST 体为空（GET）。
func xbpqSplitPost(spec string) (address, body string) {
	spec = strings.TrimSpace(spec)
	if index := strings.Index(spec, ";post;"); index >= 0 {
		return strings.TrimSpace(spec[:index]), strings.TrimSpace(spec[index+len(";post;"):])
	}
	return spec, ""
}

// fetchXBPQPost 发送 POST 搜索请求（表单体，{wd} 已由调用方替换）。
// 与 GET 路径一致地做站点编码转换（gb2312 站点搜索结果是 GBK，不转会乱码）。
func (d *Downloader) fetchXBPQPost(ctx context.Context, target, postBody string, headers map[string]string, referer, charset string) (string, error) {
	body, err := rulePostText(ctx, d, target, postBody, headers, referer)
	if err != nil {
		return "", err
	}
	return ruleDecodeGBK(body, charset), nil
}

// firstNonNilHeaders 优先用 primary，空时回落 fallback。
func firstNonNilHeaders(primary, fallback map[string]string) map[string]string {
	if len(primary) > 0 {
		return primary
	}
	return fallback
}
