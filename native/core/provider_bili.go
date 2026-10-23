package core

// Bili（csp_Bili）规则引擎：B 站公开接口的 Go 实现。
//
// 背景：csp_Bili 的规则文件本身几乎不含抓取逻辑——只有分类（classes）
// 与筛选（filter/filters）配置，数据全靠重实现 B 站公开 API：
//
//   - 分类浏览 / 搜索：/x/web-interface/wbi/search/type（按分类关键词搜视频）；
//   - 详情：/x/web-interface/view（分 P 列表即分集）；
//   - 播放：节目页 URL 交给播放链路（WebView 嗅探拿真实媒体流，
//     直链 API 需要登录态与 fnval 协商，纯匿名场景下不稳定）。
//
// 签名：B 站 wbi 接口要求 wts + w_rid（md5(query + mixinKey)），
// mixinKey 由 nav 接口返回的 img_key/sub_key 按固定表重排而来（下方
// wbiMixinKeyEncTab）。风控（HTTP 412 / code -352）时给明确提示，
// 不静默失败——这是 B 站接口的已知限制，用户换网络或稍后再试即可。
//
// 规则文件有两种历史键名：classes/filter（FongMi 官方）与
// class/filters（社区派生），解析时两者都认。

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// biliRule Bili 规则（分类 + 可选筛选 + 可选 cookie）。
type biliRule struct {
	Cookie  string
	Classes []nativeCategory // ID 为真实 type_id，抓取时再映射为序号
}

// biliParseRule 规则 JSON → 结构体（兼容 classes/class 两种键名）。
func biliParseRule(rule map[string]any) biliRule {
	out := biliRule{Cookie: ruleText(rule, "cookie")}
	for _, key := range []string{"classes", "class"} {
		items, ok := rule[key].([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name := ruleText(entry, "type_name")
			value := ruleText(entry, "type_id")
			if name == "" || value == "" {
				continue
			}
			out.Classes = append(out.Classes, nativeCategory{ID: value, Name: name})
		}
		if len(out.Classes) > 0 {
			break
		}
	}
	return out
}

// fetchBiliCategories 分类直出（零网络）：ID 用有序号，真实 type_id 见
// biliCategoryValue（与 XBPQ / 海阔同一策略，保证分类 ID 可安全传输）。
func (d *Downloader) fetchBiliCategories(site libretvSite, entry ruleEntry) ([]nativeCategory, error) {
	rule := biliParseRule(entry.Rule)
	out := make([]nativeCategory, 0, len(rule.Classes))
	for index, item := range rule.Classes {
		out = append(out, nativeCategory{ID: strconv.Itoa(index + 1), Name: item.Name})
	}
	return out, nil
}

// biliCategoryValue 分类 ID（序号）→ 真实 type_id（B 站里一般是搜索关键词）。
func biliCategoryValue(rule biliRule, category string) string {
	position := ruleAtoi(category)
	if position >= 1 && position <= len(rule.Classes) {
		return rule.Classes[position-1].ID
	}
	if len(rule.Classes) > 0 {
		return rule.Classes[0].ID
	}
	return strings.TrimSpace(category)
}

// biliMixinKeyEncTab 是 B 站 wbi 的固定重排表（官方前端同款）。
var biliMixinKeyEncTab = []int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49,
	33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13, 37, 48, 7, 16, 24, 55, 40, 61,
	26, 17, 0, 1, 60, 51, 30, 4, 22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11, 36,
	20, 34, 44, 52,
}

// wbiMixinKey img_key + sub_key → 32 位 mixinKey。
func wbiMixinKey(imgKey, subKey string) string {
	raw := imgKey + subKey
	out := make([]byte, 0, 32)
	for _, index := range biliMixinKeyEncTab {
		if index < len(raw) {
			out = append(out, raw[index])
			if len(out) == 32 {
				break
			}
		}
	}
	return string(out)
}

// wbiSign 给参数加 wts + w_rid 签名（query 按 key 排序后拼接）。
func wbiSign(params url.Values, mixinKey string) {
	params.Set("wts", strconv.FormatInt(time.Now().Unix(), 10))
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for index, key := range keys {
		if index > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(url.QueryEscape(key))
		builder.WriteByte('=')
		builder.WriteString(url.QueryEscape(params.Get(key)))
	}
	sum := md5.Sum([]byte(builder.String() + mixinKey))
	params.Set("w_rid", hex.EncodeToString(sum[:]))
}

