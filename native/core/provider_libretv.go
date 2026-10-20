package core

// LibreTV 格式片源（Apple CMS / MacCMS v10 provide/vod JSON 采集接口）。
//
// 背景：LibreTV / TVBox 生态的「片源链接」分两种形态：
//  1. 完整订阅配置：JSON，形如 {"sites":[{"key":"xx","name":"某某","type":1,
//     "api":"https://host/api.php/provide/vod"}]}（type=1 即 MacCMS 采集接口，
//     type=2 是 XML 采集接口；type≥3 的 drpy/spider 脚本地址会收成 drpy
//     候选，由用户在站源管理一键导入）；
//  2. 裸采集接口地址：形如 https://host/api.php/provide/vod，
//     LibreTV 里即一个 type=1 站点。
//
// 本文件把上述形态归一成运行时站源：源 ID 为 "libretv:<key>"，
// 剧集 ID 为 "libretv:<key>:<vodId>"，分集 ID 为 "libretv:<key>:<vodId>:<集键>"。
// 播放地址由 vod_play_url 直接给出（m3u8/mp4），复用既有的通用解析链路
// （resolveProviderMedia 对直链做 M3U8 拉取与密钥处理），不需要二级解析。
//
// 订阅持久化在引擎目录 libretv.json，导入时逐个探测（共享预算，避免拖垮
// 导入请求），探测失败的站点也会保留，用户可在站源管理里更新/检测/删除。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// sourceLibreTV LibreTV 站源家族名（完整源 ID 为 sourceLibreTV:<key>）。
	sourceLibreTV = "libretv"
	// sourceLibreTVPrefix 运行时 LibreTV 站源的 ID 前缀。
	sourceLibreTVPrefix = sourceLibreTV + ":"
	// libretvImportMaxSites 单次订阅导入的站点上限，防止异常配置刷爆列表。
	libretvImportMaxSites = 50
	// libretvProbeBudget 导入时探测总预算（所有站点共享）。
	libretvProbeBudget = 15 * time.Second
	// libretvProbePerSite 单个站点探测超时上限。
	libretvProbePerSite = 5 * time.Second
	// libretvSubscriptionMaxBytes 订阅内容大小上限（512KB）。
	libretvSubscriptionMaxBytes = 512 << 10
)

// libretvSite 一个已导入的 LibreTV 采集站点。
type libretvSite struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	API  string `json:"api"`
	// Type TVBox 站点类型：0/1/缺省 = MacCMS provide/vod JSON 采集接口，
	// 2 = MacCMS XML 采集接口（at/xml）。旧版 libretv.json 没有该字段，
	// 反序列化为 0，按 JSON 处理，向后兼容。
	Type int `json:"type,omitempty"`
}

// libretvNormalizeSiteType 归一化站点类型：只区分「XML(2)」与「JSON(其余)」。
func libretvNormalizeSiteType(siteType int) int {
	if siteType == 2 {
		return 2
	}
	return 1
}

// libretvParse 订阅 parses 数组里的一条解析规则。
//
// Type 沿用 TVBox 约定：0 = 嗅探（内置 WebView，桌面端不可用）、
// 1 = JSON 解析接口（需要第三方后端，我方按「可选项」支持）。
// 本项目扩展：Type == 2 且 URL 以 .js 结尾 = 本地脚本解析，
// 由 goja 执行脚本里的 parse(url)，**不是 TVBox 标准语义**，
// 标准配置里的 type=2 聚合解析不会被当成脚本。
type libretvParse struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	URL  string `json:"url"`
	// Ext 原样保留订阅里的 ext（对象/数组），仅存档展示，不参与执行。
	Ext json.RawMessage `json:"ext,omitempty"`
}

// libretvParseKind 返回解析方式的中文类型名（UI 展示用）。
// type 0 与 4 在 TVBox 里都是「嗅探」类，需要 WebView 实际加载播放页；
// 本实现由 Dart 侧 WebView 完成（Go 内没有浏览器环境）。
func libretvParseKind(parse libretvParse) string {
	switch {
	case libretvParseIsScript(parse):
		return "本地脚本"
	case parse.Type == 1:
		return "JSON 接口"
	case parse.Type == 0 || parse.Type == 4:
		return "WebView 嗅探"
	default:
		return "其他解析"
	}
}

// libretvParseNeedsWebView type 0 / 4：需要 WebView 加载播放页并拦截媒体请求。
// Dart 侧播放前会先嗅探，把章节地址替换为直链；Go 侧只负责报出明确错误。
func libretvParseNeedsWebView(parse libretvParse) bool {
	return parse.Type == 0 || parse.Type == 4
}

// libretvParseIsScript 判断是否为「本地解析脚本」扩展类型：
// type=2 且 url 是 http(s) 的 .js 地址。
func libretvParseIsScript(parse libretvParse) bool {
	if parse.Type != 2 {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(parse.URL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(parsed.Path), ".js")
}

// normalizeLibreTVParse 归一化订阅里的 parses 条目；返回 false 表示不可用
// （内嵌 WebView 嗅探、相对地址、空地址等）。
func normalizeLibreTVParse(raw libretvParse) (libretvParse, bool) {
	address := strings.TrimSpace(raw.URL)
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return libretvParse{}, false
	}
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		name = parsed.Host
	}
	parseType := raw.Type
	if parseType < 0 {
		parseType = 0
	}
	return libretvParse{Name: name, Type: parseType, URL: address, Ext: raw.Ext}, true
}

// looksLikeLibreTVXMLAPI 由接口地址判断是否更像 XML 采集接口。
// 苹果CMS 的 XML 版本通常写成 /at/xml/ 路径或带 at=xml / ac=xml 参数。
func looksLikeLibreTVXMLAPI(raw string) bool {
	lowered := strings.ToLower(strings.TrimSpace(raw))
	return strings.Contains(lowered, "/at/xml") ||
		strings.Contains(lowered, "at=xml") ||
		strings.Contains(lowered, "ac=xml")
}

// libretvStore 站源订阅表（进程内 + libretv.json 落盘）。
type libretvStore struct {
	directory string
	mu        chan struct{}
	sites     map[string]libretvSite
	// parses 订阅里的解析规则（按 URL 去重）。
	parses map[string]libretvParse
	// parseURL 当前选中的解析方式；空 = 直链/嗅探。
	parseURL string
}

func newLibreTVStore(directory string) *libretvStore {
	return &libretvStore{
		directory: directory,
		mu:        make(chan struct{}, 1),
		sites:     map[string]libretvSite{},
		parses:    map[string]libretvParse{},
	}
}

