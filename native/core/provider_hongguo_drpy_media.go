package core

// drpys 两段式取址链路（路线一，provider_hongguo_drpy_* 家族的网络出口）。
//
// 与既有链路的分工：
//   - 既有 resolveHongguoAppMedia 走 video_model/v1/ 单段（App 门，签名见 provider_hongguo_app.go）；
//   - 既有 resolveHongguoPlaybackAPI 走 djapi 备用接口（上游已断）；
//   - 本文件实现 drpys（实测可用）的两段式：
//     1) POST novel/player/multi_video_model/v1/，带 6 神签名头（hgDrpySignedVideoRequest），
//        从 data[vid].video_model.fallback_api 取第二段地址；
//     2) GET fallback_api（只带 UA，不带 Referer、不带签名头），
//        响应 video_info.data.video_list 给出各清晰度/编码片源；
//     3) spade_a 经 hgDrpyContentKey 派生 CENC key；
//        info.key_seed 存在时 hgDrpyDecryptSpadeURL 解出真实 CDN 地址；
//     4) 选档策略（codecRank + 清晰度）与 drpys download-quality.js 逐行对齐。
//
// 安全边界（对齐 drpys checkedUrl）：
//   - 接口请求域名限 fqnovel.com / snssdk.com；
//   - 最终 CDN 域名限 qznovelvod.com / fqnovelvod.com / douyinvod.com；
//   - 请求不跟随重定向到白名单外（POST 遇跳转直接按不安全处理）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// hgDrpyMediaReferer 是 drpys 给 CDN 请求设置的 Referer（novelquickapp.com）。
	hgDrpyMediaReferer = "https://novelquickapp.com/"
	// hgDrpyMixedVideoType 是 mixed_video_id_map 的内容类型键（drpys 固定 "1004"）。
	hgDrpyMixedVideoType = "1004"
	// hgDrpyMediaTimeout 对齐 drpys 的 12 秒 deadline。
	hgDrpyMediaTimeout = 12 * time.Second
)

// hgDrpyAPIDomains 是接口请求（第一段/第二段）的域名白名单（drpys requestJson）。
var hgDrpyAPIDomains = []string{"fqnovel.com", "snssdk.com"}

// hgDrpyCDNDomains 是最终媒体地址的域名白名单（drpys 媒体段 checkedUrl）。
var hgDrpyCDNDomains = []string{"qznovelvod.com", "fqnovelvod.com", "douyinvod.com"}

// hgDrpyDownloadQualities 是 drpys download-quality.js 支持的清晰度档位。
var hgDrpyDownloadQualities = []string{"auto", "1080p", "720p"}

// resolveHongguoDrpyMedia 走 drpys 两段式链路解析红果本集媒体。
// quality 取值 auto/1080p/720p（对齐 drpys normalizeDownloadQuality）。
func (downloader *Downloader) resolveHongguoDrpyMedia(ctx context.Context, videoID, quality string) (providerMedia, error) {
	requestedQuality, err := hgDrpyNormalizeDownloadQuality(quality)
	if err != nil {
		return providerMedia{}, err
	}
	if !hongguoNumericID.MatchString(videoID) {
		return providerMedia{}, errors.New("红果视频 ID 无效")
	}
	payload, err := hgDrpyVideoModelPayload(videoID)
	if err != nil {
		return providerMedia{}, err
	}
	firstBody, err := downloader.hgDrpyVideoModelAt(ctx, payload)
	if err != nil {
		return providerMedia{}, err
	}
	fallback, err := hgDrpyFallbackFromModel(firstBody, videoID)
	if err != nil {
		return providerMedia{}, err
	}
	// 第二段只带 UA（drpys: requestJson(fallback, { headers: { 'User-Agent': VIDEO_UA } })）。
	secondBody, err := hgDrpyRequestJSON(ctx, http.MethodGet, fallback, map[string]string{"User-Agent": hgDrpyVideoUA}, nil)
	if err != nil {
		return providerMedia{}, err
	}
	info, err := hgDrpyVideoInfo(secondBody)
	if err != nil {
		return providerMedia{}, err
	}
	return hgDrpySelectMedia(info, videoID, requestedQuality)
}