// biliHeaders B 站接口请求头（UA + Referer + 可选 Cookie）。
func biliHeaders(rule biliRule) map[string]string {
	headers := map[string]string{
		"User-Agent": userAgent,
		"Referer":    "https://www.bilibili.com/",
		"Origin":     "https://www.bilibili.com",
	}
	// clan:// 是 TVBox 的本地文件引用，纯 Go 端读不到，按匿名处理。
	if cookie := strings.TrimSpace(rule.Cookie); cookie != "" && !strings.HasPrefix(cookie, "clan://") {
		headers["Cookie"] = cookie
	}
	return headers
}

// biliGetJSON 请求 B 站接口并解析 JSON；风控时返回可读错误。
func (d *Downloader) biliGetJSON(ctx context.Context, target string, headers map[string]string) (map[string]any, error) {
	status, body, err := ruleGetWithHeaders(ctx, d, target, headers)
	if err != nil {
		return nil, err
	}
	if status == 412 {
		return nil, errors.New("B站接口触发风控（412），请稍后再试或更换网络环境")
	}
	var payload map[string]any
	if json.Unmarshal([]byte(body), &payload) != nil {
		return nil, errors.New("B站接口返回格式异常，可能已改版")
	}
	if codeText := ruleJSONText(payload["code"]); codeText != "" && ruleAtoi(codeText) != 0 {
		if message := ruleJSONText(payload["message"]); message != "" {
			return nil, fmt.Errorf("B站接口返回错误：%s", message)
		}
		return nil, errors.New("B站接口返回错误")
	}
	return payload, nil
}

// biliWBIKeys 取 wbi 签名密钥（nav 接口未登录也能拿到 img/sub key）。
func (d *Downloader) biliWBIKeys(ctx context.Context, headers map[string]string) (string, error) {
	payload, err := d.biliGetJSON(ctx, "https://api.bilibili.com/x/web-interface/nav", headers)
	if err != nil {
		return "", err
	}
	data, _ := payload["data"].(map[string]any)
	img, _ := data["wbi_img"].(map[string]any)
	imgURL := ruleJSONText(img["img_url"])
	subURL := ruleJSONText(img["sub_url"])
	if imgURL == "" || subURL == "" {
		return "", errors.New("B站 wbi 密钥获取失败")
	}
	key := func(raw string) string {
		base := raw
		if slash := strings.LastIndex(base, "/"); slash >= 0 {
			base = base[slash+1:]
		}
		return strings.TrimSuffix(base, ".png")
	}
	return wbiMixinKey(key(imgURL), key(subURL)), nil
}

// fetchBiliCatalogPage 分类浏览 / 搜索：都走 search/type（按关键词搜视频）。
func (d *Downloader) fetchBiliCatalogPage(ctx context.Context, site libretvSite, entry ruleEntry, page int, category, query string) ([]Drama, bool, error) {
	rule := biliParseRule(entry.Rule)
	if page < 1 {
		page = 1
	}
	keyword := strings.TrimSpace(query)
	if keyword == "" {
		keyword = biliCategoryValue(rule, category)
	}
	if keyword == "" {
		return nil, false, errors.New("该站源没有可用的分类关键词")
	}
	headers := biliHeaders(rule)
	mixinKey, err := d.biliWBIKeys(ctx, headers)
	if err != nil {
		return nil, false, err
	}
	params := url.Values{}
	params.Set("search_type", "video")
	params.Set("keyword", keyword)
	params.Set("page", strconv.Itoa(page))
	params.Set("page_size", "20")
	params.Set("order", "totalrank")
	wbiSign(params, mixinKey)
	payload, err := d.biliGetJSON(ctx, "https://api.bilibili.com/x/web-interface/wbi/search/type?"+params.Encode(), headers)
	if err != nil {
		return nil, false, err
	}
	data, _ := payload["data"].(map[string]any)
	rows, _ := data["result"].([]any)
	source := libretvSourceID(site.Key)
	var dramas []Drama
	for _, row := range rows {
		item, ok := row.(map[string]any)
		if !ok {
			continue
		}
		bvid := ruleJSONText(item["bvid"])
		title := biliCleanTitle(ruleJSONText(item["title"]))
		if bvid == "" || title == "" {
			continue
		}
		cover := ruleJSONText(item["pic"])
		if strings.HasPrefix(cover, "//") {
			cover = "https:" + cover
		}
		remark := firstNonEmpty(ruleJSONText(item["duration"]), ruleJSONText(item["author"]))
		sourceID := drpyEncodeSegment("https://www.bilibili.com/video/" + bvid)
		dramas = append(dramas, Drama{
			ID: providerDramaID(source, sourceID), Source: source, SourceID: sourceID,
			Title: title, Name: title,
			Cover:         cover,
			Remark:        remark,
			Desc:          ruleJSONText(item["description"]),
			Episodes:      episodeIndex(remark, 0),
			ReleaseStatus: libretvRemarksStatus(remark),
		})
	}
	return dramas, len(dramas) > 0, nil
}

