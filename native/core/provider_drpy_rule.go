package core

// rule 驱动型 drpy2 脚本（无 home / category / detail / search 函数）。
//
// TVBox 生态里大量脚本只写 rule 配置，靠运行时内置的默认函数工作：
//
//	var rule = {
//	    host: 'http://api.example.com',
//	    homeUrl: '/api/tag/hand?...',
//	    url: '/api/crumb/list?page=fypage&type=0&fyfilter',
//	    detailUrl: '/api/node/detail?id=fyid',
//	    searchUrl: '/api/video/search?key=**&page=fypage',
//	    class_name: '全部&电影', class_url: '0&1',
//	    推荐: `js: ... setResult(d)`,
//	    一级: `js: ... setResult(d)`,
//	    二级: `js: ... VOD = {...}`,
//	    搜索: `js: ... setResult(d)`,
//	}
//
// 这里实现这套默认函数的最小可用版本：模板替换（fyclass / fypage /
// fyid / fyfilter / **）+ 片段执行（input / MY_PAGE / HOST / MY_URL /
// cateObj / fl 变量，同步 request()、setResult()、VOD）。片段里的
// request 是**同步**的（drpy2 的 Nashorn 语义），与脚本函数用的异步 req 不同。

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/dop251/goja"
)

// drpyRuleSpec rule 里与浏览链路相关的静态配置。
type drpyRuleSpec struct {
	Host      string
	HomeURL   string
	URL       string
	DetailURL string
	SearchURL string
	ClassName string
	ClassURL  string
	First     string // 一级
	Second    string // 二级
	Search    string // 搜索
	Recommend string // 推荐
	FilterURL string
	FilterDef map[string]any
	Headers   map[string]string
	PlayParse bool
	Lazy      string
}

// drpyRuleFilterPattern filter_url 里的 {{fl.area or "0"}}。
var drpyRuleFilterPattern = regexp.MustCompile(`\{\{\s*fl\.([A-Za-z0-9_]+)\s+or\s+"([^"]*)"\s*\}\}`)

// drpyRuleFilterBarePattern 不带默认值的 {{fl.area}}。
var drpyRuleFilterBarePattern = regexp.MustCompile(`\{\{\s*fl\.([A-Za-z0-9_]+)\s*\}\}`)

// drpyHasFunction 脚本是否定义了可调用的同名函数。
func drpyHasFunction(vm *goja.Runtime, name string) bool {
	value := vm.Get(name)
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return false
	}
	_, ok := goja.AssertFunction(value)
	return ok
}

// drpyReadRule 从 VM 里读 rule 对象；脚本没写 rule 时返回 false。
func drpyReadRule(vm *goja.Runtime) (drpyRuleSpec, bool) {
	value := vm.Get("rule")
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return drpyRuleSpec{}, false
	}
	exported, ok := value.Export().(map[string]any)
	if !ok {
		return drpyRuleSpec{}, false
	}
	spec := drpyRuleSpec{
		Host:      drpyRuleString(exported["host"]),
		HomeURL:   drpyRuleString(exported["homeUrl"]),
		URL:       drpyRuleString(exported["url"]),
		DetailURL: drpyRuleString(exported["detailUrl"]),
		SearchURL: drpyRuleString(exported["searchUrl"]),
		ClassName: drpyRuleString(exported["class_name"]),
		ClassURL:  drpyRuleString(exported["class_url"]),
		First:     drpyRuleString(exported["一级"]),
		Second:    drpyRuleString(exported["二级"]),
		Search:    drpyRuleString(exported["搜索"]),
		Recommend: drpyRuleString(exported["推荐"]),
		FilterURL: drpyRuleString(exported["filter_url"]),
		Lazy:      drpyRuleString(exported["lazy"]),
	}
	if headers, ok := exported["headers"].(map[string]any); ok {
		spec.Headers = map[string]string{}
		for key, item := range headers {
			spec.Headers[key] = fmt.Sprint(item)
		}
	}
	if filters, ok := exported["filter_def"].(map[string]any); ok {
		spec.FilterDef = filters
	}
	if flag, ok := exported["play_parse"].(bool); ok {
		spec.PlayParse = flag
	}
	return spec, true
}

// drpyRuleString 取 rule 字段的字符串值（数字/布尔也能用）。
func drpyRuleString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case bool:
		if typed {
			return "true"
		}
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

