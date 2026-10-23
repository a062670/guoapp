package core

// 规则型片源（XBPQ / 海阔 XYQHiker / Bili）的公共层。
//
// 背景：LibreTV / TVBox 订阅里 type=3 的条目除了 drpy JS 脚本，还有三类
// 「纯规则」爬虫——逻辑不靠代码执行，靠一段 JSON 规则描述（选择器 /
// JSON 路径 + 字段映射），用 Go 重写解释器即可覆盖，不需要 JVM：
//
//   - csp_XBPQ（小白配置）：猫系选择器规则，字段为「主页url / 分类 /
//     分类url / 数组 / 标题 / 图片 / 链接 / 线路 / 播放」等，
//     截取语法「前标记&&后标记」，与 drpy 的 pdfh/pdfa 同宗；
//   - csp_XYQHiker（海阔视界）：51 个中文字段，截取模式 0=JSON /
//     1=HTML，数组规则用 JSON 路径或 CSS 选择器；
//   - csp_Bili：规则只有 cookie / classes / filter，数据走 B 站
//     公开 API 重实现（wbi 签名）。
//
// 持久化选型：**独立 rules store**，不扩 libretvSite 的采集字段——
//   - 索引 rules.json：{key, engine, file, sourceUrl, updatedAt}；
//   - 正文 rules/<key>.json：规则快照（内联规则原样保存，远程规则
//     导入时下载后保存，订阅下线不影响已导入源）；
//   - key 复用 LibreTV 站点 key，源 ID 保持 libretv:<key> 不变，
//     站源注册 / 分派 / 播放链路全部零改动。
//
// 识别顺序（parseLibreTVSubscription 内）：drpy 脚本候选 → XBPQ →
// Hiker → Bili → csp_AppYsV2 特例 → jar/py/其余 csp 跳过。
// 识别只判形状不下载；远程规则的下载发生在 importLibreTVSubscription
// （与 drpy 候选同属导入期动作），失败仅跳过该源，不影响其余源。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// ruleEngineXBPQ 小白配置规则（csp_XBPQ）。
	ruleEngineXBPQ = "xbpq"
	// ruleEngineHiker 海阔视界规则（csp_XYQHiker）。
	ruleEngineHiker = "hiker"
	// ruleEngineBili B 站规则（csp_Bili，数据走公开 API）。
	ruleEngineBili = "bili"
	// ruleStoreFile 规则索引文件名（相对引擎目录）。
	ruleStoreFile = "rules.json"
	// ruleBodiesDir 规则正文目录（相对引擎目录）。
	ruleBodiesDir = "rules"
	// ruleBodyMaxBytes 单个规则正文上限（256KB）：规则普遍几 KB，
	// 留足余量防异常配置刷盘。
	ruleBodyMaxBytes = 256 << 10
	// ruleStoreMaxBytes 索引文件上限（512KB，与订阅同级）。
	ruleStoreMaxBytes = 512 << 10
	// ruleDownloadBudget 单次订阅导入的规则下载总预算。
	ruleDownloadBudget = 25 * time.Second
	// ruleDownloadPerSite 单个远程规则下载超时。
	ruleDownloadPerSite = 5 * time.Second
)

// ruleEntry 规则索引条目（落盘部分；Rule 为内存缓存，不持久化）。
type ruleEntry struct {
	// Key 站点 key（与 libretvSite.Key 一致）。
	Key string `json:"key"`
	// Engine 规则引擎：xbpq / hiker / bili。
	Engine string `json:"engine"`
	// File rules/<key>.json（相对引擎目录）。
	File string `json:"file"`
	// SourceURL 远程规则地址（内联规则为空；更新时按它重拉）。
	SourceURL string `json:"sourceUrl,omitempty"`
	// UpdatedAt 最近一次入库/刷新时间（RFC3339）。
	UpdatedAt string `json:"updatedAt,omitempty"`
	// Rule 规则正文（load 时读入内存，避免每次请求都落盘）。
	Rule map[string]any `json:"-"`
}

// ruleBodyFile 规则正文的相对路径。
func ruleBodyFile(key string) string {
	return ruleBodiesDir + "/" + key + ".json"
}

// ruleStore 规则源订阅表（进程内 + rules.json 索引 + rules/ 正文）。
type ruleStore struct {
	directory string
	mu        chan struct{}
	entries   map[string]ruleEntry
}

func newRuleStore(directory string) *ruleStore {
	return &ruleStore{
		directory: directory,
		mu:        make(chan struct{}, 1),
		entries:   map[string]ruleEntry{},
	}
}