// hgDrpyVideoModelAt 第一段：按端点池依次尝试 multi_video_model。
//
// 默认池内只有一个端点，行为与改造前完全一致；池内有多端点时，
// 某个端点失败会换下一个继续，并对该 (线路, 端点) 组合记账
// （连续失败达阈值后冷却期内跳过，详见 provider_hongguo_health.go）。
// 记账只消费真实取流请求的结果，不额外发起探测。
func (downloader *Downloader) hgDrpyVideoModelAt(ctx context.Context, payload []byte) (map[string]any, error) {
	var lastErr error
	for _, api := range hongguoDrpyHosts {
		key := hongguoHostKey(hongguoRouteDrpy, api)
		if hongguoRoutesHealth.skip(key, time.Now()) {
			continue
		}
		signedURL, headers := hgDrpySignedVideoRequestAt(payload, time.Now(), api)
		body, err := hgDrpyRequestJSON(ctx, http.MethodPost, signedURL, headers, payload)
		if err == nil {
			hongguoRoutesHealth.noteOK(key)
			return body, nil
		}
		hongguoRoutesHealth.noteFail(key, time.Now(), hongguoClassifyFailure(err))
		lastErr = err
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("红果取址端点均不可用")
	}
	return nil, lastErr
}

// hgDrpyVideoModelPayload 构造第一段请求体（drpys fetchAppPlayUrl 的 payload 字面量）。
func hgDrpyVideoModelPayload(videoID string) ([]byte, error) {
	payload := map[string]any{
		"biz_param": map[string]any{
			"detail_page_version":       0,
			"device_level":              3,
			"disable_digg_stat":         false,
			"need_all_video_definition": true,
			"need_mp4_align":            false,
			"use_os_player":             false,
			"use_server_dns":            false,
			"video_platform":            1024,
		},
		"mixed_video_id_map": map[string]any{
			hgDrpyMixedVideoType: []string{videoID},
		},
	}
	return json.Marshal(payload)
}

// hgDrpyFallbackFromModel 从第一段响应解析 fallback_api（drpys parseFallback）。
func hgDrpyFallbackFromModel(response map[string]any, videoID string) (string, error) {
	if hgDrpyResponseCode(response) != 0 {
		return "", errors.New("App 接口暂未提供本集播放信息")
	}
	data, _ := response["data"].(map[string]any)
	entry, _ := data[videoID].(map[string]any)
	if entry == nil {
		return "", errors.New("App 接口未返回所选分集，已停止取址")
	}
	model := entry["video_model"]
	if encoded, ok := model.(string); ok {
		model = hgDrpyJSONBody(encoded)
	}
	modelMap, _ := model.(map[string]any)
	if modelMap == nil {
		return "", errors.New("App 接口未返回所选分集，已停止取址")
	}
	fallback := hgDrpyParseFallback(modelMap["fallback_api"])
	if fallback == "" {
		return "", errors.New("App 接口未返回本集片源地址")
	}
	return fallback, nil
}

// hgDrpyVideoInfo 从第二段响应取 video_info.data（drpys 的 Code/code 双重校验）。
func hgDrpyVideoInfo(response map[string]any) (map[string]any, error) {
	if hgDrpyResponseCode(response) != 0 {
		return nil, errors.New("App 接口暂未提供本集播放信息")
	}
	videoInfo, _ := response["video_info"].(map[string]any)
	if videoInfo == nil || hgDrpyResponseCode(videoInfo) != 0 {
		return nil, errors.New("App 接口暂未提供本集播放信息")
	}
	info, _ := videoInfo["data"].(map[string]any)
	if info == nil {
		return nil, errors.New("App 接口未返回本集片源模型")
	}
	return info, nil
}

// hgDrpyResponseCode 读取 drpys 的 Code || code 业务码（缺失按 0）。
func hgDrpyResponseCode(response map[string]any) int {
	for _, key := range []string{"Code", "code"} {
		if value, ok := response[key]; ok {
			if number, ok := hgDrpyNumber(value); ok {
				return int(number)
			}
		}
	}
	return 0
}

// hgDrpyParseFallback 实现 drpys parseFallback：value?.fallback_api || value。
func hgDrpyParseFallback(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		if nested, ok := typed["fallback_api"].(string); ok {
			return strings.TrimSpace(nested)
		}
	}
	return ""
}

// hgDrpyJSONBody 对应 drpys jsonBody：解析失败返回 nil。
func hgDrpyJSONBody(value string) any {
	var parsed any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return nil
	}
	return parsed
}

