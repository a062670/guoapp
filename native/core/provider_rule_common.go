package core

// 规则引擎公共辅助：XBPQ / 海阔 / Bili 三个引擎共用的
// JSON 路径取值、请求头解析（UA 宏）、分集整理。
//
// 这些函数只处理「规则语义」，不碰网络与存储——网络统一走
// fetchProviderText（超时 / 重试 / 镜像候选），存储见 rules store。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// ruleUA 请求头 UA 宏展开。
//
// TVBox 规则里 UA 不写全串，写宏：MOBILE_UA / 手机 / mobile 是移动端，
// PC_UA / 电脑 / pc / 空 是桌面端。移动端网页往往是精简版（带列表数据），
// 桌面版可能缺模板——分享农民规则就明确写了 User-Agent$MOBILE_UA，
// 不带对应 UA 时服务端会返回「缺少文件」的错误页。
func ruleUA(macro string) string {
	switch strings.ToLower(strings.TrimSpace(macro)) {
	case "mobile_ua", "mobile", "手机", "移动", "android":
		return ruleMobileUA
	case "", "pc_ua", "pc", "电脑", "桌面":
		return userAgent
	default:
		// 已经是完整 UA 串时原样使用。
		return strings.TrimSpace(macro)
	}
}

// ruleMobileUA 移动端 UA（与规则生态里的 MOBILE_UA 保持同一形态）。
const ruleMobileUA = "Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"

// ruleParseHeaders 解析规则里的请求头字段。
//
// 格式（XBPQ / 海阔通用）：Key$Value#Key2$Value2，
// 例如 "User-Agent$手机#Referer$https://a.com/"。
// 单独的 "手机" / "MOBILE_UA" 视为 User-Agent。返回键值对（键已规范化）。
func ruleParseHeaders(spec string) map[string]string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	headers := map[string]string{}
	for _, pair := range strings.Split(spec, "#") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, found := strings.Cut(pair, "$")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !found {
			// 单个值：按 UA 宏处理（"手机" / "PC_UA"）。
			headers["User-Agent"] = ruleUA(pair)
			continue
		}
		if key == "" {
			continue
		}
		if strings.EqualFold(key, "User-Agent") || strings.EqualFold(key, "ua") {
			headers["User-Agent"] = ruleUA(value)
			continue
		}
		headers[key] = value
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// ruleJSONPath 按 JSON 路径取值（XBPQ / 海阔的 JSON 模式共用）。
//
// 语法（与 XBPQ 文档一致）：
//   - 段用 . 分隔：data.list.name；
//   - 段尾 [n] 取数组第 n 个元素（n 从 1 起）：list[1].name；
//   - 段尾 [] 表示整个数组：list[]；
//   - 段尾 [n,] 表示从第 n 个取到末尾：list[3,]；
//   - 段首没有键名时（如 [1]）直接在当前节点上取下标。
//
// 取不到时返回 nil（调用方按空值处理，不报错）。
func ruleJSONPath(data any, path string) any {
	path = strings.TrimSpace(path)
	if path == "" {
		return data
	}
	current := data
	for _, segment := range strings.Split(path, ".") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		// 拆出键名与下标后缀（键名本身可能含 []，如 "list[1]"）。
		key, index := segment, ""
		if open := strings.Index(segment, "["); open >= 0 && strings.HasSuffix(segment, "]") {
			key = strings.TrimSpace(segment[:open])
			index = strings.TrimSpace(segment[open+1 : len(segment)-1])
		}
		if key != "" {
			object, ok := current.(map[string]any)
			if !ok {
				return nil
			}
			current, ok = object[key]
			if !ok {
				return nil
			}
		}
		if index == "" && !strings.Contains(segment, "[") {
			continue
		}
		list, ok := current.([]any)
		if !ok {
			return nil
		}
		switch {
		case index == "":
			// list[]：整个数组。
			current = list
		case strings.HasSuffix(index, ","):
			// list[n,]：从第 n 个开始。
			start := ruleAtoi(strings.TrimSuffix(index, ","))
			if start < 1 || start > len(list) {
				return nil
			}
			current = list[start-1:]
		default:
			// list[n]：第 n 个（1 起）。
			position := ruleAtoi(index)
			if position < 1 || position > len(list) {
				return nil
			}
			current = list[position-1]
		}
	}
	return current
}

// ruleJSONText JSON 节点 → 展示字符串（数字/布尔也能用，对象数组返回空）。
func ruleJSONText(value any) string {
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
	case float64:
		// JSON 数字：整数值去掉小数点（1740123456 不该显示成 1.740123456e+09）。
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return strings.TrimSpace(fmt.Sprint(typed))
	case map[string]any, []any:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

// ruleAtoi 宽松整数解析（非数字按空处理）。
func ruleAtoi(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	value := 0
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0
		}
		value = value*10 + int(r-'0')
	}
	return value
}

// ruleEpisode 规则引擎抓到的一集（标题 + 播放地址，地址可能待补全）。
type ruleEpisode struct {
	Title string
	URL   string
}

// ruleLine 一条播放线路（线路名 + 分集）。
type ruleLine struct {
	Title    string
	Episodes []ruleEpisode
}

