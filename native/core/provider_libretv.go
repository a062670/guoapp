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
	"sync"
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
	// libretvProbeWorkers 导入时并发探测的协程数。
	//
	// 一次订阅可能带几十个站点（上限 50），串行逐个探测会把 15 秒预算
	// 全部用满，表现出来就是「点导入后卡住」。并发 6 个后通常几秒内完成。
	// 不再往上加：片源站点多为小站，并发过高容易被风控。
	libretvProbeWorkers = 6
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
	// Engine 规则引擎（xbpq / hiker / bili）：非空表示该站点是规则型
	// Spider 源，数据抓取由 rules store 里的规则快照驱动，不走 MacCMS。
	// 空串 = 普通采集站（含旧版 libretv.json 的历史站点，天然兼容）。
	// 规则正文不在这里——见 provider_rules_store.go 的 ruleStore。
	Engine string `json:"engine,omitempty"`
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
// libretvParse 解析方式（订阅的 parses 段）。
//
// 字段类型刻意做成「宽松」：真实订阅里 type 有写成字符串的情况
// （影视仓聚合订阅就有 `"type": "1"`），用 int 会让**整份订阅**解析失败——
// 518 个站点因为一条解析规则报废。这里统一按 libretvFlexibleInt 取值。
type libretvParse struct {
	Name string         `json:"name"`
	Type libretvFlexInt `json:"type"`
	URL  string         `json:"url"`
	// Ext 原样保留订阅里的 ext（对象/数组），仅存档展示，不参与执行。
	Ext json.RawMessage `json:"ext,omitempty"`
}

// libretvFlexInt 容忍「数字或数字字符串」的整数字段。
// TVBox 订阅由人工维护，同一字段在不同仓里可能是 1 也可能是 "1"。
type libretvFlexInt int

func (v *libretvFlexInt) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*v = 0
		return nil
	}
	// 去掉字符串引号后按整数解析；解析不了就当 0，不阻断整份订阅。
	if strings.HasPrefix(text, `"`) {
		if unquoted, err := strconv.Unquote(text); err == nil {
			text = unquoted
		} else {
			text = strings.Trim(text, `"`)
		}
	}
	value, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		*v = 0
		return nil
	}
	*v = libretvFlexInt(value)
	return nil
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
		// 规则源的 API 只是「站点地址 / 展示用」，可能是裸域名（XBPQ 的主页url
		// 常是 https://host:端口），不能走采集站的「必须带路径」校验，
		// 否则重启后整条规则源会被静默丢弃。
		if engine := normalizeRuleEngine(site.Engine); engine != "" {
			key := strings.TrimSpace(site.Key)
			if !libretvKeyPattern.MatchString(key) {
				continue
			}
			name := strings.TrimSpace(site.Name)
			if name == "" {
				name = "规则片源"
			}
			store.sites[key] = libretvSite{
				Key:    key,
				Name:   name,
				API:    strings.TrimSpace(site.API),
				Type:   1,
				Engine: engine,
			}
			continue
		}
		if key, name, api, ok := normalizeLibreTVSite(site.Key, site.Name, site.API, site.Type); ok {
			store.sites[key] = libretvSite{
				Key:  key,
				Name: name,
				API:  api,
				Type: libretvNormalizeSiteType(site.Type),
			}
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
	// Type 同样是宽松整数：社区订阅里 type 有写成 "3" 的。
	Type libretvFlexInt `json:"type"`
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
	if text == "" {
		return ""
	}
	if looksLikeDrpyScriptURL(text) {
		return text
	}
	// 相对脚本路径（`./js/xxx.js`、`../lib/xxx.js`）：高天流云等大仓普遍
	// 这么写（api 是运行时库，ext 才是站点脚本）。这里原样返回，由调用方
	// 按订阅地址补全——修这个之前，298 站的大仓只能识别出 6 个运行时库。
	if libretvLooksLikeScriptPath(text) {
		return text
	}
	return ""
}

