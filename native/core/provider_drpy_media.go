package core

// drpy 脚本源播放解析（M3）。
//
// drpy2 的播放约定（tvbox drpy2 与 drpy-node 一致，已实证）：
//   - 分集 ID 反解出 (flag, episodeKey)，episodeKey 是 vod_play_url 里
//     「$ 后面的值」（播放地址或 ID，不是集名）；
//   - 调 play(flag, episodeKey, flags)（drpy-node 新脚本改用 lazy 且把
//     结果写进全局 input），返回 {url, jx, parse, header}；
//   - jx=0 且 parse=0：url 即直链；
//   - jx=1 或 parse=1：url 是解析页地址。本实现按用户确认的取舍只做
//     「简单嗅探」——抓播放页用正则提取 m3u8/mp4，不接完整解析服务；
//   - play/lazy 失败且章节自带媒体直链时，退化为直链播放。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/dop251/goja"
)

// 播放页嗅探的体积上限（与 drpyRespMaxBytes 一致的防护思路）。
const drpySniffMaxBytes = 2 << 20

var (
	// 常见播放器配置字段里的媒体地址（精确优先）。
	reDrpyMediaField = regexp.MustCompile(`(?is)["']?(?:videoSrc|videoUrl|video_url|playUrl|play_url|src|url)["']?\s*[:=]\s*["']([^"']+\.(?:m3u8|mp4)(?:[^"']*)?)["']`)
	// 页面里任意媒体地址（兜底）。
	reDrpyMediaAny = regexp.MustCompile(`(?i)https?://[^"'\s<>\\]+\.(?:m3u8|mp4)(?:\?[^"'\s<>\\]*)?`)
)

// drpyPlayResult play/lazy 返回值的归一化形态。
type drpyPlayResult struct {
	url    string
	jx     int
	parse  int
	header map[string]string
	msg    string
}