// loadRuleStore 从引擎目录读取规则索引与正文。
// 索引缺失/损坏时返回空表；正文缺失或损坏的条目会被跳过
// （防手工删文件后留下死条目，与 drpyStore 同策略）。
func loadRuleStore(directory string) *ruleStore {
	store := newRuleStore(directory)
	body, err := os.ReadFile(filepath.Join(directory, ruleStoreFile))
	if err != nil || len(body) > ruleStoreMaxBytes {
		return store
	}
	var payload struct {
		Sites []ruleEntry `json:"sites"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return store
	}
	for _, entry := range payload.Sites {
		key := strings.TrimSpace(entry.Key)
		engine := normalizeRuleEngine(entry.Engine)
		if key == "" || engine == "" || !libretvKeyPattern.MatchString(key) {
			continue
		}
		file := strings.TrimSpace(entry.File)
		if file == "" {
			file = ruleBodyFile(key)
		}
		rule, ok := readRuleBody(filepath.Join(directory, filepath.FromSlash(file)))
		if !ok {
			continue
		}
		store.entries[key] = ruleEntry{
			Key:       key,
			Engine:    engine,
			File:      file,
			SourceURL: strings.TrimSpace(entry.SourceURL),
			UpdatedAt: strings.TrimSpace(entry.UpdatedAt),
			Rule:      rule,
		}
	}
	return store
}

// readRuleBody 读取并解析单个规则正文；任何异常都按「不可用」处理。
func readRuleBody(path string) (map[string]any, bool) {
	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 || len(body) > ruleBodyMaxBytes {
		return nil, false
	}
	var rule map[string]any
	if json.Unmarshal(body, &rule) != nil || len(rule) == 0 {
		return nil, false
	}
	return rule, true
}

// normalizeRuleEngine 归一化引擎名；无法识别时返回空串。
func normalizeRuleEngine(engine string) string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case ruleEngineXBPQ:
		return ruleEngineXBPQ
	case ruleEngineHiker:
		return ruleEngineHiker
	case ruleEngineBili:
		return ruleEngineBili
	default:
		return ""
	}
}

// saveLocked 写索引（调用方需持有 mu）。
func (store *ruleStore) saveLocked() error {
	keys := make([]string, 0, len(store.entries))
	for key := range store.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	payload := struct {
		Sites []ruleEntry `json:"sites"`
	}{Sites: make([]ruleEntry, 0, len(keys))}
	for _, key := range keys {
		payload.Sites = append(payload.Sites, store.entries[key])
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writeNativeCacheFile(filepath.Join(store.directory, ruleStoreFile), body)
}

func (store *ruleStore) lock()   { store.mu <- struct{}{} }
func (store *ruleStore) unlock() { <-store.mu }

// put 登记规则：写正文快照 + 更新索引 + 刷新内存缓存。
// body 为 nil 时仅更新索引元信息（远程重拉失败保留旧快照用）。
func (store *ruleStore) put(entry ruleEntry, body map[string]any) error {
	key := strings.TrimSpace(entry.Key)
	engine := normalizeRuleEngine(entry.Engine)
	if key == "" || engine == "" || !libretvKeyPattern.MatchString(key) {
		return errors.New("规则站点 key 无效")
	}
	store.lock()
	defer store.unlock()
	file := strings.TrimSpace(entry.File)
	if file == "" {
		file = ruleBodyFile(key)
	}
	rule := body
	if rule == nil {
		// 保留旧正文：内存里没有（异常情况）时按缺失处理。
		if existing, ok := store.entries[key]; ok && existing.Rule != nil {
			rule = existing.Rule
		}
	}
	if rule != nil {
		if err := writeRuleBody(filepath.Join(store.directory, filepath.FromSlash(file)), rule); err != nil {
			return err
		}
	}
	previous := store.entries[key]
	updatedAt := strings.TrimSpace(entry.UpdatedAt)
	if updatedAt == "" {
		updatedAt = previous.UpdatedAt
	}
	store.entries[key] = ruleEntry{
		Key:       key,
		Engine:    engine,
		File:      file,
		SourceURL: firstNonEmpty(strings.TrimSpace(entry.SourceURL), previous.SourceURL),
		UpdatedAt: updatedAt,
		Rule:      rule,
	}
	return store.saveLocked()
}

// writeRuleBody 写规则正文快照（原子写，防半截文件）。
func writeRuleBody(path string, rule map[string]any) error {
	body, err := json.Marshal(rule)
	if err != nil {
		return err
	}
	if len(body) > ruleBodyMaxBytes {
		return errors.New("规则内容过大")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeNativeCacheFile(path, body)
}

// remove 删除规则条目与正文快照。
func (store *ruleStore) remove(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	store.lock()
	defer store.unlock()
	entry, ok := store.entries[key]
	if !ok {
		return nil
	}
	delete(store.entries, key)
	if err := store.saveLocked(); err != nil {
		return err
	}
	if file := strings.TrimSpace(entry.File); file != "" {
		_ = os.Remove(filepath.Join(store.directory, filepath.FromSlash(file)))
	}
	return nil
}

// get 按站点 key 取规则条目（含正文）。
func (store *ruleStore) get(key string) (ruleEntry, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return ruleEntry{}, false
	}
	store.lock()
	defer store.unlock()
	entry, ok := store.entries[key]
	return entry, ok
}

// entryFor 按完整源 ID（libretv:<key>）取规则条目。
func (store *ruleStore) entryFor(source string) (ruleEntry, bool) {
	key := libretvKeyFromSource(source)
	if key == "" {
		return ruleEntry{}, false
	}
	return store.get(key)
}

// list 返回全部规则条目（按 key 排序，稳定输出）。
func (store *ruleStore) list() []ruleEntry {
	store.lock()
	defer store.unlock()
	keys := make([]string, 0, len(store.entries))
	for key := range store.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]ruleEntry, 0, len(keys))
	for _, key := range keys {
		out = append(out, store.entries[key])
	}
	return out
}

// ruleEngineLabel 引擎中文名（UI 展示与导入结果用）。
func ruleEngineLabel(engine string) string {
	switch normalizeRuleEngine(engine) {
	case ruleEngineXBPQ:
		return "XBPQ 规则"
	case ruleEngineHiker:
		return "海阔规则"
	case ruleEngineBili:
		return "Bili"
	default:
		return "规则"
	}
}

// ruleCandidate 订阅里发现的规则源候选（与 drpyCandidate 对应：
// 解析阶段只收集，不下载；导入期才落地）。
type ruleCandidate struct {
	// Key 订阅条目原始 key（合法时直接复用，保证用户可识别且跨导入稳定）。
	Key     string
	Name    string
	Engine  string
	RuleURL string         // 远程规则地址（内联规则为空）
	Rule    map[string]any // 内联规则（远程规则为空）
}

// ruleSpiderEngine 订阅 api 字段 → 规则引擎名；不是规则型 Spider 返回空。
// 按 Spider 名精确匹配（生态里这三个是固定值），不做模糊猜测，
// 避免把同名不同义的条目误收。
func ruleSpiderEngine(api string) string {
	switch strings.TrimSpace(api) {
	case "csp_XBPQ":
		return ruleEngineXBPQ
	case "csp_XYQHiker":
		return ruleEngineHiker
	case "csp_Bili", "csp_BiliGuard":
		// BiliGuard 是 Bili 的加固版（cookie 池 / 风控重试），规则文件同构
		// （classes + filter + cookie），抓取链路完全一致。
		return ruleEngineBili
	default:
		return ""
	}
}

// ruleCandidateFromEntry 从订阅条目提取规则候选。
// 返回 (候选, 跳过原因)：不是规则型 Spider 时两者皆空。
//
// ext 在生态里有三种形态（都见过真实样本）：
//  1. 字符串 = 远程规则地址（容忍 TVBox 的 ;md5 校验后缀）；
//  2. 对象含 "json" = 间接远程规则（如 csp_Bili 的
//     {"json": "https://.../bilibili.json"}，Ray-dxawi 配置实证）；
//  3. 对象本身 = 内联规则（如 FongMi 官方配置的 Bili JSON）。
//
// 三种形态都不做网络请求——远程规则在导入期下载（见
// importLibreTVRuleCandidates），识别阶段只判形状。
func ruleCandidateFromEntry(baseURL string, entry libretvConfigSite) (ruleCandidate, string) {
	engine := ruleSpiderEngine(entry.API)
	if engine == "" {
		return ruleCandidate{}, ""
	}
	name := libretvDisplayName(entry)
	// cleanRuleURL 清洗地址：容忍 ;md5 后缀；相对路径（海阔订阅大量用
	// `./XYQHiker/xxx.json`）按订阅地址补全——与 drpy 脚本候选同规则。
	cleanRuleURL := func(raw string) string {
		text := strings.TrimSpace(raw)
		if cut := strings.IndexByte(text, ';'); cut >= 0 {
			text = strings.TrimSpace(text[:cut])
		}
		if text == "" || isProviderHTTPMediaURL(text) {
			return text
		}
		if resolved := catVodResolveURL(baseURL, text); resolved != "" {
			return resolved
		}
		return text
	}
	if text := libretvConfigExtText(entry.Ext); text != "" {
		text = cleanRuleURL(text)
		if !isProviderHTTPMediaURL(text) {
			return ruleCandidate{}, fmt.Sprintf("%s：%s 规则地址无效", name, ruleEngineLabel(engine))
		}
		return ruleCandidate{Key: strings.TrimSpace(entry.Key), Name: name, Engine: engine, RuleURL: text}, ""
	}
	var inline map[string]any
	if json.Unmarshal(entry.Ext, &inline) == nil && len(inline) > 0 {
		// 间接形态：{"json": "<远程规则地址>"}。
		if nested := ruleText(inline, "json"); nested != "" {
			nested = cleanRuleURL(nested)
			if !isProviderHTTPMediaURL(nested) {
				return ruleCandidate{}, fmt.Sprintf("%s：%s 规则地址无效", name, ruleEngineLabel(engine))
			}
			return ruleCandidate{Key: strings.TrimSpace(entry.Key), Name: name, Engine: engine, RuleURL: nested}, ""
		}
		return ruleCandidate{Key: strings.TrimSpace(entry.Key), Name: name, Engine: engine, Rule: inline}, ""
	}
	return ruleCandidate{}, fmt.Sprintf("%s：%s 规则缺少内容（ext）", name, ruleEngineLabel(engine))
}

// validateRuleDocument 规则形状强校验：每种引擎的最小必需字段。
// 识别阶段只判形状——远程规则下载后、内联规则解析后各校验一次，
// 缺字段的按「不支持」跳过并给出具体原因，不静默丢弃。
func validateRuleDocument(engine string, rule map[string]any) string {
	if len(rule) == 0 {
		return "规则内容为空"
	}
	switch engine {
	case ruleEngineXBPQ:
		// XBPQ 至少要有分类页或搜索页地址，否则导入后永远取不到数据。
		if ruleText(rule, "分类url") == "" && ruleText(rule, "搜索url") == "" {
			return "XBPQ 规则缺少「分类url / 搜索url」"
		}
	case ruleEngineHiker:
		if ruleText(rule, "分类链接") == "" && ruleText(rule, "搜索链接") == "" {
			return "海阔规则缺少「分类链接 / 搜索链接」"
		}
	case ruleEngineBili:
		// 两种历史键名并存：FongMi 官方规则用 classes/filter，
		// 社区派生规则（如 fantaiying7/EXT）用 class/filters。
		if !ruleListPresent(rule, "classes", "class") {
			return "Bili 规则缺少 classes 分类"
		}
	}
	return ""
}

// ruleText 取规则里的字符串字段（容忍数字/布尔，与 drpyRuleString 同思路）。
func ruleText(rule map[string]any, key string) string {
	value, ok := rule[key]
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	if boolean, ok := value.(bool); ok {
		if boolean {
			return "true"
		}
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// ruleListPresent 多个候选键里是否至少有一个是非空数组。
// 用于兼容同义不同名的历史字段（classes/class、filter/filters）。
func ruleListPresent(rule map[string]any, keys ...string) bool {
	for _, key := range keys {
		if items, ok := rule[key].([]any); ok && len(items) > 0 {
			return true
		}
	}
	return false
}

// ruleSiteAPI 规则源的站点地址（libretvSite.API）：优先规则自带的
// 站点地址，取不到才退回规则文件地址。用于 key 派生 / Referer 推导 /
// UI 展示，不参与数据请求（数据请求用规则里的各链接字段）。
func ruleSiteAPI(engine string, rule map[string]any, ruleURL string) string {
	keys := []string{"主页url"}
	switch engine {
	case ruleEngineXBPQ:
		keys = append(keys, "分类url", "搜索url")
	case ruleEngineHiker:
		keys = append(keys, "分类链接", "搜索链接")
	}
	for _, key := range keys {
		if value := ruleText(rule, key); value != "" {
			return value
		}
	}
	return ruleURL
}

// ruleCandidateIdentity 候选身份（同一订阅里去重用）：
//   - 远程规则按地址（同一地址重复出现即重复）；
//   - 内联规则按「引擎 + 规则内容哈希」——同一订阅里可能有多条同引擎
//     内联规则（例如多个 Bili 分类站），只按站点地址去重会互相吞掉。
func (candidate ruleCandidate) identity() string {
	if candidate.RuleURL != "" {
		return candidate.RuleURL
	}
	if len(candidate.Rule) > 0 {
		if body, err := json.Marshal(candidate.Rule); err == nil {
			sum := sha256.Sum256(body)
			return candidate.Engine + "|" + hex.EncodeToString(sum[:8])
		}
	}
	return candidate.Engine + "|" + ruleSiteAPI(candidate.Engine, candidate.Rule, "")
}

// rulesStore 规则表（懒初始化：测试与旧装配路径可能没有挂载）。
// 生产路径由 newNativeEngine 统一 loadRuleStore 初始化。
func (d *Downloader) rulesStore() *ruleStore {
	if d.rules == nil {
		directory := ""
		if d.drpy != nil {
			directory = d.drpy.directory
		}
		d.rules = newRuleStore(directory)
	}
	return d.rules
}

// libretvRuleEntry 规则源分派入口：
//   - 返回 (nil, nil)：普通采集站，调用方走 MacCMS 链路；
//   - 返回 (entry, nil)：规则源，调用方按 entry.Engine 分派；
//   - 返回 (nil, err)：站点标了 Engine 但规则快照丢失（需重新导入）。
func (d *Downloader) libretvRuleEntry(site libretvSite) (*ruleEntry, error) {
	if normalizeRuleEngine(site.Engine) == "" {
		return nil, nil
	}
	entry, ok := d.rulesStore().get(site.Key)
	if !ok || entry.Rule == nil {
		return nil, errors.New("该站源的规则文件已丢失，请在站源管理重新导入订阅")
	}
	return &entry, nil
}

// fetchRuleDocument 下载并解析远程规则 JSON。
// 规则是纯数据（不执行），但同样走 fetchProviderText 的统一超时 /
// 重试 / 镜像候选链路；大小受 ruleBodyMaxBytes 约束。
func (d *Downloader) fetchRuleDocument(ctx context.Context, rawURL string) (map[string]any, error) {
	text := strings.TrimSpace(rawURL)
	if !isProviderHTTPMediaURL(text) {
		return nil, errors.New("规则地址无效")
	}
	body, err := d.fetchProviderText(ctx, text, text)
	if err != nil {
		return nil, err
	}
	if len(body) > ruleBodyMaxBytes {
		return nil, errors.New("规则内容过大")
	}
	document := strings.TrimPrefix(body, "\ufeff")
	var rule map[string]any
	if json.Unmarshal([]byte(document), &rule) != nil || len(rule) == 0 {
		// 规则由人工维护，常见两种"不严格 JSON"：
		//   1. 带 // 注释（真实样本「短剧屋」就有）；
		//   2. 被文件托管页包裹（file.html?id=… 返回 HTML 包着 JSON）。
		// 先剥注释，再按 JSON 对象边界抠一次，仍失败才判定无效。
		cleaned := ruleStripJSONComments(document)
		if json.Unmarshal([]byte(cleaned), &rule) != nil || len(rule) == 0 {
			trimmed := libretvExtractJSONObject(cleaned)
			if trimmed == "" || json.Unmarshal([]byte(trimmed), &rule) != nil || len(rule) == 0 {
				return nil, errors.New("规则不是有效的 JSON 对象")
			}
		}
	}
	return rule, nil
}

// refreshRuleSnapshot 规则源显式刷新失败时按来源地址重拉规则快照。
//
// 触发条件只有「用户主动刷新（Force）且本次抓取失败」：规则目标站点
// 改版后，新规则文件往往已经在原地址更新，重拉一次即可自愈。
// 下载失败 / 形状不合法时保留旧快照（订阅下线后旧规则仍可继续用），
// 返回 false 让调用方按原错误提示，不掩盖真实故障。
func (d *Downloader) refreshRuleSnapshot(ctx context.Context, site libretvSite) bool {
	if normalizeRuleEngine(site.Engine) == "" {
		return false
	}
	store := d.rulesStore()
	entry, ok := store.get(site.Key)
	if !ok || strings.TrimSpace(entry.SourceURL) == "" {
		return false
	}
	ruleCtx, cancel := context.WithTimeout(ctx, ruleDownloadPerSite)
	defer cancel()
	rule, err := d.fetchRuleDocument(ruleCtx, entry.SourceURL)
	if err != nil {
		return false
	}
	if reason := validateRuleDocument(entry.Engine, rule); reason != "" {
		return false
	}
	entry.UpdatedAt = time.Now().Format(time.RFC3339)
	return store.put(entry, rule) == nil
}