// libretvLooksLikeScriptPath 判断是否是相对脚本路径（待补全）。
// 形态：`./js/xx.js`、`../lib/xx.js`、`js/xx.js`、`/lib/xx.js`。
// 排除 csp_ / http(s) 等明显不是路径的写法。
func libretvLooksLikeScriptPath(raw string) bool {
	text := strings.TrimSpace(raw)
	if text == "" || strings.ContainsAny(text, " \t\n\r") {
		return false
	}
	if strings.HasPrefix(text, "csp_") || strings.HasPrefix(text, "py_") {
		return false
	}
	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		return false
	}
	path := text
	if cut := strings.IndexAny(path, "?#;"); cut >= 0 {
		path = path[:cut]
	}
	return strings.HasSuffix(strings.ToLower(path), ".js")
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
// libretvExtractJSONObject 从任意文本里抠出 JSON 对象（订阅正文可能带
// 前后包裹文本或 HTML）。
//
// 注意：这里**不做**注释剥离，保持「原始切片」语义——调用方可自行决定。
// 订阅配置的注释剥离统一走 libretvCleanConfigText。
func libretvExtractJSONObject(body string) string {
	trimmed := strings.TrimSpace(body)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end <= start {
		return ""
	}
	return trimmed[start : end+1]
}

// libretvCleanConfigText 清洗订阅/配置文本，让「人工维护的不严格 JSON」
// 也能解析。做三件事：
//
//  1. 去 BOM；
//  2. 去 // 与 /* */ 注释——真实订阅普遍带注释（maoystv 的 001.json
//     第 2 行就是 `//数据接口`），不处理会整份解析失败；
//  3. 转义字符串字面量里的裸换行 / 制表符——真实订阅里作者手滑断行很常见
//     （小盒子 4K 的 `"优\n酷"` 中间就是真换行），标准 JSON 不允许，
//     Go 的 encoding/json 会直接报错。
//
// 不做的事：不去尾随逗号、不修中文引号——那些属于「文件损坏」，宁可报错
// 让用户换源，也不要猜出一份可能张冠李戴的配置。
func libretvCleanConfigText(body string) string {
	return escapeRawJSONControlChars(ruleStripJSONComments(strings.TrimPrefix(body, "\ufeff")))
}

// escapeRawJSONControlChars 把 JSON 字符串字面量内部的裸控制字符转义。
// 扫描时区分「字符串内 / 字符串外」，只在字符串内替换，避免影响结构空白。
func escapeRawJSONControlChars(body string) string {
	if !strings.ContainsAny(body, "\n\r\t") {
		return body
	}
	var builder strings.Builder
	builder.Grow(len(body))
	inString := false
	escaped := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inString {
			switch {
			case escaped:
				builder.WriteByte(c)
				escaped = false
			case c == '\\':
				builder.WriteByte(c)
				escaped = true
			case c == '"':
				builder.WriteByte(c)
				inString = false
			case c == '\n':
				builder.WriteString(`\n`)
			case c == '\r':
				builder.WriteString(`\r`)
			case c == '\t':
				builder.WriteString(`\t`)
			default:
				builder.WriteByte(c)
			}
			continue
		}
		if c == '"' {
			inString = true
		}
		builder.WriteByte(c)
	}
	return builder.String()
}