// drpyRuleCategories 用 class_name / class_url 生成分类列表。
func drpyRuleCategories(spec drpyRuleSpec) []nativeCategory {
	names := strings.Split(spec.ClassName, "&")
	ids := strings.Split(spec.ClassURL, "&")
	categories := make([]nativeCategory, 0, len(names))
	for index, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		id := strconv.Itoa(index)
		if index < len(ids) {
			if value := strings.TrimSpace(ids[index]); value != "" {
				id = value
			}
		}
		categories = append(categories, nativeCategory{ID: id, Name: name})
	}
	return categories
}

// drpyRuleFilter 用 filter_url + filter_def 拼筛选串（未提供时为空）。
func drpyRuleFilter(spec drpyRuleSpec, tid string) string {
	if spec.FilterURL == "" {
		return ""
	}
	defaults := map[string]string{}
	if raw, ok := spec.FilterDef[tid].(map[string]any); ok {
		for key, value := range raw {
			defaults[key] = fmt.Sprint(value)
		}
	} else if raw, ok := spec.FilterDef[tid].(map[string]string); ok {
		for key, value := range raw {
			defaults[key] = value
		}
	}
	filtered := drpyRuleFilterPattern.ReplaceAllStringFunc(spec.FilterURL, func(match string) string {
		parts := drpyRuleFilterPattern.FindStringSubmatch(match)
		if len(parts) < 3 {
			return ""
		}
		if value, ok := defaults[parts[1]]; ok && value != "" {
			return value
		}
		return parts[2]
	})
	filtered = drpyRuleFilterBarePattern.ReplaceAllStringFunc(filtered, func(match string) string {
		parts := drpyRuleFilterBarePattern.FindStringSubmatch(match)
		if len(parts) < 2 {
			return ""
		}
		return defaults[parts[1]]
	})
	return filtered
}

// drpyRuleURL 把 rule 里的路径模板补全成完整地址。
func drpyRuleURL(spec drpyRuleSpec, template, tid, episodeID, keyword string, page int) string {
	address := strings.TrimSpace(template)
	if address == "" {
		return ""
	}
	if tid != "" {
		address = strings.ReplaceAll(address, "fyclass", tid)
	}
	if episodeID != "" {
		address = strings.ReplaceAll(address, "fyid", episodeID)
	}
	if keyword != "" {
		address = strings.ReplaceAll(address, "**", url.QueryEscape(keyword))
	}
	address = strings.ReplaceAll(address, "fypage", strconv.Itoa(page))
	address = strings.ReplaceAll(address, "fyfilter", drpyRuleFilter(spec, tid))
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return address
	}
	host := strings.TrimRight(strings.TrimSpace(spec.Host), "/")
	if host == "" {
		return address
	}
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "http://" + host
	}
	if !strings.HasPrefix(address, "/") {
		address = "/" + address
	}
	return host + address
}

// drpyRuleInstallRequest 给片段装同步 request（drpy2 的 Nashorn 语义：
// 直接返回响应正文，不是 Promise；rule.headers 作为默认请求头）。
func drpyRuleInstallRequest(vm *goja.Runtime, host *drpyHost, defaults map[string]string) {
	vm.Set("request", func(call goja.FunctionCall) goja.Value {
		target := strings.TrimSpace(call.Argument(0).String())
		if target == "" {
			return vm.ToValue("")
		}
		options := drpyExportOptions(call.Argument(1))
		if options == nil {
			options = map[string]any{}
		}
		headers := map[string]any{}
		for key, value := range defaults {
			headers[key] = value
		}
		if custom, ok := options["headers"].(map[string]any); ok {
			for key, value := range custom {
				headers[key] = value
			}
		}
		if len(headers) > 0 {
			options["headers"] = headers
		}
		response, err := host.fetch(target, options)
		if err != nil {
			return vm.ToValue("")
		}
		return vm.ToValue(response.Content)
	})
}