// hgDrpySelectMedia 按 drpys 策略选档并组装 variants。
func hgDrpySelectMedia(info map[string]any, videoID, requestedQuality string) (providerMedia, error) {
	rows := hgDrpyMediaRows(info)
	filtered := make([]hgDrpyMediaRow, 0, len(rows))
	for _, row := range rows {
		if row.codecRank >= 2 || !hgDrpyMeetsDownloadQuality(row.width, row.height, requestedQuality) {
			continue
		}
		filtered = append(filtered, row)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if requestedQuality == "auto" {
			if filtered[i].codecRank != filtered[j].codecRank {
				return filtered[i].codecRank < filtered[j].codecRank
			}
			return filtered[i].pixels > filtered[j].pixels
		}
		shortI := hgDrpyShortSide(filtered[i].width, filtered[i].height)
		shortJ := hgDrpyShortSide(filtered[j].width, filtered[j].height)
		if shortI != shortJ {
			return shortI < shortJ
		}
		return filtered[i].codecRank < filtered[j].codecRank
	})
	if len(filtered) == 0 {
		if requestedQuality == "auto" {
			return providerMedia{}, errors.New("本集暂未提供兼容的 H.264/HEVC 片源")
		}
		return providerMedia{}, fmt.Errorf("本集暂无尺寸可确认且满足 %s 的 H.264/HEVC 片源，未降级", requestedQuality)
	}
	var keySeed []byte
	if seed, ok := info["key_seed"].(string); ok && strings.TrimSpace(seed) != "" {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(seed))
		if err != nil {
			return providerMedia{}, errors.New("App 接口返回的片源密钥种子无效")
		}
		keySeed = decoded
	}
	var variants []providerMedia
	seen := map[string]bool{}
	var lastErr error
	for _, row := range filtered {
		spade := hgDrpyFirstString(row.item["spade_a"], hgDrpyNestedString(row.item, "encrypt_info", "spade_a"))
		var contentKey []byte
		if spade != "" {
			key, err := hgDrpyContentKey(spade)
			if err != nil {
				lastErr = err
				continue
			}
			contentKey = key
		}
		for _, candidate := range hgDrpyURLCandidates(row.item) {
			if candidate == "" {
				continue
			}
			mediaURL, err := hgDrpyResolveMediaURL(candidate, keySeed)
			if err != nil {
				lastErr = err
				continue
			}
			contentKeyHex := ""
			if len(contentKey) > 0 {
				contentKeyHex = fmt.Sprintf("%x", contentKey)
			}
			identity := mediaURL + ":" + contentKeyHex
			if seen[identity] {
				continue
			}
			seen[identity] = true
			qualityNumber := 0
			if requestedQuality == "auto" {
				qualityNumber = hgDrpyFirstNumber(row.quality)
				if qualityNumber == 0 {
					qualityNumber = hgDrpyShortSide(row.width, row.height)
				}
			} else {
				qualityNumber, _ = strconv.Atoi(strings.TrimSuffix(requestedQuality, "p"))
			}
			variants = append(variants, providerMedia{
				URL:      mediaURL,
				Referer:  hgDrpyMediaReferer,
				CENCKey:  contentKey,
				Quality:  qualityNumber,
				Variants: nil,
			})
		}
	}
	if len(variants) == 0 {
		if lastErr != nil {
			return providerMedia{}, fmt.Errorf("本集片源地址或密钥无法解析：%v", publicError(lastErr))
		}
		return providerMedia{}, errors.New("本集片源地址或密钥无法解析")
	}
	selected := variants[0]
	selected.Variants = append([]providerMedia(nil), variants...)
	return selected, nil
}

// hgDrpyMediaRow 是 drpys 选档逻辑中的一行（item + 解析出的编码/清晰度/尺寸）。
type hgDrpyMediaRow struct {
	item      map[string]any
	codec     string
	codecRank int
	quality   string
	pixels    int
	width     int
	height    int
}