// drpyIsDirectMediaURL 地址是否本身就是可直接播放的媒体直链
// （http(s) + .m3u8/.mp4 结尾，带查询串也认）。
func drpyIsDirectMediaURL(raw string) bool {
	if !isProviderHTTPMediaURL(raw) {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return drpyMediaPathSupported(strings.ToLower(parsed.Path))
}

// drpyMediaExtensions 可直接播放的媒体后缀。除视频外也认音频——Bookan
// 这类有声书源的播放地址是 .m4a / .mp3。
var drpyMediaExtensions = []string{
	".m3u8", ".mp4", ".m4a", ".mp3", ".aac", ".flac", ".wav",
	".mkv", ".flv", ".mov", ".webm", ".m4v",
}

func drpyMediaPathSupported(path string) bool {
	for _, extension := range drpyMediaExtensions {
		if strings.HasSuffix(path, extension) {
			return true
		}
	}
	return false
}

// resolveDrpyMedia drpy 脚本源播放解析。失败时按「章节自带直链 → 报错」
// 的顺序退化，保证脚本临时失效时已缓存的直链仍可播放。
func (d *Downloader) resolveDrpyMedia(ctx context.Context, task Task, site drpySite) (providerMedia, error) {
	source, sourceID, valid := splitProviderDramaID(task.Chapter.ID)
	if !valid {
		return providerMedia{}, errors.New("剧集信息无效，请刷新剧库")
	}
	vodPart, episodeBlob, found := strings.Cut(sourceID, ":")
	if !found {
		return providerMedia{}, errors.New("分集信息无效，请刷新剧库")
	}
	vodID, ok := drpyDecodeSegment(vodPart)
	if !ok || strings.TrimSpace(vodID) == "" {
		return providerMedia{}, errors.New("剧集信息无效，请刷新剧库")
	}
	flag, episodeKey, ok := drpyPartsFromBlob(episodeBlob)
	if !ok || strings.TrimSpace(episodeKey) == "" {
		return providerMedia{}, errors.New("分集信息无效，请刷新剧库")
	}
	media := providerMedia{Referer: drpySiteReferer(site)}
	// 两种方言共用同一套结果处理：{url, jx, parse, header} → 直链 / 嗅探。
	apply := func(vm *goja.Runtime) error {
		var result drpyPlayResult
		var callErr error
		if drpySiteIsCatVod(site) {
			result, callErr = catVodCallPlay(vm, flag, episodeKey)
		} else if !drpyHasFunction(vm, "play") && !drpyHasFunction(vm, "lazy") {
			// rule 驱动型脚本没有取流函数：分集地址本身就是播放地址
			// （相对路径后面按 host 补全，播放页地址走嗅探兜底）。
			result = drpyPlayResult{url: episodeKey}
		} else {
			result, callErr = drpyCallPlay(vm, flag, episodeKey)
		}
		if callErr != nil {
			return callErr
		}
		if result.url == "" {
			if result.msg != "" {
				return errors.New(result.msg)
			}
			return errors.New("脚本未返回播放地址")
		}
		address := drpyJoinURL(drpySiteReferer(site), result.url)
		if strings.HasPrefix(address, "tvbox-xg:") {
			return errors.New("该源使用 TVBox 专有协议（tvbox-xg），本机播放器无法播放")
		}
		referer := media.Referer
		if value := drpyHeaderValue(result.header, "Referer"); value != "" {
			referer = value
		}
		if result.jx == 1 || result.parse == 1 {
			// 简单嗅探：抓解析页提取直链（完整解析不在本实现范围）。
			sniffed, sniffErr := d.drpySniffPlayPage(ctx, address, referer)
			if sniffErr != nil {
				return fmt.Errorf("解析播放页失败: %w", sniffErr)
			}
			address = sniffed
		}
		if !drpyIsDirectMediaURL(address) {
			return fmt.Errorf("脚本返回的播放地址无法播放: %s", address)
		}
		media.URL, media.Referer = address, referer
		return nil
	}
	var playErr error
	if drpySiteIsCatVod(site) {
		playErr = d.catVodInvoke(ctx, site, apply)
	} else {
		playErr = d.drpyInvoke(ctx, site, apply)
	}
	if playErr == nil {
		return d.finishProviderMedia(ctx, media, source)
	}
	// 退化：章节自带媒体直链时仍可播放（脚本临时失效的兜底）。
	if drpyIsDirectMediaURL(task.Chapter.VideoURL) {
		fallback := providerMedia{URL: task.Chapter.VideoURL, Referer: firstNonEmpty(task.Chapter.Referer, media.Referer)}
		return d.finishProviderMedia(ctx, fallback, source)
	}
	return providerMedia{}, playErr
}

// drpyCallPlay 调脚本的 play（drpy-node 新脚本用 lazy）。play 缺失或未
// 返回结果时回退 lazy；lazy 的惯例是把结果写进全局 input、函数本身返回
// 空，需要补读。两者都不可用时返回更有信息量的那个错误。
func drpyCallPlay(vm *goja.Runtime, flag, episodeKey string) (drpyPlayResult, error) {
	raw, err := drpyCall(vm, "play", flag, episodeKey, flag)
	if err != nil || raw == nil {
		// lazy 必须真的给出结果才替换；否则保留 play 的错误
		// （例如 play 抛异常、lazy 只是返回空，不该把错误抹掉）。
		if lazyRaw, lazyErr := drpyCall(vm, "lazy", flag, episodeKey, flag); lazyErr == nil && lazyRaw != nil {
			raw, err = lazyRaw, nil
		}
	}
	if err != nil {
		return drpyPlayResult{}, err
	}
	if raw == nil {
		// lazy 惯例：结果在全局 input 里。
		if value := vm.Get("input"); value != nil && !goja.IsUndefined(value) && !goja.IsNull(value) {
			raw = value.Export()
		}
	}
	if raw == nil {
		return drpyPlayResult{}, errors.New("脚本未返回播放地址")
	}
	return drpyPlayResultFrom(raw), nil
}

// drpyPlayResultFrom 归一化 play/lazy 的返回值：字符串直链、drpy2 的
// {url, jx, parse, header, msg} 对象都认。
func drpyPlayResultFrom(raw any) drpyPlayResult {
	switch value := raw.(type) {
	case string:
		text := strings.TrimSpace(value)
		// CatVod 的 play 返回 **JSON 字符串**（drpy2 直接返回对象），
		// 先解一层；否则整段 JSON 会被当成播放地址。
		if strings.HasPrefix(text, "{") {
			var decoded map[string]any
			if json.Unmarshal([]byte(text), &decoded) == nil {
				return drpyPlayResultFrom(decoded)
			}
		}
		return drpyPlayResult{url: text}
	case map[string]any:
		return drpyPlayResult{
			url:    drpyString(value, "url"),
			jx:     drpyInt(value, "jx"),
			parse:  drpyInt(value, "parse"),
			header: drpyHeaderMap(value["header"]),
			msg:    drpyString(value, "msg"),
		}
	default:
		return drpyPlayResult{}
	}
}

// drpyHeaderMap 归一化 header 参数：对象或 JSON 字符串都认。
func drpyHeaderMap(raw any) map[string]string {
	switch value := raw.(type) {
	case map[string]any:
		flat := map[string]string{}
		for key, item := range value {
			if item == nil {
				continue
			}
			flat[strings.TrimSpace(key)] = strings.TrimSpace(fmt.Sprint(item))
		}
		return flat
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return nil
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(text), &decoded) != nil {
			return nil
		}
		return drpyHeaderMap(decoded)
	default:
		return nil
	}
}