// drpyRuleRunSnippet 执行 `js:` 片段：包一层 IIFE，避免同一 VM 里多次
// 执行时 let/var 重复声明；片段用 setResult() 交回列表，详情写 VOD。
func drpyRuleRunSnippet(vm *goja.Runtime, code string, vars map[string]any) (any, error) {
	code = strings.TrimSpace(code)
	if strings.HasPrefix(code, "js:") {
		code = strings.TrimSpace(strings.TrimPrefix(code, "js:"))
	}
	if code == "" {
		return nil, errors.New("该操作在脚本里没有对应的 rule 片段")
	}
	if err := vm.Set("__drpyRuleResult", goja.Undefined()); err != nil {
		return nil, err
	}
	for name, value := range vars {
		if err := vm.Set(name, value); err != nil {
			return nil, err
		}
	}
	wrapped := "(function(){\n" + code + "\n})();"
	if _, err := vm.RunString(wrapped); err != nil {
		message := err.Error()
		// 片段里最常见的失败是 JSON.parse 收到网页（接口挂了 / 被拦截），
		// 单独点明一下，避免只看到「invalid character '<'」。
		if strings.Contains(message, "invalid character '<'") ||
			strings.Contains(message, "Unexpected token '<'") {
			return nil, errors.New("接口返回的是网页而不是 JSON（源接口可能已失效或被拦截）")
		}
		return nil, drpyWrapError(err)
	}
	value := vm.Get("__drpyRuleResult")
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return nil, nil
	}
	return value.Export(), nil
}

// drpyRuleVars 片段可见的上下文字变量（与 drpy2 一致）。
func drpyRuleVars(spec drpyRuleSpec, input string, page int, tid, name, episodeID string) map[string]any {
	cate := map[string]any{"tid": tid, "name": name, "id": tid}
	return map[string]any{
		"input":   input,
		"MY_PAGE": page,
		"MY_URL":  input,
		"HOST":    strings.TrimRight(spec.Host, "/"),
		"MY_HOME": drpyRuleURL(spec, spec.HomeURL, tid, episodeID, "", page),
		"cateObj": cate,
		"fl":      map[string]any{},
		"KEY":     episodeID,
	}
}

// drpyRuleDrama 片段返回的列表项 {title,img,desc,url} → Drama。
func drpyRuleDrama(source string, item map[string]any) (Drama, bool) {
	title := drpyFirstString(item, "title", "name", "vod_name")
	identifier := drpyFirstString(item, "url", "id", "vod_id")
	if title == "" || identifier == "" {
		return Drama{}, false
	}
	cover := drpyFirstString(item, "img", "pic", "vod_pic")
	desc := drpyFirstString(item, "desc", "remark", "vod_remarks")
	return Drama{
		ID:            providerDramaID(source, drpyEncodeSegment(identifier)),
		Source:        source,
		SourceID:      drpyEncodeSegment(identifier),
		Title:         title,
		Name:          title,
		Cover:         cover,
		Remark:        desc,
		Episodes:      episodeIndex(desc, 0),
		ReleaseStatus: libretvRemarksStatus(desc),
	}, true
}

// drpyFirstString 取多个候选键里的第一个非空字符串。
func drpyFirstString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := item[key]; ok && value != nil {
			if text := strings.TrimSpace(fmt.Sprint(value)); text != "" {
				return text
			}
		}
	}
	return ""
}

// drpyRuleList 把片段结果（[]any）转成剧集列表。
func drpyRuleList(source string, raw any) []Drama {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	dramas := make([]Drama, 0, len(items))
	for _, entry := range items {
		item, valid := entry.(map[string]any)
		if !valid {
			continue
		}
		if drama, ok := drpyRuleDrama(source, item); ok {
			dramas = append(dramas, drama)
		}
	}
	return dramas
}

// drpyRuleCategory 无 category 函数时：模板 URL + 一级片段。
func drpyRuleCategory(
	vm *goja.Runtime,
	site drpySite,
	spec drpyRuleSpec,
	tid string,
	page int,
) ([]Drama, bool, error) {
	template := spec.URL
	if template == "" {
		template = spec.HomeURL
	}
	if strings.TrimSpace(template) == "" {
		return nil, false, errors.New("脚本没有 category 函数，rule 里也没有 url 模板")
	}
	input := drpyRuleURL(spec, template, tid, "", "", page)
	code := spec.First
	if strings.TrimSpace(code) == "" {
		return nil, false, fmt.Errorf("脚本没有 category 函数，rule 里也缺少「一级」解析规则（%s）", input)
	}
	raw, err := drpyRuleRunSnippet(vm, code, drpyRuleVars(spec, input, page, tid, "", ""))
	if err != nil {
		return nil, false, fmt.Errorf("一级解析失败（%s）：%w", drpyShortURL(input), err)
	}
	dramas := drpyRuleList(drpySourceID(site.Key), raw)
	// 片段结果没有总页数，用「本页是否有内容」判断还能不能翻页。
	return dramas, len(dramas) > 0, nil
}

