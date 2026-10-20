package core

// LibreTV 播放解析（parses）。
//
// 背景：TVBox / LibreTV 订阅除了 sites 还带 parses —— 采集站返回的
// vod_play_url 有时是网页地址（腾讯 / 优酷 / 自有播放页），需要二次解析
// 才能拿到 m3u8/mp4。本项目提供三种处理方式：
//
//  1. 直链/嗅探（默认，无后端）：URL 本身是媒体直链就直接播；是网页时
//     抓取页面，用正则从 HTML/JSON 里嗅探媒体地址；
//  2. JSON 解析接口（type=1，可选）：GET <url>?url=<encode(播放页)>，
//     从返回的 JSON 里取 playUrl/url。依赖第三方后端，不是「无后端」方案；
//  3. 本地解析脚本（type=2 且 url 以 .js 结尾，本项目扩展）：goja 执行
//     脚本里的 parse(url)，返回 {url, jx, parse, header}。req / pdfh /
//     pdfa / 看门狗全部复用 drpy 那套宿主环境。
//
// 注意：type=2 在 TVBox 标准里是「聚合解析」，这里借用为本地脚本属于
// **扩展语义**，README 已标注，避免用户拿标准配置对号入座。

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
)

const (
	// libretvParsesDir 本地解析脚本缓存目录（相对引擎目录）。
	libretvParsesDir = "parses"
	// libretvParseScriptMaxBytes 单个解析脚本大小上限（512KB）。
	libretvParseScriptMaxBytes = 512 << 10
)

// libretvParseScriptKey 脚本缓存键（URL 的 SHA1 前 16 位）。
func libretvParseScriptKey(parse libretvParse) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(parse.URL)))
	return hex.EncodeToString(sum[:])[:16]
}

// libretvParseScriptFile 脚本缓存文件（相对引擎目录）。
func libretvParseScriptFile(parse libretvParse) string {
	return libretvParsesDir + "/" + libretvParseScriptKey(parse) + ".js"
}

// resolveLibreTVMedia LibreTV 分集播放解析：
// 直链直接播；非直链按所选解析方式处理，未选择时走「嗅探」。
func (d *Downloader) resolveLibreTVMedia(ctx context.Context, task Task, site libretvSite) (providerMedia, error) {
	chapter := task.Chapter
	address := strings.TrimSpace(chapter.VideoURL)
	referer := firstNonEmpty(chapter.Referer, libretvSiteReferer(site))
	// 采集站多数返回 m3u8/mp4 直链：无论选了什么解析方式都直接播，
	// 避免把本来可用的直链送进解析器反而失败。
	if drpyIsDirectMediaURL(address) {
		return d.finishProviderMedia(ctx, providerMedia{URL: address, Referer: referer}, chapter.Source)
	}
	parse, hasParse := d.libretvSelectedParse()
	var (
		resolved string
		headers  map[string]string
		err      error
	)
	switch {
	case hasParse && libretvParseIsScript(parse):
		resolved, headers, err = d.runLibretvScriptParse(ctx, parse, address, referer)
	case hasParse && parse.Type == 1:
		resolved, headers, err = d.runLibretvJSONParse(ctx, parse, address, referer)
	case hasParse && libretvParseNeedsWebView(parse):
		// type 0/4 需要真实浏览器环境。播放器会先由 Dart 侧 WebView 嗅探并把
		// 章节地址替换成直链，不会走到这里；下载等无 WebView 的链路给出明确提示。
		return providerMedia{}, errors.New("该解析方式需要 WebView 嗅探，请在播放器中使用；下载暂不支持此解析方式")
	case hasParse:
		return providerMedia{}, fmt.Errorf("所选解析方式（%s）暂不支持，请改用「直链/嗅探」、「WebView 嗅探」或本地解析脚本", libretvParseKind(parse))
	default:
		// 默认「直链/嗅探」：直链在函数开头已处理，这里只剩网页。
		resolved, err = d.drpySniffPlayPage(ctx, address, referer)
		if err == nil && strings.TrimSpace(resolved) == "" {
			err = errors.New("未找到视频地址")
		}
		if err != nil {
			return providerMedia{}, errors.New("未从播放页找到视频地址，可在站源管理选择解析方式")
		}
	}
	if err != nil {
		return providerMedia{}, err
	}
	resolved = strings.TrimSpace(resolved)
	if resolved == "" {
		return providerMedia{}, errors.New("解析未返回播放地址")
	}
	if value := drpyHeaderValue(headers, "Referer"); value != "" {
		referer = value
	}
	// jx/parse 语义：解析结果仍是网页时再嗅探一次。
	if !drpyIsDirectMediaURL(resolved) {
		sniffed, sniffErr := d.drpySniffPlayPage(ctx, resolved, referer)
		if sniffErr != nil {
			return providerMedia{}, fmt.Errorf("解析结果不是可播放地址，且嗅探失败: %w", sniffErr)
		}
		resolved = sniffed
	}
	return d.finishProviderMedia(ctx, providerMedia{URL: resolved, Referer: referer}, chapter.Source)
}

// runLibretvJSONParse 调用标准 type=1 JSON 解析接口。
// 约定：GET <url>?url=<播放页>，返回 {url|playUrl|data.url} 与可选 header。
func (d *Downloader) runLibretvJSONParse(ctx context.Context, parse libretvParse, address, referer string) (string, map[string]string, error) {
	endpoint := strings.TrimSpace(parse.URL)
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	body, err := d.fetchProviderText(ctx, endpoint+separator+"url="+url.QueryEscape(address), referer)
	if err != nil {
		return "", nil, fmt.Errorf("解析接口请求失败：%w", publicError(err))
	}
	playURL, headers, err := libretvParseJSONResponse(body)
	if err != nil {
		return "", nil, err
	}
	if strings.HasPrefix(playURL, "//") {
		playURL = "https:" + playURL
	}
	if strings.HasPrefix(playURL, "/") {
		playURL = drpyJoinURL(endpoint, playURL)
	}
	return playURL, headers, nil
}