// biliCleanTitle 去掉搜索结果的 <em class="keyword"> 高亮标签。
func biliCleanTitle(raw string) string {
	text := strings.TrimSpace(raw)
	for {
		start := strings.Index(text, "<")
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], ">")
		if end < 0 {
			break
		}
		text = text[:start] + text[start+end+1:]
	}
	return strings.TrimSpace(libretvHTMLText(text))
}

// fetchBiliDetail 详情：view 接口拿分 P 列表，分集地址指向节目页
// （?p=N），播放交给 WebView 嗅探。
func (d *Downloader) fetchBiliDetail(ctx context.Context, site libretvSite, entry ruleEntry, vodID string) (Drama, []Chapter, error) {
	rule := biliParseRule(entry.Rule)
	pageURL, decoded := drpyDecodeSegment(vodID)
	if !decoded {
		pageURL = ""
	}
	bvid := ""
	if index := strings.LastIndex(pageURL, "/video/"); index >= 0 {
		bvid = strings.TrimPrefix(pageURL[index+len("/video/"):], "/")
	}
	if bvid == "" {
		return Drama{}, nil, errors.New("B站视频 ID 无效，请刷新列表后重试")
	}
	headers := biliHeaders(rule)
	payload, err := d.biliGetJSON(ctx, "https://api.bilibili.com/x/web-interface/view?bvid="+url.QueryEscape(bvid), headers)
	if err != nil {
		return Drama{}, nil, err
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return Drama{}, nil, errors.New("B站没有返回该视频的详情")
	}
	source := libretvSourceID(site.Key)
	title := ruleJSONText(data["title"])
	cover := ruleJSONText(data["pic"])
	if strings.HasPrefix(cover, "//") {
		cover = "https:" + cover
	}
	drama := Drama{
		ID: providerDramaID(source, vodID), Source: source, SourceID: vodID,
		Title: title, Name: title, Cover: cover,
		Desc:     ruleJSONText(data["desc"]),
		TypeName: "B站",
	}
	pages, _ := data["pages"].([]any)
	var episodes []ruleEpisode
	for index, row := range pages {
		page, ok := row.(map[string]any)
		if !ok {
			continue
		}
		number := pageNumber(page["page"], index+1)
		part := ruleJSONText(page["part"])
		if part == "" {
			part = fmt.Sprintf("P%d", number)
		}
		episodes = append(episodes, ruleEpisode{
			Title: fmt.Sprintf("P%d %s", number, part),
			URL:   fmt.Sprintf("https://www.bilibili.com/video/%s?p=%d", bvid, number),
		})
	}
	if len(episodes) == 0 {
		episodes = []ruleEpisode{{Title: "P1", URL: fmt.Sprintf("https://www.bilibili.com/video/%s", bvid)}}
	}
	drama.Episodes = len(episodes)
	return drama, ruleChapters(source, vodID, "https://www.bilibili.com/", []ruleLine{{Title: "默认", Episodes: episodes}}), nil
}

// pageNumber JSON 数字 → 分 P 序号。
func pageNumber(value any, fallback int) int {
	number := ruleAtoi(ruleJSONText(value))
	if number <= 0 {
		return fallback
	}
	return number
}