// ruleChapters 规则引擎共用：把抓到的分集整理成 Chapter 列表。
//
// ID 用「线路序号-集序号」，与 MacCMS 采集站的 ID 形态一致；
// 集号从标题里解析（第12集 / 12 / EP12），解析不到时用序号。
// 非 http(s) 地址与重复地址直接丢弃（部分站点会给空链接的占位集）。
func ruleChapters(source, vodID, referer string, lines []ruleLine) []Chapter {
	var chapters []Chapter
	seenURL := map[string]bool{}
	for lineIndex, line := range lines {
		for epIndex, episode := range line.Episodes {
			address := strings.TrimSpace(episode.URL)
			if !isProviderHTTPMediaURL(address) || seenURL[address] {
				continue
			}
			seenURL[address] = true
			title := strings.TrimSpace(episode.Title)
			if title == "" {
				title = fmt.Sprintf("第%d集", epIndex+1)
			}
			number := episodeIndex(title, epIndex+1)
			chapters = append(chapters, Chapter{
				ID:             providerChapterID(source, vodID, fmt.Sprintf("%d-%d", lineIndex, epIndex)),
				Source:         source,
				Title:          title,
				VideoURL:       address,
				CurrentEpisode: rawEpisode(number),
				Referer:        referer,
			})
		}
	}
	if len(chapters) == 0 {
		return nil
	}
	sortProviderChapters(chapters)
	return uniqueChapters(chapters)
}

// ruleResolveURL 规则里的链接补全：前后缀拼接 + 相对地址解析。
// base 一般是详情页地址或站点地址（provide 层已有的 catVodResolveURL）。
func ruleResolveURL(base, address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return ""
	}
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return address
	}
	if strings.HasPrefix(address, "//") {
		return "https:" + address
	}
	if resolved := catVodResolveURL(base, address); resolved != "" {
		return resolved
	}
	return address
}

// ruleDecodeGBK 站点编码归一：规则声明或页面 meta 是 gb2312/gbk 时
// 转 UTF-8（老站很多是 GBK，不转会得到乱码标题）。
// declared 为规则里的「编码」字段；为空时从页面头部嗅探 meta charset。
func ruleDecodeGBK(body, declared string) string {
	if body == "" {
		return body
	}
	charset := strings.ToLower(strings.TrimSpace(declared))
	if charset == "" {
		head := body
		if len(head) > 2048 {
			head = head[:2048]
		}
		lowered := strings.ToLower(head)
		if !strings.Contains(lowered, "gb2312") && !strings.Contains(lowered, "gbk") {
			return body
		}
		charset = "gbk"
	}
	if !strings.Contains(charset, "gb") {
		return body
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().String(body)
	if err != nil {
		return body
	}
	return decoded
}

// rulePostText 规则引擎的 POST 请求（表单体）：与 fetchProviderText 的
// 超时 / 体积 / 状态码 / 拦截页判断保持一致，规则自定义头追加在后面。
func rulePostText(ctx context.Context, d *Downloader, target, postBody string, headers map[string]string, referer string) (string, error) {
	req, err := http.NewRequestWithContext(withUntrustedTargets(ctx), http.MethodPost, target, strings.NewReader(postBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", firstNonEmpty(headers["User-Agent"], userAgent))
	// 请求体形态决定 Content-Type：以 { / [ 开头按 JSON 发（海阔的
	// 「POST请求数据」常见 JSON），其余按表单发（XBPQ 的 ;post; 约定）。
	if trimmed := strings.TrimSpace(postBody); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	for key, value := range headers {
		if strings.EqualFold(key, "User-Agent") || value == "" {
			continue
		}
		req.Header.Set(key, value)
	}
	resp, err := d.doCatalogRequestWithTimeout(req, providerTimeout)
	if err != nil {
		return "", err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, providerMaxBodyBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if len(body) > providerMaxBodyBytes {
		return "", fmt.Errorf("响应超过 %d 字节", providerMaxBodyBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || catalogResponseBlockReason(resp, body) != "" {
		return "", d.catalogResponseError(req, resp, body)
	}
	return string(body), nil
}

// ruleGetWithHeaders 带完整自定义头的 GET（B 站需要 Cookie，
// fetchProviderText 只支持 UA 与 Referer 两个覆盖点，够不上）。
// 返回 HTTP 状态码与正文；4xx/5xx 不在这里报错，由调用方按语义处理
// （B 站风控 412 要单独给提示）。
func ruleGetWithHeaders(ctx context.Context, d *Downloader, target string, headers map[string]string) (int, string, error) {
	req, err := http.NewRequestWithContext(withUntrustedTargets(ctx), http.MethodGet, target, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", firstNonEmpty(headers["User-Agent"], userAgent))
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	for key, value := range headers {
		if strings.EqualFold(key, "User-Agent") || value == "" {
			continue
		}
		req.Header.Set(key, value)
	}
	resp, err := d.doCatalogRequestWithTimeout(req, providerTimeout)
	if err != nil {
		return 0, "", err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, providerMaxBodyBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return resp.StatusCode, "", readErr
	}
	if closeErr != nil {
		return resp.StatusCode, "", closeErr
	}
	if len(body) > providerMaxBodyBytes {
		return resp.StatusCode, "", fmt.Errorf("响应超过 %d 字节", providerMaxBodyBytes)
	}
	return resp.StatusCode, string(body), nil
}

// ruleStripJSONComments 去掉 JSON 文本里的 // 行注释与 /* */ 块注释。
//
// 海阔/猫系规则由人工维护，作者常写注释（真实样本「短剧屋」就有），
// 严格 JSON 解析会直接失败。这里按字符扫描，字符串内部（含 http://）
// 一律不动，避免把 URL 里的 // 当注释切掉。
func ruleStripJSONComments(body string) string {
	var builder strings.Builder
	builder.Grow(len(body))
	inString := false
	escaped := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inString {
			builder.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			builder.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(body) {
			switch body[i+1] {
			case '/':
				for i < len(body) && body[i] != '\n' {
					i++
				}
				if i < len(body) {
					builder.WriteByte('\n')
				}
				continue
			case '*':
				i += 2
				for i+1 < len(body) && !(body[i] == '*' && body[i+1] == '/') {
					i++
				}
				i++
				continue
			}
		}
		builder.WriteByte(c)
	}
	return builder.String()
}