// drpyCandidate 混合订阅里发现的 drpy 脚本候选：type≥3 且 api 是 .js
// 脚本地址。不自动导入（脚本需要单独校验），由用户在站源管理一键导入。
type drpyCandidate struct {
	Name string
	API  string
	// Relative 脚本地址原本是相对路径（`./js/xx.js`），上面的 API 是按
	// 订阅地址补全出来的。
	//
	// 补全只在「订阅就是脚本所在站点」时成立；多源聚合订阅（如影视仓
	// 聚合 518 站）把各家的配置合并成一个文件却**没改写相对路径**，补全
	// 出来的是聚合文件所在目录下的地址，必然 404。标记出来是为了在候选
	// 列表里提示，避免用户一个个点了才发现全失败。
	Relative bool
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

// libretvParseResult 订阅解析结果。
//
// 规则型 Spider（XBPQ / 海阔 / Bili）加入后返回值由五元组收拢成结构体：
// 后续再加引擎字段时不必再动所有调用方（测试里有 8 处解构调用）。
type libretvParseResult struct {
	// Sites 可直接入库的采集站（MacCMS JSON/XML 与 csp_AppYsV2）。
	Sites []libretvSite
	// DrpyCandidates type≥3 且 api 是 .js 的脚本候选（用户确认后导入）。
	DrpyCandidates []drpyCandidate
	// RuleCandidates 规则型 Spider 候选（导入期下载 / 校验 / 入库）。
	RuleCandidates []ruleCandidate
	// Parses 订阅里的解析规则。
	Parses []libretvParse
	// ParseBroken parses 段解析失败（字段类型不合规等）：站点仍可导入，
	// 只是没有解析方式，导入结果里会提示。
	ParseBroken bool
	// Skipped 被跳过的条目与原因。
	Skipped []string
}

// parseLibreTVSubscription 解析 LibreTV 订阅配置。
//
// type≥3 的判定顺序：drpy 脚本候选 → 规则型 Spider（XBPQ / 海阔 /
// Bili，纯数据规则）→ csp_AppYsV2 采集接口 → 带原因跳过；type=0/1/缺省
// 按 JSON 采集接口导入，type=2 按 XML 采集接口导入（TVBox 的苹果CMS XML
// 版本）。baseURL 为订阅自身地址，用于把 CatVod 蜘蛛常见的相对写法
// （./cat/xxx_open.js）补全成完整地址后再判定候选。
func parseLibreTVSubscription(body, baseURL string) (libretvParseResult, error) {
	// 先清洗（去 BOM / // 注释），再抠 JSON 对象：真实订阅普遍带注释，
	// 不清洗会整份解析失败。
	cleaned := libretvCleanConfigText(body)
	document := libretvExtractJSONObject(cleaned)
	if document == "" {
		return libretvParseResult{}, errors.New("未找到有效的 LibreTV 配置（需要包含 sites 的 JSON）")
	}
	// sites 与 parses **分开解析**：两者在订阅里是并列字段，其中一段写坏
	// 不该拖垮另一段。真实订阅「影视仓聚合」的 parses 里有一条
	// `"type": "1"`（字符串），合在一起解析会让 518 个站点全部报废。
	var siteDoc struct {
		Sites []libretvConfigSite `json:"sites"`
	}
	if json.Unmarshal([]byte(document), &siteDoc) != nil {
		return libretvParseResult{}, errors.New("配置解析失败，请确认是 LibreTV/TVBox 格式的 JSON")
	}
	var parseDoc struct {
		Parses []libretvParse `json:"parses"`
	}
	// parses 解析失败只记跳过原因——解析方式缺失不阻断片源导入。
	if json.Unmarshal([]byte(document), &parseDoc) != nil {
		parseDoc.Parses = nil
	}
	var result libretvParseResult
	result.ParseBroken = json.Unmarshal([]byte(document), &parseDoc) != nil
	// parses 与 sites 相互独立：只带解析规则的订阅也可以导入（解析方式本身
	// 对已导入的站点同样生效）。
	seenParse := map[string]bool{}
	for _, raw := range parseDoc.Parses {
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
		result.Parses = append(result.Parses, parse)
	}
	if len(siteDoc.Sites) == 0 && len(result.Parses) == 0 {
		return libretvParseResult{}, errors.New("配置里没有 sites 条目")
	}
	seenAPI := map[string]bool{}
	usedKey := map[string]bool{}
	for _, entry := range siteDoc.Sites {
		api := strings.TrimSpace(entry.API)
		if api == "" {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s：缺少 api 地址", libretvDisplayName(entry)))
			continue
		}
		if entry.Type >= 3 {
			// drpy/spider 脚本：优先取 ext（TVBox 约定 ext 是站点脚本，
			// api 是 drpy 运行时库）；不是脚本的条目继续往下判规则源。
			candidate := libretvConfigScriptCandidate(entry)
			// 相对路径（CatVod 蜘蛛的 ./cat/xxx.js）：按订阅地址补全。
			relative := !isProviderHTTPMediaURL(candidate)
			if relative {
				if resolved := catVodResolveURL(baseURL, candidate); resolved != "" {
					candidate = resolved
				}
			}
			if looksLikeDrpyScriptURL(candidate) && !seenAPI[candidate] {
				result.DrpyCandidates = append(result.DrpyCandidates, drpyCandidate{
					Name:     libretvDisplayName(entry),
					API:      candidate,
					Relative: relative,
				})
				seenAPI[candidate] = true
				continue
			}
			// 同一脚本地址重复出现：静默去重，不刷跳过原因。
			if looksLikeDrpyScriptURL(candidate) {
				continue
			}
			// 规则型 Spider（csp_XBPQ / csp_XYQHiker / csp_Bili）：
			// 纯数据规则，Go 重写解释器，不依赖 JVM / jar 包。
			// 识别阶段只判形状（ext 形态 + 必需字段），远程规则的下载
			// 与字段校验在导入期完成（importLibreTVRuleCandidates）。
			if rule, reason := ruleCandidateFromEntry(baseURL, entry); rule.Engine != "" || reason != "" {
				if reason != "" {
					result.Skipped = append(result.Skipped, reason)
					continue
				}
				result.RuleCandidates = append(result.RuleCandidates, rule)
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
					result.Sites = append(result.Sites, libretvSite{Key: key, Name: name, API: normalized, Type: 1})
					if len(result.Sites) >= libretvImportMaxSites {
						result.Skipped = append(result.Skipped, fmt.Sprintf("站点数量超过 %d 个，仅导入前 %d 个", libretvImportMaxSites, libretvImportMaxSites))
						break
					}
					continue
				}
			}
			result.Skipped = append(
				result.Skipped,
				fmt.Sprintf("%s：%s", libretvDisplayName(entry), libretvUnsupportedReason(entry)),
			)
			continue
		}
		key, name, normalized, ok := normalizeLibreTVSite(entry.Key, entry.Name, api, int(entry.Type))
		if !ok {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s：接口地址无效", libretvDisplayName(entry)))
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
		result.Sites = append(result.Sites, libretvSite{Key: key, Name: name, API: normalized, Type: libretvNormalizeSiteType(int(entry.Type))})
		if len(result.Sites) >= libretvImportMaxSites {
			result.Skipped = append(result.Skipped, fmt.Sprintf("站点数量超过 %d 个，仅导入前 %d 个", libretvImportMaxSites, libretvImportMaxSites))
			break
		}
	}
	// 只有 drpy / 规则候选 / 解析规则时不算失败：用户可一键导入脚本源
	// 或直接用解析。
	if len(result.Sites) == 0 && len(result.DrpyCandidates) == 0 && len(result.RuleCandidates) == 0 && len(result.Parses) == 0 {
		return libretvParseResult{Skipped: result.Skipped}, errors.New("配置里没有可导入的采集接口站点")
	}
	return result, nil
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
	parsed, err := parseLibreTVSubscription(body, configURL)
	if err != nil && looksLikeLibreTVBase58(body) {
		// 社区配置工具（LunaTV-config 等）的 Base58 订阅：format=2 是原始 JSON、
		// format=3 是带代理前缀的 JSON，解码后复用同一套解析，客户端不需要
		// 实时解析 type≥3 脚本，也不需要自建后端。
		if decoded, decodeErr := libretvBase58Decode(body); decodeErr == nil {
			if decodedResult, decodedErr := parseLibreTVSubscription(string(decoded), configURL); decodedErr == nil || len(decodedResult.Skipped) > 0 {
				// 部分站点不可用时也保留跳过原因（例如配置里混了 drpy 源）。
				parsed, err = decodedResult, decodedErr
			}
		}
	}
	sites, drpyCandidates, ruleCandidates, parses, skipped := parsed.Sites, parsed.DrpyCandidates, parsed.RuleCandidates, parsed.Parses, parsed.Skipped
	// 解析段写坏（字段类型不合规等）：站点已照常导入，但要让用户知道
	// 「解析方式没进来」——否则会出现「片源在但点开播不了」的困惑。
	if parsed.ParseBroken {
		skipped = append(skipped, "订阅的解析规则（parses）格式不合规已跳过，片源照常导入；部分需要解析的片源可能无法播放")
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
	// 规则型 Spider：远程规则下载 + 形状校验 + 快照入库，成功者并入
	// 本次待入库站点（随下面的统一探测一起验证）。规则是纯数据，
	// 不做用户二次确认（与需要执行代码的 drpy 脚本不同）。
	if len(ruleCandidates) > 0 {
		ruleSites, ruleSkipped := engine.importLibreTVRuleCandidates(ctx, ruleCandidates, libretvImportMaxSites-len(sites))
		sites = append(sites, ruleSites...)
		skipped = append(skipped, ruleSkipped...)
	}
	// 先串行入库（保证 key 冲突处理与顺序稳定），再并发探测。
	pending := sites[:0:0]
	for _, site := range sites {
		if _, err := store.put(site); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s：保存失败（%s）", site.Name, publicError(err)))
			continue
		}
		pending = append(pending, site)
	}
	added := make([]map[string]any, len(pending))
	verified := 0
	// 并发探测：一次订阅几十个站点时串行会把总预算跑满，表现为导入卡住。
	// budget 是所有协程共享的剩余预算，取用与扣减都在锁内。
	var probeMu sync.Mutex
	budget := libretvProbeBudget
	var probeGroup sync.WaitGroup
	semaphore := make(chan struct{}, libretvProbeWorkers)
	for index, site := range pending {
		probeGroup.Add(1)
		semaphore <- struct{}{}
		go func(index int, site libretvSite) {
			defer probeGroup.Done()
			defer func() { <-semaphore }()
			ok := false
			probeMu.Lock()
			remaining := budget
			probeMu.Unlock()
			if remaining > 0 && ctx.Err() == nil {
				per := libretvProbePerSite
				if per > remaining {
					per = remaining
				}
				started := time.Now()
				probeCtx, cancel := context.WithTimeout(ctx, per)
				// 直接走目录链路：既验证接口，又把首页写进缓存，
				// 导入完成后站源管理里立刻能看到剧集数。
				page, probeErr := engine.nativeCatalog(probeCtx, nativeInput{Source: libretvSourceID(site.Key), Page: 1, Force: true})
				cancel()
				elapsed := time.Since(started)
				ok = probeErr == nil && len(page.Items) > 0 && page.Warning == ""
				probeMu.Lock()
				budget -= elapsed
				if ok {
					verified++
				}
				probeMu.Unlock()
			}
			probeMu.Lock()
			added[index] = map[string]any{
				"key":      site.Key,
				"name":     site.Name,
				"api":      site.API,
				"type":     site.Type,
				"engine":   site.Engine,
				"verified": ok,
			}
			probeMu.Unlock()
		}(index, site)
	}
	probeGroup.Wait()
	// 订阅变更后 Dart 侧会重新拉取列表，无需额外广播。
	// drpy 候选不自动导入（脚本需要用户确认后才加入），仅透传给 Dart 侧展示。
	drpy := make([]map[string]any, 0, len(drpyCandidates))
	for _, candidate := range drpyCandidates {
		// relative 标记脚本地址原本是相对路径（按订阅地址补全而来）。
		// 多源聚合订阅往往没改写相对路径，补全出来必然 404——标出来让
		// UI 提前提示，别让用户一个个点了才发现全失败。
		drpy = append(drpy, map[string]any{
			"name":     candidate.Name,
			"api":      candidate.API,
			"relative": candidate.Relative,
		})
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

// importLibreTVRuleCandidates 规则候选落地：远程规则下载 → 形状校验 →
// 规则快照入库 → 返回可入库的站点。
//
// 与 drpy 候选的差异：规则是纯数据（不执行代码），不需要用户二次确认，
// 导入订阅时直接落地；失败仅跳过该源并给出具体原因，不影响其余源。
// 下载共享预算（ruleDownloadBudget），防止几十个规则源拖垮一次导入。
//
// remaining 为本次导入还能新增的站点数（与采集站共享
// libretvImportMaxSites 上限）。
func (engine *nativeEngine) importLibreTVRuleCandidates(ctx context.Context, candidates []ruleCandidate, remaining int) ([]libretvSite, []string) {
	d := engine.downloader
	rules := d.rulesStore()
	sites := make([]libretvSite, 0, len(candidates))
	var skipped []string
	seen := map[string]bool{}
	usedKey := map[string]bool{}
	budget := ruleDownloadBudget
	for _, candidate := range candidates {
		if len(sites) >= remaining {
			skipped = append(skipped, fmt.Sprintf("站点数量超过 %d 个，仅导入前 %d 个", libretvImportMaxSites, libretvImportMaxSites))
			break
		}
		// 同一订阅里重复出现的规则（同地址 / 同站内联规则）静默去重。
		if identity := candidate.identity(); identity != "" {
			if seen[identity] {
				continue
			}
			seen[identity] = true
		}
		rule := candidate.Rule
		if rule == nil {
			if budget <= 0 {
				skipped = append(skipped, fmt.Sprintf("%s：规则下载超出本次导入预算，请重新导入或稍后再试", candidate.Name))
				continue
			}
			per := ruleDownloadPerSite
			if per > budget {
				per = budget
			}
			ruleCtx, cancel := context.WithTimeout(ctx, per)
			fetched, err := d.fetchRuleDocument(ruleCtx, candidate.RuleURL)
			cancel()
			budget -= per
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s：规则下载失败（%s）", candidate.Name, publicError(err)))
				continue
			}
			rule = fetched
		}
		if reason := validateRuleDocument(candidate.Engine, rule); reason != "" {
			skipped = append(skipped, fmt.Sprintf("%s：%s", candidate.Name, reason))
			continue
		}
		siteAPI := ruleSiteAPI(candidate.Engine, rule, candidate.RuleURL)
		// normalizeLibreTVSite 要求 api 带路径：XBPQ 的「主页url」常是
		// 裸域名（https://host:5200），补一个 "/" 让它通过校验
		// （入库时末尾斜杠会被去掉）。
		if parsed, err := url.Parse(siteAPI); err == nil && parsed.Scheme != "" && parsed.Path == "" {
			siteAPI += "/"
		}
		// key 优先级：订阅里合法的 key（用户可识别、跨导入稳定）>
		// 规则地址派生（同一规则重复导入稳定）> 站点地址派生（内联规则）。
		rawKey := strings.TrimSpace(candidate.Key)
		if !libretvKeyPattern.MatchString(rawKey) {
			rawKey = ""
			if candidate.RuleURL != "" {
				rawKey = libretvKeyFromAPI(candidate.RuleURL)
			}
		}
		key, name, api, ok := normalizeLibreTVSite(rawKey, candidate.Name, siteAPI, 1)
		if !ok {
			skipped = append(skipped, fmt.Sprintf("%s：站点地址无效", candidate.Name))
			continue
		}
		for usedKey[key] { // key 冲突时退化：优先用规则地址区分
			seed := api
			if candidate.RuleURL != "" {
				seed = candidate.RuleURL
			}
			key = libretvKeyFromAPI(seed + "/" + strconv.Itoa(len(usedKey)))
		}
		usedKey[key] = true
		entry := ruleEntry{
			Key:       key,
			Engine:    candidate.Engine,
			File:      ruleBodyFile(key),
			SourceURL: candidate.RuleURL,
			UpdatedAt: time.Now().Format(time.RFC3339),
		}
		if err := rules.put(entry, rule); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s：规则保存失败（%s）", candidate.Name, publicError(err)))
			continue
		}
		sites = append(sites, libretvSite{Key: key, Name: name, API: api, Type: 1, Engine: candidate.Engine})
	}
	return sites, skipped
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
	// 规则源连带删除规则快照（普通采集站没有对应条目，remove 为空操作）。
	if err := engine.downloader.rulesStore().remove(key); err != nil {
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
		item := map[string]any{
			"key":    site.Key,
			"name":   site.Name,
			"api":    site.API,
			"type":   site.Type,
			"engine": site.Engine,
			"count":  counts[site.Key],
		}
		// 规则源带上规则来源地址：跨设备同步时凭它重建规则（重新下载）。
		// 内联规则的来源地址为空，无法在别的设备重建，同步侧会跳过并提示。
		if site.Engine != "" {
			if entry, ok := engine.downloader.rulesStore().get(site.Key); ok && entry.SourceURL != "" {
				item["ruleUrl"] = entry.SourceURL
			}
		}
		out = append(out, item)
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
// 规则型 Spider（XBPQ / 海阔 / Bili）在函数入口分派到各自引擎，
// app_runtime.go 的四处调用点保持零改动。
func (d *Downloader) fetchLibreTVCatalogPage(ctx context.Context, site libretvSite, page int, category, query string) ([]Drama, bool, error) {
	if entry, err := d.libretvRuleEntry(site); err != nil {
		return nil, false, err
	} else if entry != nil {
		switch entry.Engine {
		case ruleEngineXBPQ:
			return d.fetchXBPQCatalogPage(ctx, site, *entry, page, category, query)
		case ruleEngineHiker:
			return d.fetchHikerCatalogPage(ctx, site, *entry, page, category, query)
		case ruleEngineBili:
			return d.fetchBiliCatalogPage(ctx, site, *entry, page, category, query)
		default:
			return nil, false, errors.New("该站源的规则引擎暂不支持，请重新导入订阅")
		}
	}
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
	if entry, err := d.libretvRuleEntry(site); err != nil {
		return nil, err
	} else if entry != nil {
		switch entry.Engine {
		case ruleEngineXBPQ:
			return d.fetchXBPQCategories(site, *entry)
		case ruleEngineHiker:
			return d.fetchHikerCategories(site, *entry)
		case ruleEngineBili:
			return d.fetchBiliCategories(site, *entry)
		default:
			return nil, errors.New("该站源的规则引擎暂不支持，请重新导入订阅")
		}
	}
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
	if entry, err := d.libretvRuleEntry(site); err != nil {
		return Drama{}, nil, err
	} else if entry != nil {
		switch entry.Engine {
		case ruleEngineXBPQ:
			return d.fetchXBPQDetail(ctx, site, *entry, vodID)
		case ruleEngineHiker:
			return d.fetchHikerDetail(ctx, site, *entry, vodID)
		case ruleEngineBili:
			return d.fetchBiliDetail(ctx, site, *entry, vodID)
		default:
			return Drama{}, nil, errors.New("该站源的规则引擎暂不支持，请重新导入订阅")
		}
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