// libretvParseJSONResponse 从解析接口响应里提取播放地址与 header。
// 兼容纯文本地址、常见 JSON 字段与 data 包裹。
func libretvParseJSONResponse(body string) (string, map[string]string, error) {
	text := strings.TrimSpace(body)
	if text == "" {
		return "", nil, errors.New("解析接口返回为空")
	}
	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		return text, nil, nil
	}
	var payload map[string]any
	if json.Unmarshal([]byte(text), &payload) != nil {
		return "", nil, errors.New("解析接口返回的不是 JSON，也不是播放地址")
	}
	candidate := firstNonEmpty(
		drpyString(payload, "url"),
		drpyString(payload, "playUrl"),
		drpyString(payload, "play_url"),
		drpyString(payload, "video"),
	)
	if candidate == "" {
		if data, ok := payload["data"].(map[string]any); ok {
			candidate = firstNonEmpty(
				drpyString(data, "url"),
				drpyString(data, "playUrl"),
				drpyString(data, "play_url"),
			)
		}
	}
	if candidate == "" {
		if message := firstNonEmpty(drpyString(payload, "msg"), drpyString(payload, "message")); message != "" {
			return "", nil, errors.New(message)
		}
		return "", nil, errors.New("解析接口未返回播放地址")
	}
	headers := drpyHeaderMap(payload["header"])
	if len(headers) == 0 {
		headers = drpyHeaderMap(payload["headers"])
	}
	return candidate, headers, nil
}

// runLibretvScriptParse 执行本地解析脚本：parse(url) → {url, jx, parse, header}。
// 脚本按需下载并缓存到 parses/<hash>.js，宿主环境与 drpy 完全一致。
func (d *Downloader) runLibretvScriptParse(ctx context.Context, parse libretvParse, address, referer string) (string, map[string]string, error) {
	if d.drpy == nil {
		return "", nil, errors.New("解析脚本运行环境不可用")
	}
	if err := d.ensureLibretvParseScript(ctx, parse); err != nil {
		return "", nil, err
	}
	site := drpySite{
		Key:       "parse-" + libretvParseScriptKey(parse),
		Name:      parse.Name,
		ScriptURL: parse.URL,
		File:      libretvParseScriptFile(parse),
	}
	var result drpyPlayResult
	err := d.drpyInvoke(ctx, site, func(vm *goja.Runtime) error {
		raw, callErr := drpyCall(vm, "parse", address)
		if callErr != nil && strings.Contains(callErr.Error(), "missing parse") {
			// 兜底：部分脚本把函数挂在 rule.parse 上。
			if _, snippetErr := vm.RunString(`
				if (globalThis.rule && typeof globalThis.rule.parse === 'function') {
					globalThis.__parseTarget = globalThis.rule.parse;
				}
			`); snippetErr == nil {
				if target := vm.Get("__parseTarget"); target != nil && !goja.IsUndefined(target) && !goja.IsNull(target) {
					raw, callErr = drpyCall(vm, "__parseTarget", address)
				}
			}
		}
		if callErr != nil {
			return callErr
		}
		if raw == nil {
			return errors.New("解析脚本未返回结果")
		}
		result = drpyPlayResultFrom(raw)
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("解析脚本执行失败：%w", err)
	}
	if strings.TrimSpace(result.url) == "" {
		if result.msg != "" {
			return "", nil, errors.New(result.msg)
		}
		return "", nil, errors.New("解析脚本未返回播放地址")
	}
	address = drpyJoinURL(referer, result.url)
	headers := result.header
	if value := drpyHeaderValue(headers, "Referer"); value != "" {
		referer = value
	}
	// jx/parse > 0 表示返回的是需要再次解析的页面；统一走后续嗅探分支，
	// 这里无需额外处理，返回值原样交给上层。
	return address, headers, nil
}

// ensureLibretvParseScript 确保脚本已缓存：优先读本地，缺失时从 URL 下载。
func (d *Downloader) ensureLibretvParseScript(ctx context.Context, parse libretvParse) error {
	file := libretvParseScriptFile(parse)
	path := filepath.Join(d.drpy.directory, filepath.FromSlash(file))
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		if len(body) > libretvParseScriptMaxBytes {
			return errors.New("解析脚本过大")
		}
		return nil
	}
	fetched, err := d.fetchProviderText(ctx, parse.URL, parse.URL)
	if err != nil {
		return fmt.Errorf("解析脚本下载失败：%w", publicError(err))
	}
	if len(fetched) > libretvParseScriptMaxBytes {
		return errors.New("解析脚本过大")
	}
	if strings.TrimSpace(fetched) == "" {
		return errors.New("解析脚本内容为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("解析脚本目录创建失败：%w", publicError(err))
	}
	if err := os.WriteFile(path, []byte(fetched), 0o600); err != nil {
		return fmt.Errorf("解析脚本保存失败：%w", publicError(err))
	}
	// 脚本内容更新后丢弃旧的编译缓存。
	d.drpy.invalidateProgram("parse-" + libretvParseScriptKey(parse))
	return nil
}

// libretvSiteReferer 采集站 Referer（scheme://host/）。
func libretvSiteReferer(site libretvSite) string {
	parsed, err := url.Parse(strings.TrimSpace(site.API))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host + "/"
}