// drpyRuleSearch 无 search 函数时：searchUrl 模板 + 搜索片段。
func drpyRuleSearch(
	vm *goja.Runtime,
	site drpySite,
	spec drpyRuleSpec,
	keyword string,
	page int,
) ([]Drama, bool, error) {
	if strings.TrimSpace(spec.SearchURL) == "" || strings.TrimSpace(spec.Search) == "" {
		return nil, false, errors.New("该脚本不支持搜索（没有 search 函数，rule 里也缺少 searchUrl / 搜索）")
	}
	input := drpyRuleURL(spec, spec.SearchURL, "", "", keyword, page)
	raw, err := drpyRuleRunSnippet(vm, spec.Search, drpyRuleVars(spec, input, page, "", "", ""))
	if err != nil {
		return nil, false, fmt.Errorf("搜索解析失败（%s）：%w", drpyShortURL(input), err)
	}
	dramas := drpyRuleList(drpySourceID(site.Key), raw)
	return dramas, len(dramas) > 0, nil
}

// drpyRuleDetail 无 detail 函数时：detailUrl 模板 + 二级片段（写 VOD）。
func drpyRuleDetail(
	vm *goja.Runtime,
	site drpySite,
	spec drpyRuleSpec,
	vodID string,
) (Drama, []Chapter, error) {
	if strings.TrimSpace(spec.DetailURL) == "" || strings.TrimSpace(spec.Second) == "" {
		return Drama{}, nil, errors.New("脚本没有 detail 函数，rule 里也缺少 detailUrl / 二级")
	}
	input := drpyRuleURL(spec, spec.DetailURL, "", vodID, "", 1)
	if _, err := drpyRuleRunSnippet(vm, spec.Second, drpyRuleVars(spec, input, 1, "", "", vodID)); err != nil {
		return Drama{}, nil, fmt.Errorf("二级解析失败（%s）：%w", drpyShortURL(input), err)
	}
	value := vm.Get("VOD")
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return Drama{}, nil, errors.New("二级解析没有返回剧集信息")
	}
	vod, ok := value.Export().(map[string]any)
	if !ok {
		return Drama{}, nil, errors.New("二级解析返回的 VOD 不是对象")
	}
	source := drpySourceID(site.Key)
	row := macCMSListRow{
		ID:       drpyFirstString(vod, "vod_id", "id"),
		Name:     drpyFirstString(vod, "vod_name", "name", "title"),
		Pic:      drpyFirstString(vod, "vod_pic", "pic"),
		TypeName: drpyFirstString(vod, "type_name", "type"),
		Remarks:  drpyFirstString(vod, "vod_remarks", "remarks"),
		Year:     drpyFirstString(vod, "vod_year", "year"),
		Area:     drpyFirstString(vod, "vod_area", "area"),
		Lang:     drpyFirstString(vod, "vod_lang", "lang"),
		Score:    drpyFirstString(vod, "vod_score", "score"),
		Actor:    drpyFirstString(vod, "vod_actor", "actor"),
		Director: drpyFirstString(vod, "vod_director", "director"),
	}
	if row.ID == "" {
		row.ID = vodID
	}
	if row.Name == "" {
		return Drama{}, nil, errors.New("二级解析没有返回片名")
	}
	drama := drpyDramaFromRow(source, row, drpyFirstString(vod, "vod_content", "content", "desc"))
	playFrom := strings.Split(drpyFirstString(vod, "vod_play_from"), "$$$")
	playGroups := strings.Split(drpyFirstString(vod, "vod_play_url"), "$$$")
	chapters := drpyChaptersFromGroups(source, row.ID, drpySiteReferer(site), playFrom, playGroups)
	return drama, chapters, nil
}

// drpyShortURL 错误文案里的地址简称（drpyWrapError 会在 200 字处截断，
// 太长会把真正的原因挤掉）。
func drpyShortURL(raw string) string {
	text := strings.TrimSpace(raw)
	text = strings.TrimPrefix(text, "https://")
	text = strings.TrimPrefix(text, "http://")
	if len([]rune(text)) > 60 {
		text = string([]rune(text)[:60]) + "…"
	}
	return text
}