// loadLibreTVStore 从引擎目录读取已保存的订阅；文件缺失或损坏时返回空表。
func loadLibreTVStore(directory string) *libretvStore {
	store := newLibreTVStore(directory)
	body, err := os.ReadFile(filepath.Join(directory, "libretv.json"))
	if err != nil || len(body) > libretvSubscriptionMaxBytes {
		return store
	}
	var payload struct {
		Sites    []libretvSite  `json:"sites"`
		Parses   []libretvParse `json:"parses"`
		ParseURL string         `json:"parseUrl"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return store
	}
	for _, site := range payload.Sites {
		if key, name, api, ok := normalizeLibreTVSite(site.Key, site.Name, site.API, site.Type); ok {
			store.sites[key] = libretvSite{Key: key, Name: name, API: api, Type: libretvNormalizeSiteType(site.Type)}
		}
	}
	for _, parse := range payload.Parses {
		if normalized, ok := normalizeLibreTVParse(parse); ok {
			store.parses[normalized.URL] = normalized
		}
	}
	// 选择已失效（对应解析被新订阅覆盖掉）时回到直链/嗅探。
	if _, ok := store.parses[strings.TrimSpace(payload.ParseURL)]; ok {
		store.parseURL = strings.TrimSpace(payload.ParseURL)
	}
	return store
}

// saveLocked 写盘（调用方需持有 mu）。
func (store *libretvStore) saveLocked() error {
	keys := make([]string, 0, len(store.sites))
	for key := range store.sites {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	payload := struct {
		Sites    []libretvSite  `json:"sites"`
		Parses   []libretvParse `json:"parses,omitempty"`
		ParseURL string         `json:"parseUrl,omitempty"`
	}{
		Sites:    make([]libretvSite, 0, len(keys)),
		ParseURL: store.parseURL,
	}
	for _, key := range keys {
		payload.Sites = append(payload.Sites, store.sites[key])
	}
	urls := make([]string, 0, len(store.parses))
	for address := range store.parses {
		urls = append(urls, address)
	}
	sort.Strings(urls)
	for _, address := range urls {
		payload.Parses = append(payload.Parses, store.parses[address])
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writeNativeCacheFile(filepath.Join(store.directory, "libretv.json"), body)
}

func (store *libretvStore) lock()   { store.mu <- struct{}{} }
func (store *libretvStore) unlock() { <-store.mu }

// get 按完整源 ID（libretv:<key>）取站点。
func (store *libretvStore) get(source string) (libretvSite, bool) {
	key := libretvKeyFromSource(source)
	if key == "" {
		return libretvSite{}, false
	}
	store.lock()
	defer store.unlock()
	site, ok := store.sites[key]
	return site, ok
}

// put 登记站点并落盘；返回是否新增（false 表示已存在同 key 站点被覆盖）。
func (store *libretvStore) put(site libretvSite) (bool, error) {
	store.lock()
	defer store.unlock()
	_, existed := store.sites[site.Key]
	store.sites[site.Key] = site
	return !existed, store.saveLocked()
}

// remove 删除站点并落盘。
func (store *libretvStore) remove(key string) error {
	store.lock()
	defer store.unlock()
	if _, ok := store.sites[key]; !ok {
		return nil
	}
	delete(store.sites, key)
	return store.saveLocked()
}

// list 返回全部站点（按 key 排序，稳定输出）。
func (store *libretvStore) list() []libretvSite {
	store.lock()
	defer store.unlock()
	keys := make([]string, 0, len(store.sites))
	for key := range store.sites {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]libretvSite, 0, len(keys))
	for _, key := range keys {
		out = append(out, store.sites[key])
	}
	return out
}

// libretvSite 按完整源 ID（libretv:<key>）取站点；Downloader 便捷方法。
func (d *Downloader) libretvSite(source string) (libretvSite, bool) {
	if d.libretv == nil {
		return libretvSite{}, false
	}
	return d.libretv.get(source)
}

// putParses 合并订阅里的解析规则（按 URL 去重，新条目覆盖旧条目）并落盘。
func (store *libretvStore) putParses(parses []libretvParse) error {
	if len(parses) == 0 {
		return nil
	}
	store.lock()
	defer store.unlock()
	for _, parse := range parses {
		normalized, ok := normalizeLibreTVParse(parse)
		if !ok {
			continue
		}
		store.parses[normalized.URL] = normalized
	}
	return store.saveLocked()
}

// listParses 返回全部解析规则（按名称 + URL 排序，输出稳定）。
func (store *libretvStore) listParses() []libretvParse {
	store.lock()
	items := make([]libretvParse, 0, len(store.parses))
	for _, parse := range store.parses {
		items = append(items, parse)
	}
	store.unlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].URL < items[j].URL
	})
	return items
}

// selectedParse 返回当前选中的解析方式；空 URL 表示直链/嗅探。
func (store *libretvStore) selectedParse() (libretvParse, bool) {
	store.lock()
	defer store.unlock()
	if store.parseURL == "" {
		return libretvParse{}, false
	}
	parse, ok := store.parses[store.parseURL]
	return parse, ok
}

// selectedParseURL 返回当前选中的解析 URL（空 = 直链/嗅探）。
func (store *libretvStore) selectedParseURL() string {
	store.lock()
	defer store.unlock()
	return store.parseURL
}

// setSelectedParse 持久化解析方式选择；空串 = 直链/嗅探。
// 选择不存在的解析时返回错误，避免 UI 与存储状态漂移。
func (store *libretvStore) setSelectedParse(address string) error {
	address = strings.TrimSpace(address)
	store.lock()
	defer store.unlock()
	if address != "" {
		if _, ok := store.parses[address]; !ok {
			return errors.New("该解析方式不在已导入的订阅里，请重新导入订阅")
		}
	}
	if store.parseURL == address {
		return nil
	}
	store.parseURL = address
	return store.saveLocked()
}

// Downloader 便捷方法：当前选中的解析方式。
func (d *Downloader) libretvSelectedParse() (libretvParse, bool) {
	if d.libretv == nil {
		return libretvParse{}, false
	}
	return d.libretv.selectedParse()
}

// libretvKeyFromSource 从完整源 ID 取出站点 key；不是 libretv 源时返回空串。
func libretvKeyFromSource(source string) string {
	trimmed := strings.TrimSpace(source)
	if !strings.HasPrefix(trimmed, sourceLibreTVPrefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, sourceLibreTVPrefix))
}

// libretvSourceID 站点 key → 完整源 ID。
func libretvSourceID(key string) string {
	return sourceLibreTVPrefix + strings.TrimSpace(key)
}

var libretvKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// normalizeLibreTVSite 校验并归一化站点字段：key 缺省时由接口地址派生，
// name 缺省时回落为接口主机名，api 统一去掉末尾斜杠，type 归一为 JSON/XML 两档。
func normalizeLibreTVSite(rawKey, rawName, rawAPI string, siteType int) (key, name, api string, ok bool) {
	api = strings.TrimSpace(rawAPI)
	parsed, err := url.Parse(api)
	if err != nil || !isProviderHTTPMediaURL(api) || parsed.Path == "" {
		return "", "", "", false
	}
	api = strings.TrimRight(api, "/")
	name = strings.TrimSpace(rawName)
	if name == "" || len([]rune(name)) > 48 {
		if name == "" {
			name = parsed.Hostname()
		} else {
			name = string([]rune(name)[:48])
		}
	}
	if name == "" {
		name = "LibreTV 片源"
	}
	key = strings.TrimSpace(rawKey)
	if !libretvKeyPattern.MatchString(key) {
		key = libretvKeyFromAPI(api)
	}
	return key, name, api, true
}

// libretvKeyFromAPI 由接口地址派生稳定 key：主机名 slug + 地址哈希前 6 位。
func libretvKeyFromAPI(api string) string {
	host := "site"
	if parsed, err := url.Parse(api); err == nil && parsed.Hostname() != "" {
		host = parsed.Hostname()
	}
	slug := regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(host), "")
	if slug == "" {
		slug = "site"
	}
	if len(slug) > 16 {
		slug = slug[:16]
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(api))))
	return slug + "-" + hex.EncodeToString(sum[:])[:6]
}

// libretvConfigSite LibreTV 订阅配置里的 sites 条目。
type libretvConfigSite struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	API  string `json:"api"`
	Type int    `json:"type"`
	// Ext TVBox type≥3 的扩展字段：drpy 站点里 api 是运行时库
	// （drpy2.min.js），ext 才是真正的站点脚本地址。也可能是对象或
	// JSON 规则文件（csp_ 规则），此时不参与脚本候选。
	Ext json.RawMessage `json:"ext"`
	// Jar Java 版 spider 包地址（TVBox 老格式），本实现不支持。
	Jar string `json:"jar"`
}

// libretvConfigExtText 取出 ext 里的字符串；ext 是对象/数组时返回空。
func libretvConfigExtText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return strings.TrimSpace(text)
}

// libretvConfigExtScriptURL ext 里的脚本地址：只有「字符串且是 http(s)
// 的 .js」才算脚本。
func libretvConfigExtScriptURL(raw json.RawMessage) string {
	text := libretvConfigExtText(raw)
	if !looksLikeDrpyScriptURL(text) {
		return ""
	}
	return text
}

// libretvConfigScriptCandidate 决定 type≥3 条目的脚本候选地址。
// TVBox 的 drpy 站点约定：api 是运行时（drpy2.min.js，ES Module，
// goja 无法编译），ext 才是站点脚本（var rule + 解析函数）。
// 因此优先取 ext，缺失时退回 api（drpy-node 等直接以脚本为 api 的变体）；
// 但 api 明显是 drpy 运行时库时直接放弃，避免把库当脚本导入。
func libretvConfigScriptCandidate(entry libretvConfigSite) string {
	if script := libretvConfigExtScriptURL(entry.Ext); script != "" {
		return script
	}
	api := strings.TrimSpace(entry.API)
	if looksLikeDrpyRuntimeLibrary(api) {
		return ""
	}
	return api
}

// libretvConfigVodAPI 判断 type≥3 条目是否可以直接当成 MacCMS 采集接口
// 导入（JSON 采集，必要时按响应回退 XML）。
//
// 生态里两类常见情况：
//   - type=4：T4 / CatVod / drpy-node 服务器，api 本身就是一个 vod 接口地址，
//     响应结构与 MacCMS provide/vod 同构（code/msg/page/list + vod_* 字段）；
//   - type=3 且 api=csp_AppYsV2：苹果CMS 二开的 /v1.vod 接口，接口地址在
//     ext 里，同样是 MacCMS 结构。
//
// 其余 csp_* 的 ext 多为站点首页（不是接口）或专属 Java 爬虫的参数，
// 一律返回空，避免把首页当接口导入后永远取不到数据。
func libretvConfigVodAPI(entry libretvConfigSite) string {
	if strings.TrimSpace(entry.Jar) != "" {
		return ""
	}
	address := ""
	switch {
	case entry.Type == 4:
		address = strings.TrimSpace(entry.API)
	case entry.Type == 3 && strings.TrimSpace(entry.API) == "csp_AppYsV2":
		address = libretvConfigExtText(entry.Ext)
	default:
		return ""
	}
	if !isProviderHTTPMediaURL(address) {
		return ""
	}
	// 站点首页（https://www.xxx.com/）不算采集接口：需要带 vod / api 之类
	// 的采集路径，否则导入后永远取不到数据。
	lowered := strings.ToLower(address)
	if !strings.Contains(lowered, "vod") &&
		!strings.Contains(lowered, "/api") &&
		!strings.Contains(lowered, "api.php") {
		return ""
	}
	return address
}

// libretvUnsupportedReason type≥3 条目不可用时的原因。
// TVBox 的 type=3 里混着多种形态（drpy 脚本、csp_ 规则、jar 包、py 源），
// 统一报「类型不支持」没法判断是哪一种，这里按条目内容给出具体说明。
func libretvUnsupportedReason(entry libretvConfigSite) string {
	api := strings.TrimSpace(entry.API)
	ext := libretvConfigExtText(entry.Ext)
	switch {
	case strings.TrimSpace(entry.Jar) != "":
		return "jar / spider 包源暂不支持（仅支持 drpy JS 脚本）"
	case strings.HasPrefix(api, "csp_"):
		// 点名具体的 Java Spider，方便判断要不要等支持。
		return fmt.Sprintf("%s 是 TVBox 内置 Java Spider，暂不支持（需要 jar / Java 运行环境）", api)
	case looksLikeDrpyRuntimeLibrary(api):
		return "只有 drpy 运行时库（api），缺少站点脚本（ext）"
	case ext != "" && !looksLikeDrpyScriptURL(ext):
		return "扩展配置 ext 不是 drpy 脚本（.js），暂不支持"
	default:
		return fmt.Sprintf("type=%d 类型暂不支持（仅支持 provide/vod 采集接口与 drpy 脚本）", entry.Type)
	}
}

// （drpy.min.js / drpy2.min.js / drpy2.js）。这类文件是 ES Module 打包产物，
// 不是站点脚本；只有在 ext 没有给出脚本时才用这个判定兜底。
func looksLikeDrpyRuntimeLibrary(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return false
	}
	name := strings.ToLower(path.Base(parsed.Path))
	if !strings.HasPrefix(name, "drpy") {
		return false
	}
	// 只认打包产物：drpy.min.js / drpy2.min.js / drpy2.js。
	// 裸 drpy.js 可能是站点脚本（生态里确有同名脚本），不做拦截。
	return strings.Contains(name, ".min.") || name == "drpy2.js"
}

// libretvDisplayName 配置条目的展示名（报错用）。
func libretvDisplayName(entry libretvConfigSite) string {
	if name := strings.TrimSpace(entry.Name); name != "" {
		return name
	}
	if key := strings.TrimSpace(entry.Key); key != "" {
		return key
	}
	if api := strings.TrimSpace(entry.API); api != "" {
		return api
	}
	return "未命名站点"
}

// libretvExtractJSONObject 从订阅内容里截取 JSON 对象：
// 兼容纯 JSON、var config = {...}; 之类的 JS 包裹、以及带前后缀杂文本的情况。
func libretvExtractJSONObject(body string) string {
	trimmed := strings.TrimSpace(body)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end <= start {
		return ""
	}
	return trimmed[start : end+1]
}

// drpyCandidate 混合订阅里发现的 drpy 脚本候选：type≥3 且 api 是 .js
// 脚本地址。不自动导入（脚本需要单独校验），由用户在站源管理一键导入。
type drpyCandidate struct {
	Name string
	API  string
}

// looksLikeDrpyScriptURL 判断订阅里的 api 是否像 drpy 脚本地址
// （http(s) + 路径 .js 结尾，容忍查询串）。
func looksLikeDrpyScriptURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(parsed.Path), ".js")
}

// parseLibreTVSubscription 解析 LibreTV 订阅配置，返回可导入站点、drpy
// 脚本候选、解析规则（parses）与被跳过原因。type≥3 且 api 是 .js 脚本
// 地址的收成 drpy 候选（用户在站源管理一键导入）；type≥3 的非脚本地址
// （csp_ 规则等）与历史行为一致带原因跳过；type=0/1/缺省按 JSON 采集接口
// 导入，type=2 按 XML 采集接口导入（TVBox 的苹果CMS XML 版本）。
// baseURL 为订阅自身的地址，用于把 CatVod 蜘蛛常见的相对写法
// （./cat/xxx_open.js）补全成完整地址后再判定候选。
func parseLibreTVSubscription(body, baseURL string) (sites []libretvSite, drpyCandidates []drpyCandidate, parses []libretvParse, skipped []string, err error) {
	document := libretvExtractJSONObject(body)
	if document == "" {
		return nil, nil, nil, nil, errors.New("未找到有效的 LibreTV 配置（需要包含 sites 的 JSON）")
	}
	var config struct {
		Sites  []libretvConfigSite `json:"sites"`
		Parses []libretvParse      `json:"parses"`
	}
	if json.Unmarshal([]byte(document), &config) != nil {
		return nil, nil, nil, nil, errors.New("配置解析失败，请确认是 LibreTV/TVBox 格式的 JSON")
	}
	// parses 与 sites 相互独立：只带解析规则的订阅也可以导入（解析方式本身
	// 对已导入的站点同样生效）。
	seenParse := map[string]bool{}
	for _, raw := range config.Parses {
		parse, ok := normalizeLibreTVParse(raw)
		if !ok {
			// TVBox 内置 WebView 嗅探（url 是 "Web" 之类的标记）在纯 Go 端
			// 无法执行，静默跳过——不打断导入，也不误导用户。
			continue
		}
		if seenParse[parse.URL] {
			continue
		}
		seenParse[parse.URL] = true
		parses = append(parses, parse)
	}
	if len(config.Sites) == 0 && len(parses) == 0 {
		return nil, nil, nil, nil, errors.New("配置里没有 sites 条目")
	}
	seenAPI := map[string]bool{}
	usedKey := map[string]bool{}
	for _, entry := range config.Sites {
		api := strings.TrimSpace(entry.API)
		if api == "" {
			skipped = append(skipped, fmt.Sprintf("%s：缺少 api 地址", libretvDisplayName(entry)))
			continue
		}
		if entry.Type >= 3 {
			// drpy/spider 脚本：优先取 ext（TVBox 约定 ext 是站点脚本，
			// api 是 drpy 运行时库）；其余（csp_ 规则等）不支持。
			candidate := libretvConfigScriptCandidate(entry)
			// 相对路径（CatVod 蜘蛛的 ./cat/xxx.js）：按订阅地址补全。
			if !isProviderHTTPMediaURL(candidate) {
				if resolved := catVodResolveURL(baseURL, candidate); resolved != "" {
					candidate = resolved
				}
			}
			if looksLikeDrpyScriptURL(candidate) && !seenAPI[candidate] {
				drpyCandidates = append(drpyCandidates, drpyCandidate{Name: libretvDisplayName(entry), API: candidate})
				seenAPI[candidate] = true
				continue
			}
			// 同一脚本地址重复出现：静默去重，不刷跳过原因。
			if looksLikeDrpyScriptURL(candidate) {
				continue
			}
			// type=4 服务器 / csp_AppYsV2：接口地址可直接按 MacCMS 采集站导入。
			if api := libretvConfigVodAPI(entry); api != "" {
				key, name, normalized, ok := normalizeLibreTVSite(entry.Key, entry.Name, api, 1)
				if ok && !seenAPI[normalized] {
					for usedKey[key] { // key 冲突时退化为地址派生 key
						key = libretvKeyFromAPI(normalized + "/" + strconv.Itoa(len(usedKey)))
					}
					seenAPI[normalized] = true
					usedKey[key] = true
					sites = append(sites, libretvSite{Key: key, Name: name, API: normalized, Type: 1})
					if len(sites) >= libretvImportMaxSites {
						skipped = append(skipped, fmt.Sprintf("站点数量超过 %d 个，仅导入前 %d 个", libretvImportMaxSites, libretvImportMaxSites))
						break
					}
					continue
				}
			}
			skipped = append(
				skipped,
				fmt.Sprintf("%s：%s", libretvDisplayName(entry), libretvUnsupportedReason(entry)),
			)
			continue
		}
		key, name, normalized, ok := normalizeLibreTVSite(entry.Key, entry.Name, api, entry.Type)
		if !ok {
			skipped = append(skipped, fmt.Sprintf("%s：接口地址无效", libretvDisplayName(entry)))
			continue
		}
		if seenAPI[normalized] {
			continue // 同一接口重复出现，静默去重
		}
		for usedKey[key] { // key 冲突时退化为地址派生 key
			key = libretvKeyFromAPI(normalized + "/" + strconv.Itoa(len(usedKey)))
		}
		seenAPI[normalized] = true
		usedKey[key] = true
		sites = append(sites, libretvSite{Key: key, Name: name, API: normalized, Type: libretvNormalizeSiteType(entry.Type)})
		if len(sites) >= libretvImportMaxSites {
			skipped = append(skipped, fmt.Sprintf("站点数量超过 %d 个，仅导入前 %d 个", libretvImportMaxSites, libretvImportMaxSites))
			break
		}
	}
	// 只有 drpy 候选 / 解析规则时不算失败：用户可一键导入脚本源或直接用解析。
	if len(sites) == 0 && len(drpyCandidates) == 0 && len(parses) == 0 {
		return nil, nil, nil, skipped, errors.New("配置里没有可导入的采集接口站点")
	}
	return sites, drpyCandidates, parses, skipped, nil
}

// looksLikeMacCMSAPI 判断裸地址是否像 MacCMS 采集接口（用于配置解析失败的兜底）。
func looksLikeMacCMSAPI(raw string) bool {
	lowered := strings.ToLower(raw)
	return strings.Contains(lowered, "provide/vod") ||
		strings.Contains(lowered, "api.php") ||
		strings.HasSuffix(lowered, "/api")
}

// importLibreTVSubscription 导入订阅：输入可以是配置链接、配置 JSON 文本，
// 或裸 provide/vod 接口地址。导入后按共享预算逐个探测，探测失败的站点同样保留
// （用户可在站源管理里更新/检测/删除），避免一次网络抖动整批丢弃。
func (engine *nativeEngine) importLibreTVSubscription(ctx context.Context, raw string) (any, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, errors.New("请输入 LibreTV 片源链接或配置内容")
	}
	if len(text) > libretvSubscriptionMaxBytes {
		return nil, errors.New("内容过大，请确认粘贴的是片源配置")
	}
	d := engine.downloader
	var body string
	if isProviderHTTPMediaURL(text) && !strings.ContainsAny(text, "\n\r") {
		fetched, err := d.fetchProviderText(ctx, text, text)
		if err != nil {
			return nil, fmt.Errorf("片源链接获取失败：%w", publicError(err))
		}
		body = fetched
	} else {
		body = text
	}
	configURL := ""
	if isProviderHTTPMediaURL(text) && !strings.ContainsAny(text, "\n\r") {
		configURL = text
	}
	sites, drpyCandidates, parses, skipped, err := parseLibreTVSubscription(body, configURL)
	if err != nil && looksLikeLibreTVBase58(body) {
		// 社区配置工具（LunaTV-config 等）的 Base58 订阅：format=2 是原始 JSON、
		// format=3 是带代理前缀的 JSON，解码后复用同一套解析，客户端不需要
		// 实时解析 type≥3 脚本，也不需要自建后端。
		if decoded, decodeErr := libretvBase58Decode(body); decodeErr == nil {
			decodedSites, decodedDrpy, decodedParses, decodedSkipped, decodedErr := parseLibreTVSubscription(string(decoded), configURL)
			// 部分站点不可用时也保留跳过原因（例如配置里混了 drpy 源）。
			if decodedErr == nil || len(decodedSkipped) > 0 {
				sites, drpyCandidates, parses, skipped, err = decodedSites, decodedDrpy, decodedParses, decodedSkipped, decodedErr
			}
		}
	}
	if err != nil {
		// 兜底：输入本身就是裸采集接口地址时，按单站点导入。
		// 地址形如 /at/xml 的按 XML 采集站处理，其余按 JSON 采集站处理。
		if isProviderHTTPMediaURL(text) && looksLikeMacCMSAPI(text) {
			siteType := 1
			if looksLikeLibreTVXMLAPI(text) {
				siteType = 2
			}
			if key, name, api, ok := normalizeLibreTVSite("", "", text, siteType); ok {
				sites, skipped, err = []libretvSite{{Key: key, Name: name, API: api, Type: siteType}}, nil, nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	store := d.libretv
	// 解析规则先入库：即使本次没有新站点，用户也能在站源管理里选择解析方式。
	if err := store.putParses(parses); err != nil {
		skipped = append(skipped, fmt.Sprintf("解析规则保存失败（%s）", publicError(err)))
	}
	added := make([]map[string]any, 0, len(sites))
	verified := 0
	budget := libretvProbeBudget
	for _, site := range sites {
		if _, err := store.put(site); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s：保存失败（%s）", site.Name, publicError(err)))
			continue
		}
		ok := false
		if budget > 0 {
			per := libretvProbePerSite
			if per > budget {
				per = budget
			}
			started := time.Now()
			probeCtx, cancel := context.WithTimeout(ctx, per)
			// 直接走目录链路：既验证接口，又把首页写进缓存，
			// 导入完成后站源管理里立刻能看到剧集数。
			page, probeErr := engine.nativeCatalog(probeCtx, nativeInput{Source: libretvSourceID(site.Key), Page: 1, Force: true})
			cancel()
			budget -= time.Since(started)
			ok = probeErr == nil && len(page.Items) > 0 && page.Warning == ""
			if ok {
				verified++
			}
		}
		added = append(added, map[string]any{"key": site.Key, "name": site.Name, "api": site.API, "type": site.Type, "verified": ok})
	}
	// 订阅变更后 Dart 侧会重新拉取列表，无需额外广播。
	// drpy 候选不自动导入（脚本需要用户确认后才加入），仅透传给 Dart 侧展示。
	drpy := make([]map[string]any, 0, len(drpyCandidates))
	for _, candidate := range drpyCandidates {
		drpy = append(drpy, map[string]any{"name": candidate.Name, "api": candidate.API})
	}
	// 解析规则已入库；这里同样回传一份，导入结果对话框可直接展示。
	importedParses := make([]map[string]any, 0, len(parses))
	for _, parse := range parses {
		importedParses = append(importedParses, map[string]any{
			"name": parse.Name,
			"type": parse.Type,
			"url":  parse.URL,
			"kind": libretvParseKind(parse),
		})
	}
	return map[string]any{
		"added":    added,
		"skipped":  skipped,
		"verified": verified,
		"total":    len(added),
		"drpy":     drpy,
		"parses":   importedParses,
	}, nil
}

// removeLibreTVSite 删除已导入站点，并清掉它的目录缓存与任务记录。
func (engine *nativeEngine) removeLibreTVSite(key string) error {
	key = strings.TrimSpace(key)
	if key == "" || !libretvKeyPattern.MatchString(key) {
		return errors.New("站点 key 无效")
	}
	store := engine.downloader.libretv
	if _, ok := store.get(libretvSourceID(key)); !ok {
		return errors.New("站点不存在或已删除")
	}
	engine.mu.Lock()
	source := libretvSourceID(key)
	if task := engine.sourceTasks[source]; task != nil {
		task.cancel()
		delete(engine.sourceTasks, source)
	}
	for catalogKey := range engine.catalogs {
		if catalogKey == source || strings.HasPrefix(catalogKey, source+"|") {
			delete(engine.catalogs, catalogKey)
		}
	}
	for stateKey := range engine.catalogStates {
		if stateKey == source || strings.HasPrefix(stateKey, source+"|") {
			delete(engine.catalogStates, stateKey)
		}
	}
	for optionKey := range engine.categoryOptions {
		if optionKey == source || strings.HasPrefix(optionKey, source+"|") {
			delete(engine.categoryOptions, optionKey)
		}
	}
	delete(engine.sourceRecords, source)
	saveErr := engine.writeCatalogDiskLocked()
	engine.mu.Unlock()
	if err := store.remove(key); err != nil {
		return err
	}
	engine.saveSourceRecordsFile()
	if saveErr != nil {
		return saveErr
	}
	// 订阅变更后 Dart 侧会重新拉取列表，无需额外广播。
	return nil
}

// saveSourceRecordsFile 立即持久化站源任务记录（删除站点后调用）。
func (engine *nativeEngine) saveSourceRecordsFile() {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.saveSourceRecordsLocked()
}

// libretvSiteList 供 libretvSites 操作返回：站点 + 当前缓存剧集数。
func (engine *nativeEngine) libretvSiteList() []map[string]any {
	store := engine.downloader.libretv
	engine.mu.Lock()
	counts := map[string]int{}
	for catalogKey, items := range engine.catalogs {
		key := libretvKeyFromSource(catalogKey)
		if key == "" {
			continue
		}
		counts[key] += len(items)
	}
	engine.mu.Unlock()
	out := make([]map[string]any, 0)
	for _, site := range store.list() {
		out = append(out, map[string]any{
			"key":   site.Key,
			"name":  site.Name,
			"api":   site.API,
			"type":  site.Type,
			"count": counts[site.Key],
		})
	}
	return out
}

// libretvParseConfig 供 libretvParseConfig 操作返回：已导入的解析方式列表
// 与当前选择。cached 表示本地脚本是否已缓存（远程脚本首次使用才下载）。
func (engine *nativeEngine) libretvParseConfig() map[string]any {
	store := engine.downloader.libretv
	items := store.listParses()
	out := make([]map[string]any, 0, len(items))
	for _, parse := range items {
		cached := false
		if libretvParseIsScript(parse) {
			_, err := os.Stat(filepath.Join(engine.directory, libretvParseScriptFile(parse)))
			cached = err == nil
		}
		out = append(out, map[string]any{
			"name":   parse.Name,
			"type":   parse.Type,
			"url":    parse.URL,
			"kind":   libretvParseKind(parse),
			"script": libretvParseIsScript(parse),
			"cached": cached,
		})
	}
	return map[string]any{"parses": out, "selected": store.selectedParseURL()}
}

// libretvSetParse 切换解析方式；空 URL = 直链/嗅探。
func (engine *nativeEngine) libretvSetParse(address string) (map[string]any, error) {
	if err := engine.downloader.libretv.setSelectedParse(address); err != nil {
		return nil, err
	}
	return map[string]any{"selected": engine.downloader.libretv.selectedParseURL()}, nil
}

// macCMSResponse MacCMS provide/vod 通用响应信封。
type macCMSResponse struct {
	Code      any    `json:"code"`
	Msg       string `json:"msg"`
	Page      any    `json:"page"`
	PageCount any    `json:"pagecount"`
	Limit     any    `json:"limit"`
	Total     any    `json:"total"`
	List      []struct {
		VodID       any    `json:"vod_id"`
		VodName     string `json:"vod_name"`
		VodPic      string `json:"vod_pic"`
		TypeName    string `json:"type_name"`
		VodRemarks  string `json:"vod_remarks"`
		VodYear     string `json:"vod_year"`
		VodArea     string `json:"vod_area"`
		VodLang     string `json:"vod_lang"`
		VodHits     any    `json:"vod_hits"`
		VodScore    any    `json:"vod_score"`
		VodActor    string `json:"vod_actor"`
		VodDirector string `json:"vod_director"`
		VodContent  string `json:"vod_content"`
		VodPlayFrom string `json:"vod_play_from"`
		VodPlayURL  string `json:"vod_play_url"`
		VodTime     string `json:"vod_time"`
		VodTag      string `json:"vod_tag"`
	} `json:"list"`
	// Class ac=list 的分类列表（type_id/type_name）。
	Class []struct {
		TypeID   any    `json:"type_id"`
		TypeName string `json:"type_name"`
	} `json:"class"`
}

// macCMSListRow 归一化后的列表行（detail/list 两处复用）。
type macCMSListRow struct {
	ID        string
	Name      string
	Pic       string
	TypeName  string
	Remarks   string
	Year      string
	Area      string
	Lang      string
	Hits      string
	Score     string
	Actor     string
	Director  string
	UpdatedAt string
	Tag       string
}

// libretvScoreText 评分归一化：MacCMS 的 vod_score 有的站返回数字、有的返回字符串。
// 不能复用 nativeText——它对 float64 走 strconv.Itoa(int(v)) 截断，8.6 会变成 "8"。
// 0 / 0.0 视为「未评分」返回空串，避免详情页显示「0 分」这种占位。
func libretvScoreText(value any) string {
	text := ""
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		text = strings.TrimSpace(v)
	case json.Number:
		text = strings.TrimSpace(string(v))
	case float64:
		// 'f' + -1 精度：8.6 → "8.6"，8 → "8"，不产生多余尾零。
		text = strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		text = strconv.Itoa(v)
	case int64:
		text = strconv.FormatInt(v, 10)
	default:
		text = strings.TrimSpace(nativeText(value))
	}
	switch text {
	case "", "0", "0.0", "0.00", "<nil>":
		return ""
	}
	return text
}

func (response *macCMSResponse) rows() []macCMSListRow {
	out := make([]macCMSListRow, 0, len(response.List))
	for _, row := range response.List {
		id := strings.TrimSpace(nativeText(row.VodID))
		if id == "" {
			continue
		}
		out = append(out, macCMSListRow{
			ID:        id,
			Name:      strings.TrimSpace(row.VodName),
			Pic:       strings.TrimSpace(row.VodPic),
			TypeName:  strings.TrimSpace(row.TypeName),
			Remarks:   strings.TrimSpace(row.VodRemarks),
			Year:      strings.TrimSpace(row.VodYear),
			Area:      strings.TrimSpace(row.VodArea),
			Lang:      strings.TrimSpace(row.VodLang),
			Hits:      strings.TrimSpace(nativeText(row.VodHits)),
			Score:     libretvScoreText(row.VodScore),
			Actor:     strings.TrimSpace(row.VodActor),
			Director:  strings.TrimSpace(row.VodDirector),
			UpdatedAt: strings.TrimSpace(row.VodTime),
			Tag:       strings.TrimSpace(row.VodTag),
		})
	}
	return out
}

// macCMSSuccess MacCMS 成功码：规范是 code=1，部分新站用 200。
func (response *macCMSResponse) macCMSSuccess() bool {
	switch value := response.Code.(type) {
	case float64:
		return value == 1 || value == 200
	case string:
		return value == "1" || value == "200"
	}
	return false
}

// errLibreTVNotJSON 接口返回的不是 JSON —— 用于触发 XML 格式回退。
var errLibreTVNotJSON = errors.New("片源接口返回的不是有效 JSON，可能不是 provide/vod 采集接口")

// libretvAPI 请求一次 provide/vod 接口并解码信封。
func (d *Downloader) libretvAPI(ctx context.Context, site libretvSite, params map[string]string) (*macCMSResponse, error) {
	address, err := url.Parse(site.API)
	if err != nil {
		return nil, errors.New("片源接口地址无效")
	}
	query := address.Query()
	for key, value := range params {
		query.Set(key, value)
	}
	address.RawQuery = query.Encode()
	referer := address.Scheme + "://" + address.Host + "/"
	body, err := d.fetchProviderText(ctx, address.String(), referer)
	if err != nil {
		return nil, err
	}
	var response macCMSResponse
	if json.Unmarshal([]byte(body), &response) != nil {
		return nil, errLibreTVNotJSON
	}
	if !response.macCMSSuccess() && len(response.List) == 0 {
		msg := strings.TrimSpace(response.Msg)
		if msg == "" {
			msg = "接口返回异常"
		}
		return nil, errors.New(msg)
	}
	return &response, nil
}

// libretvRemarksStatus 从「更新至10集 / 全30集 / 已完结」推断连载状态。
var libretvFinishedPattern = regexp.MustCompile(`(?i)(完结|全集|全\s*\d+\s*[集话期]|已完结)`)

func libretvRemarksStatus(remarks string) string {
	switch {
	case libretvFinishedPattern.MatchString(remarks):
		return "finished"
	case strings.Contains(remarks, "更新至") || strings.Contains(remarks, "更新"):
		return "ongoing"
	default:
		return ""
	}
}

// libretvDramaFromRow 列表行 → Drama（详情接口的同名字段直接复用）。
func libretvDramaFromRow(source string, row macCMSListRow, content string) Drama {
	count := episodeIndex(row.Remarks, 0)
	return Drama{
		ID:            providerDramaID(source, row.ID),
		Source:        source,
		SourceID:      row.ID,
		Title:         row.Name,
		Name:          row.Name,
		Cover:         row.Pic,
		TypeName:      row.TypeName,
		Remark:        row.Remarks,
		Score:         row.Score,
		Actor:         row.Actor,
		Director:      row.Director,
		Views:         row.Hits,
		OnlineDate:    row.Year,
		Episodes:      count,
		Desc:          strings.TrimSpace(content),
		Tags:          libretvTags(row),
		ReleaseStatus: libretvRemarksStatus(row.Remarks),
		UpdateTime:    libretvUpdateTime(row.UpdatedAt),
	}
}

// libretvTags 详情页标签：vod_tag 是站点自带的分类标签（逗号分隔），
// 优先保留；再补地区 / 语言 / 年份这些通用维度。去重后最多 12 个。
func libretvTags(row macCMSListRow) []string {
	var tags []string
	seen := map[string]bool{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || strings.Contains(value, " ") || seen[value] {
			return
		}
		seen[value] = true
		tags = append(tags, value)
	}
	for _, tag := range strings.FieldsFunc(row.Tag, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == '/' || r == '|'
	}) {
		add(tag)
	}
	for _, value := range []string{row.Area, row.Lang, row.Year} {
		add(value)
	}
	if len(tags) > 12 {
		tags = tags[:12]
	}
	return tags
}

// libretvUpdateTime vod_time 归一化：站点常见 "2026-10-19 20:00:00"、
// "2026-10-19 20:00"、时间戳与空串。只保留展示需要的部分，
// 解析失败时原样返回（截断到 19 字符），供详情页直接展示。
func libretvUpdateTime(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	// 纯数字时间戳（秒或毫秒）。
	if digits := text; len(digits) >= 10 && len(digits) <= 13 {
		allDigits := true
		for _, r := range digits {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			var sec int64
			if len(digits) > 10 {
				sec = mustParseInt64(digits) / 1000
			} else {
				sec = mustParseInt64(digits)
			}
			if sec > 0 {
				return time.Unix(sec, 0).Format("2006-01-02 15:04")
			}
		}
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return parsed.Format("2006-01-02 15:04")
		}
	}
	if len(text) > 19 {
		return text[:19]
	}
	return text
}

// mustParseInt64 只用于已确认全为数字的串。
func mustParseInt64(text string) int64 {
	var value int64
	for _, r := range text {
		value = value*10 + int64(r-'0')
	}
	return value
}

// libretvHTMLText 去掉 vod_content 里的 HTML 标签与实体，压缩空白。
var libretvHTMLTag = regexp.MustCompile(`(?is)<[^>]+>`)

func libretvHTMLText(raw string) string {
	text := libretvHTMLTag.ReplaceAllString(raw, " ")
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	return strings.Join(strings.Fields(text), " ")
}

// fetchLibreTVCatalogPage 拉取一页列表/搜索结果。
// category 为 MacCMS type_id（数字），query 非空时走 wd 搜索。
func (d *Downloader) fetchLibreTVCatalogPage(ctx context.Context, site libretvSite, page int, category, query string) ([]Drama, bool, error) {
	if site.Type == 2 {
		return d.fetchLibreTVCatalogPageXML(ctx, site, page, category, query)
	}
	params := map[string]string{"ac": "detail"}
	requestedPage := page
	if page > 1 {
		params["pg"] = strconv.Itoa(page)
	}
	if category != "" {
		params["t"] = category
	}
	if query != "" {
		// 搜索固定回到第一页（部分站点 pg 与 wd 并存时会错页）。
		params["wd"] = query
		params["pg"] = "1"
		requestedPage = 1
	}
	response, err := d.libretvAPI(ctx, site, params)
	if err != nil {
		// 站点类型未标注（例如裸地址导入的 XML 接口）时，按响应格式回退。
		if errors.Is(err, errLibreTVNotJSON) {
			return d.fetchLibreTVCatalogPageXML(ctx, site, page, category, query)
		}
		return nil, false, err
	}
	source := libretvSourceID(site.Key)
	var dramas []Drama
	for _, row := range response.rows() {
		if row.Name == "" {
			continue
		}
		dramas = append(dramas, libretvDramaFromRow(source, row, ""))
	}
	hasMore := false
	if pageCount, _ := strconv.Atoi(nativeText(response.PageCount)); pageCount > 0 {
		hasMore = requestedPage < pageCount
	} else {
		limit, _ := strconv.Atoi(nativeText(response.Limit))
		if limit <= 0 {
			limit = 20
		}
		hasMore = len(dramas) >= limit
	}
	return dramas, hasMore, nil
}

// fetchLibreTVCategories 拉取分类（ac=list → class 列表）；站点不支持时只有「全部」。
func (d *Downloader) fetchLibreTVCategories(ctx context.Context, site libretvSite) ([]nativeCategory, error) {
	if site.Type == 2 {
		return d.fetchLibreTVCategoriesXML(ctx, site)
	}
	response, err := d.libretvAPI(ctx, site, map[string]string{"ac": "list"})
	if err != nil {
		if errors.Is(err, errLibreTVNotJSON) {
			return d.fetchLibreTVCategoriesXML(ctx, site)
		}
		return nil, err
	}
	var categories []nativeCategory
	for _, entry := range response.Class {
		id := strings.TrimSpace(nativeText(entry.TypeID))
		name := strings.TrimSpace(entry.TypeName)
		if id == "" || name == "" || len([]rune(name)) > 24 {
			continue
		}
		categories = append(categories, nativeCategory{ID: id, Name: name})
	}
	return categories, nil
}

// fetchLibreTVDetail 拉取详情：剧集元数据 + vod_play_url 解析出的分集。
// vod_play_from / vod_play_url 用 $$$ 分隔多个播放源，集与集之间用 # 分隔，
// 每集格式为「集名$播放地址」。多个播放源都保留（首选源失效时可换备用源），
// 集号重复时在标题后追记播放源名以便区分。
func (d *Downloader) fetchLibreTVDetail(ctx context.Context, site libretvSite, vodID string) (Drama, []Chapter, error) {
	vodID = strings.TrimSpace(vodID)
	if vodID == "" {
		return Drama{}, nil, errors.New("片源 ID 无效")
	}
	if site.Type == 2 {
		return d.fetchLibreTVDetailXML(ctx, site, vodID)
	}
	response, err := d.libretvAPI(ctx, site, map[string]string{"ac": "detail", "ids": vodID})
	if err != nil {
		if errors.Is(err, errLibreTVNotJSON) {
			return d.fetchLibreTVDetailXML(ctx, site, vodID)
		}
		return Drama{}, nil, err
	}
	rows := response.rows()
	if len(rows) == 0 {
		return Drama{}, nil, errors.New("片源接口没有返回该剧的详情")
	}
	row := rows[0]
	source := libretvSourceID(site.Key)
	first := response.List[0]
	drama := libretvDramaFromRow(source, row, libretvHTMLText(first.VodContent))
	referer := firstNonEmpty(libretvSiteReferer(site), site.API)
	playFrom := strings.Split(first.VodPlayFrom, "$$$")
	playGroups := strings.Split(first.VodPlayURL, "$$$")
	return drama, libretvChaptersFromGroups(source, vodID, referer, playFrom, playGroups), nil
}

// libretvChaptersFromGroups 把「播放源分组」解析成分集列表，JSON 与 XML 接口共用。
// JSON 接口用 $$$ 分隔多个播放源、# 分隔分集、$ 分隔集名与播放地址；
// XML 接口的每个 <dd flag="源名"> 对应一个播放源，内部同样是 # 与 $ 分隔。
// 多个播放源都保留（首选源失效时可换备用源），集号重复时在标题后追记播放源名。
func libretvChaptersFromGroups(source, vodID, referer string, playFrom, playGroups []string) []Chapter {
	var chapters []Chapter
	seenURL := map[string]bool{}
	seenEpisode := map[int]string{}
	for playIndex, group := range playGroups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		playName := ""
		if playIndex < len(playFrom) {
			playName = strings.TrimSpace(playFrom[playIndex])
		}
		episodeIndexInGroup := 0
		for _, entry := range strings.Split(group, "#") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			name, address, found := strings.Cut(entry, "$")
			if !found {
				continue
			}
			address = strings.TrimSpace(address)
			if !isProviderHTTPMediaURL(address) || seenURL[address] {
				continue
			}
			episodeIndexInGroup++
			seenURL[address] = true
			title := strings.TrimSpace(name)
			if title == "" {
				title = fmt.Sprintf("第%d集", episodeIndexInGroup)
			}
			number := episodeIndex(title, episodeIndexInGroup)
			if previous, duplicated := seenEpisode[number]; duplicated && playName != "" && previous != playName {
				// 同一集号来自不同播放源：追记源名，避免列表里出现完全相同的标题。
				title = fmt.Sprintf("%s·%s", title, playName)
			}
			seenEpisode[number] = playName
			chapters = append(chapters, Chapter{
				ID:             providerChapterID(source, vodID, fmt.Sprintf("%d-%d", playIndex, episodeIndexInGroup)),
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