// hgDrpyMediaRows 把 video_list（map 或 list）展开为选档行（drpys Object.values 语义）。
func hgDrpyMediaRows(info map[string]any) []hgDrpyMediaRow {
	list := info["video_list"]
	var items []any
	switch typed := list.(type) {
	case map[string]any:
		// drpys 用 Object.values，顺序不敏感（后面有稳定排序）。
		items = make([]any, 0, len(typed))
		for _, value := range typed {
			items = append(items, value)
		}
	case []any:
		items = typed
	}
	rows := make([]hgDrpyMediaRow, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		if hgDrpyFirstString(item["main_url"], item["backup_url"], item["backup_url_list"]) == "" && len(hgDrpyURLCandidates(item)) == 0 {
			continue
		}
		meta, _ := item["video_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		codec := strings.ToLower(hgDrpyFirstString(item["codec_type"], meta["codec_type"]))
		codecRank := 2
		switch codec {
		case "h264", "avc", "avc1":
			codecRank = 0
		case "hevc", "h265", "hvc1", "hev1", "bytevc1":
			codecRank = 1
		}
		quality := hgDrpyFirstString(item["quality_desc"], item["definition"], item["height"], item["vheight"], meta["definition"])
		width, height := 0, 0
		for _, pair := range [][2]any{
			{item["vwidth"], item["vheight"]},
			{item["width"], item["height"]},
			{meta["vwidth"], meta["vheight"]},
			{meta["width"], meta["height"]},
		} {
			candidateWidth, okWidth := hgDrpyPositiveInt(pair[0])
			candidateHeight, okHeight := hgDrpyPositiveInt(pair[1])
			if okWidth && okHeight {
				width, height = candidateWidth, candidateHeight
				break
			}
		}
		pixels := 0
		if width > 0 && height > 0 {
			if width < height {
				pixels = width
			} else {
				pixels = height
			}
		} else {
			pixels = hgDrpyFirstNumber(quality)
		}
		rows = append(rows, hgDrpyMediaRow{
			item: item, codec: codec, codecRank: codecRank,
			quality: quality, pixels: pixels, width: width, height: height,
		})
	}
	return rows
}

// hgDrpyURLCandidates 汇总 drpys 的 URL 候选顺序：main_url、backup_url、backup_url_list。
func hgDrpyURLCandidates(item map[string]any) []string {
	var candidates []string
	if main, ok := item["main_url"].(string); ok && strings.TrimSpace(main) != "" {
		candidates = append(candidates, strings.TrimSpace(main))
	}
	if backup, ok := item["backup_url"].(string); ok && strings.TrimSpace(backup) != "" {
		candidates = append(candidates, strings.TrimSpace(backup))
	}
	switch list := item["backup_url_list"].(type) {
	case []any:
		for _, value := range list {
			if address, ok := value.(string); ok && strings.TrimSpace(address) != "" {
				candidates = append(candidates, strings.TrimSpace(address))
			}
		}
	case string:
		if strings.TrimSpace(list) != "" {
			candidates = append(candidates, strings.TrimSpace(list))
		}
	}
	return candidates
}

// hgDrpyResolveMediaURL 解出最终 CDN 地址并过域名白名单（drpys decryptSpadeUrl/cleanUrl/checkedUrl）。
func hgDrpyResolveMediaURL(candidate string, keySeed []byte) (string, error) {
	var media string
	if len(keySeed) > 0 {
		decrypted, err := hgDrpyDecryptSpadeURL(candidate, keySeed)
		if err != nil {
			return "", err
		}
		media = decrypted
	} else {
		media = hgDrpyCleanURL(candidate)
	}
	return hgDrpyCheckedURL(media, hgDrpyCDNDomains)
}

// hgDrpyCleanURL 对应 drpys cleanUrl：反转义常见的转义斜杠/与号。
func hgDrpyCleanURL(value string) string {
	cleaned := strings.TrimSpace(value)
	cleaned = strings.ReplaceAll(cleaned, `\/`, "/")
	cleaned = strings.ReplaceAll(cleaned, `\u0026`, "&")
	cleaned = strings.ReplaceAll(cleaned, "&amp;", "&")
	return cleaned
}

// hgDrpyCheckedURL 对应 drpys checkedUrl：https、无凭据、端口 443、域名白名单。
func hgDrpyCheckedURL(value string, domains []string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("App 片源地址格式无效")
	}
	if parsed.User != nil || (parsed.Port() != "" && parsed.Port() != "443") {
		return "", errors.New("App 片源返回了不受支持的服务地址")
	}
	host := parsed.Hostname()
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return parsed.String(), nil
		}
	}
	return "", errors.New("App 片源返回了不受支持的服务地址")
}

// hgDrpyNormalizeDownloadQuality 对应 drpys normalizeDownloadQuality。
func hgDrpyNormalizeDownloadQuality(value string) (string, error) {
	if value == "" {
		value = "auto"
	}
	for _, quality := range hgDrpyDownloadQualities {
		if value == quality {
			return value, nil
		}
	}
	return "", errors.New("下载清晰度无效，请选择自动、1080p 或 720p")
}

// hgDrpyMeetsDownloadQuality 对应 drpys meetsDownloadQuality：
// auto 全放过；指定档位要求短边 >= 档位数（尺寸缺失不过）。
func hgDrpyMeetsDownloadQuality(width, height int, quality string) bool {
	if quality == "auto" {
		return true
	}
	threshold, err := strconv.Atoi(strings.TrimSuffix(quality, "p"))
	if err != nil {
		return false
	}
	if width <= 0 || height <= 0 {
		return false
	}
	short := width
	if height < short {
		short = height
	}
	return short >= threshold
}