// drpyHeaderValue 大小写不敏感地取 header 值。
func drpyHeaderValue(header map[string]string, key string) string {
	if len(header) == 0 {
		return ""
	}
	if value, ok := header[key]; ok {
		return strings.TrimSpace(value)
	}
	for name, value := range header {
		if strings.EqualFold(name, key) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// drpySniffPlayPage 简单嗅探：抓解析页，从 HTML/JSON 里提取 m3u8/mp4 直链。
// 只做正则级提取（用户确认的取舍），不跑完整解析。
func (d *Downloader) drpySniffPlayPage(ctx context.Context, address, referer string) (string, error) {
	if !isProviderHTTPMediaURL(address) {
		return "", errors.New("解析地址无效")
	}
	// 解析页多半按移动端 UA 放行，嗅探时带移动端 UA。
	pageContext := context.WithValue(ctx, providerTextUserAgentKey{}, drpyMobileUA)
	body, err := d.fetchProviderText(pageContext, address, referer)
	if err != nil {
		return "", err
	}
	if len(body) > drpySniffMaxBytes {
		body = body[:drpySniffMaxBytes]
	}
	// 先精确匹配常见播放器字段，再退到通用媒体地址正则。
	if match := reDrpyMediaField.FindStringSubmatch(body); match != nil {
		return drpyUnescapeMediaURL(match[1]), nil
	}
	if match := reDrpyMediaAny.FindString(body); match != "" {
		return drpyUnescapeMediaURL(match), nil
	}
	return "", errors.New("未能从播放页提取到播放地址")
}

// drpyUnescapeMediaURL 去掉 JSON 转义（\/ 与 \u0026 之类在正则结果里常见）。
func drpyUnescapeMediaURL(raw string) string {
	unescaped := strings.TrimSpace(raw)
	unescaped = strings.ReplaceAll(unescaped, `\/`, "/")
	// JSON 字符串反转义（\u0026 等），失败则沿用替换后的原值。
	var decoded string
	if err := json.Unmarshal([]byte(`"`+strings.ReplaceAll(unescaped, `"`, "")+`"`), &decoded); err == nil {
		return strings.TrimSpace(decoded)
	}
	return unescaped
}