// hgDrpyShortSide 取短边（drpys Math.min(width, height)）。
func hgDrpyShortSide(width, height int) int {
	if width <= 0 || height <= 0 {
		return 0
	}
	if height < width {
		return height
	}
	return width
}

// hgDrpyFirstString 实现 drpys str(a || b || ...) 的链式取值（空则下一个）。
func hgDrpyFirstString(values ...any) string {
	for _, value := range values {
		if text := hgDrpyStringOf(value); text != "" {
			return text
		}
	}
	return ""
}

// hgDrpyStringOf 对应 drpys str()：null→空串，其余 String() 后 trim。
func hgDrpyStringOf(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case float64:
		return strings.TrimSpace(strconv.FormatFloat(typed, 'f', -1, 64))
	case bool:
		return strconv.FormatBool(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

// hgDrpyNestedString 取嵌套 map 的字符串字段（drpys item.encrypt_info?.spade_a）。
func hgDrpyNestedString(item map[string]any, keys ...string) string {
	current := item
	for index, key := range keys {
		value, ok := current[key]
		if !ok {
			return ""
		}
		if index == len(keys)-1 {
			return hgDrpyStringOf(value)
		}
		next, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		current = next
	}
	return ""
}

// hgDrpyNumber 解析 JSON 数字/数字字符串为 float64。
func hgDrpyNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return number, err == nil
	default:
		return 0, false
	}
}

// hgDrpyPositiveInt 解析正整数（drpys Number.isFinite && > 0）。
func hgDrpyPositiveInt(value any) (int, bool) {
	number, ok := hgDrpyNumber(value)
	if !ok || number <= 0 {
		return 0, false
	}
	return int(number), true
}

// hgDrpyFirstNumber 取字符串中第一段数字（drpys quality.match(/\d+/)?.[0]）。
func hgDrpyFirstNumber(value string) int {
	match := hongguoQualityNumber.FindString(value)
	if match == "" {
		return 0
	}
	number, err := strconv.Atoi(match)
	if err != nil {
		return 0
	}
	return number
}

// hgDrpyRequestJSON 发请求并解析 JSON（drpys requestJson 的最小必要语义）：
// 域名白名单、不自动跟随重定向（GET 最多 4 跳且每跳过白名单，POST 遇跳转拒绝）、
// 响应体限 2MB、非 200 报错。
func hgDrpyRequestJSON(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (map[string]any, error) {
	currentURL := rawURL
	for redirects := 0; redirects < 4; redirects++ {
		checked, err := hgDrpyCheckedURL(currentURL, hgDrpyAPIDomains)
		if err != nil {
			return nil, err
		}
		currentURL = checked
		request, err := http.NewRequestWithContext(ctx, method, currentURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		client := &http.Client{
			Timeout: hgDrpyMediaTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		response, err := client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("App 片源网络请求失败，请稍后重试")
		}
		if response.Body != nil {
			limited := io.LimitReader(response.Body, 2*1024*1024)
			payload, readErr := io.ReadAll(limited)
			response.Body.Close()
			if readErr != nil {
				return nil, errors.New("App 片源响应读取失败")
			}
			switch response.StatusCode {
			case http.StatusOK:
				// 设备级风控：HTTP 200 但内容为空。不判的话会掉进 JSON 解析失败，
				// 被归成 parse（"不会自愈"，30 分钟冷却）而非 risk（1 分钟）。
				if err := hongguoMediaBodyOrRisk(payload); err != nil {
					return nil, err
				}
				var parsed map[string]any
				decoder := json.NewDecoder(bytes.NewReader(payload))
				decoder.UseNumber()
				if err := decoder.Decode(&parsed); err != nil || parsed == nil {
					return nil, errors.New("App 接口未返回有效数据")
				}
				return parsed, nil
			case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
				// drpys：签名体不重发到跳转目标；POST 遇跳转直接拒绝。
				if method == http.MethodPost {
					return nil, errors.New("App 接口发生不支持的跳转")
				}
				location := response.Header.Get("Location")
				if location == "" {
					return nil, errors.New("App 接口发生不支持的跳转")
				}
				next, err := url.Parse(location)
				if err != nil {
					return nil, errors.New("App 片源地址格式无效")
				}
				currentURL = request.URL.ResolveReference(next).String()
				continue
			default:
				return nil, fmt.Errorf("App 片源请求失败（HTTP %d）", response.StatusCode)
			}
		}
		response.Body.Close()
	}
	return nil, errors.New("App 片源跳转次数过多")
}
