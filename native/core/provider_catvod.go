package core

// CatVod（TVBox 里标注 [cat] / ./cat/*.js）JS 蜘蛛运行时。
//
// 与 drpy 的关键差异：
//  1. 蜘蛛是 ES Module：`import { _, load } from './lib/cat.js'`，
//     goja 只支持普通脚本，需要先剥离 import 再从库导出对象注入；
//  2. 入口不是全局函数，而是末尾的
//       export function __jsEvalReturn() { return { init, home, ... } }
//     先跑脚本，再调 __jsEvalReturn() 拿到 API 对象，然后调上面的方法；
//  3. lib/cat.js 是 400~500KB 的打包产物（cheerio + dayjs + jinja2 + Crypto），
//     尾部是 `export{a as _, b as load, ...}`，同样要转成全局注入；加载时
//     会探测 Node 的 process / Buffer / console，需要少量垫片。
//
// 好消息：返回结构与 MacCMS 同构（{class:[{type_id,type_name}]}、
// {list:[{vod_id,vod_name,vod_pic,vod_remarks}]}、detail 里的
// vod_play_from / vod_play_url）、play 返回 {url, parse}，因此解析层可以
// 直接复用 drpy 那套 macCMSResponse 映射。

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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

const (
	// catVodEngine drpySite.Engine 的 CatVod 标记（空 = 普通 drpy2）。
	catVodEngine = "catvod"
	// catVodLibDir lib/cat.js 缓存目录（相对引擎目录）。
	catVodLibDir = "catlib"
)

var (
	// catVodImportPattern 蜘蛛的 import 语句：import { a, b as c } from './lib/cat.js';
	catVodImportPattern = regexp.MustCompile(
		`(?m)^\s*import\s+(?:\{([^}]*)\}|(\w+))\s+from\s*['"]([^'"]+)['"]\s*;?\s*$`,
	)
	// catVodBareImportPattern 无绑定导入：import './lib/cat.js';
	catVodBareImportPattern = regexp.MustCompile(`(?m)^\s*import\s*['"][^'"]+['"]\s*;?\s*$`)
	// catVodExportPattern 库尾部的 export{a as _, b as load};
	catVodExportPattern = regexp.MustCompile(`(?s)export\s*\{([^{}]*)\}\s*;?\s*$`)
	// catVodExportMemberPattern export 关键字修饰的顶层声明。
	catVodExportMemberPattern = regexp.MustCompile(`(?m)^\s*export\s+(function|const|let|var|class|async)\s+`)
)

// catVodProgramCache lib/cat.js 的编译缓存（体积大，编译一次约几十毫秒）。
var catVodProgramCache sync.Map

// drpySiteIsCatVod 该脚本源是否走 CatVod 运行时。
func drpySiteIsCatVod(site drpySite) bool {
	return strings.EqualFold(strings.TrimSpace(site.Engine), catVodEngine)
}

// looksLikeCatVodScript 静态判断：CatVod 蜘蛛必然有 __jsEvalReturn。
func looksLikeCatVodScript(script string) bool {
	return strings.Contains(script, "__jsEvalReturn")
}

// catVodLibPath 从蜘蛛的 import 里取 lib/cat.js 的路径（如 ./lib/cat.js）。
func catVodLibPath(script string) string {
	matches := catVodImportPattern.FindAllStringSubmatch(script, -1)
	for _, match := range matches {
		path := strings.TrimSpace(match[3])
		if path == "" {
			continue
		}
		// 只认 cat.js 系列（可能有 cat.js / cat.min.js）。
		if strings.Contains(strings.ToLower(path), "cat.js") ||
			strings.Contains(strings.ToLower(path), "cat.mjs") {
			return path
		}
	}
	return ""
}

// catVodResolveURL 相对地址按蜘蛛地址解析（支持 ./lib/x.js、../lib/x.js）。
func catVodResolveURL(base, path string) string {
	baseURL, err := url.Parse(strings.TrimSpace(base))
	if err != nil || baseURL.Host == "" {
		return ""
	}
	reference, err := url.Parse(strings.TrimSpace(path))
	if err != nil {
		return ""
	}
	resolved := baseURL.ResolveReference(reference)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return ""
	}
	return resolved.String()
}

// catVodTransformLib 把库尾部的 export{...} 换成全局注入。
func catVodTransformLib(script string) (string, error) {
	match := catVodExportPattern.FindStringSubmatch(script)
	if match == nil {
		return "", errors.New("lib/cat.js 没有找到 export 语句，无法转成本机可用的形态")
	}
	members := make([]string, 0, 16)
	for _, item := range strings.Split(match[1], ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		local, exported := item, item
		if index := strings.Index(item, " as "); index >= 0 {
			local = strings.TrimSpace(item[:index])
			exported = strings.TrimSpace(item[index+4:])
		}
		if local == "" || exported == "" {
			continue
		}
		members = append(members, fmt.Sprintf("%s: %s", exported, local))
	}
	if len(members) == 0 {
		return "", errors.New("lib/cat.js 的 export 为空")
	}
	body := catVodExportPattern.ReplaceAllString(script, "")
	return body + "\n;globalThis.__cat = {" + strings.Join(members, ", ") + "};\n", nil
}

// catVodTransformSpider 蜘蛛脚本 → 普通脚本：
//  1. 删除 import 语句；
//  2. 记录 import 的绑定名，稍后从 __cat 注入；
//  3. 去掉顶层声明的 export 关键字。
func catVodTransformSpider(script string) (string, []string) {
	names := []string{}
	for _, match := range catVodImportPattern.FindAllStringSubmatch(script, -1) {
		if specifiers := strings.TrimSpace(match[1]); specifiers != "" {
			for _, item := range strings.Split(specifiers, ",") {
				item = strings.TrimSpace(item)
				if item == "" {
					continue
				}
				// `b as c` 时脚本里用的是 c。
				if index := strings.Index(item, " as "); index >= 0 {
					names = append(names, strings.TrimSpace(item[index+4:]))
					continue
				}
				names = append(names, item)
			}
			continue
		}
		if defaultName := strings.TrimSpace(match[2]); defaultName != "" {
			names = append(names, defaultName)
		}
	}
	body := catVodImportPattern.ReplaceAllString(script, "")
	body = catVodBareImportPattern.ReplaceAllString(body, "")
	body = catVodExportMemberPattern.ReplaceAllString(body, "$1 ")
	return body, names
}

// catVodInjectBindings 把 __cat 的导出注入成蜘蛛脚本里的全局变量。
//
// 这里**不能**用 `var NAME = ...`：部分 lib/cat.js 变体在顶层用
// `let _ = ...` / `const _ = ...` 声明导出名（再 `export{_ as _}`），
// 与 var 声明同名会直接抛
// `SyntaxError: Identifier '_' has already been declared`。
// 因此改成先按「赋值」写入（能改 let、能落地到已有 var / 隐式全局），
// 常量绑定赋值会抛错，捕获后保留库自身的值（通常就是同一份导出）；
// 再兜一层 globalThis 赋值，覆盖脚本作用域里没有同名绑定的情况。
func catVodInjectBindings(vm *goja.Runtime, names []string) error {
	if len(names) == 0 {
		return nil
	}
	var builder strings.Builder
	for _, name := range names {
		if !gojaIdentifierPattern.MatchString(name) {
			continue
		}
		fmt.Fprintf(&builder, "try { %s = __cat ? __cat.%s : undefined; } catch (e) {}\n", name, name)
		fmt.Fprintf(
			&builder,
			"if (typeof %s === 'undefined' && globalThis.__cat) { globalThis[%q] = __cat.%s; }\n",
			name, name, name,
		)
	}
	if builder.Len() == 0 {
		return nil
	}
	_, err := vm.RunString(builder.String())
	return err
}

// gojaIdentifierPattern 合法的 JS 标识符（防注入）。
var gojaIdentifierPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// catVodInstallShims Node 侧最小垫片：打包产物加载时会探测这些全局。
func catVodInstallShims(vm *goja.Runtime) error {
	_, err := vm.RunString(`
		if (typeof globalThis.process === 'undefined') {
			globalThis.process = {
				env: {}, platform: 'android', version: 'v20.0.0', arch: 'arm64',
				cwd: function () { return '/'; },
				nextTick: function (fn) { fn(); }
			};
		}
		if (typeof globalThis.console === 'undefined') {
			globalThis.console = {
				log: function () {}, error: function () {}, warn: function () {},
				info: function () {}, debug: function () {}
			};
		}
		if (typeof globalThis.Buffer === 'undefined') {
			globalThis.Buffer = {
				isBuffer: function (value) { return value instanceof Uint8Array; },
				from: function (value, encoding) {
					if (value instanceof Uint8Array) return value;
					var text = String(value);
					var out = new Uint8Array(text.length);
					for (var i = 0; i < text.length; i++) out[i] = text.charCodeAt(i) & 255;
					return out;
				},
				alloc: function (size) { return new Uint8Array(size || 0); },
				concat: function (list) {
					var total = 0;
					for (var i = 0; i < list.length; i++) total += (list[i] && list[i].length) || 0;
					var out = new Uint8Array(total);
					var at = 0;
					for (var j = 0; j < list.length; j++) { out.set(list[j] || [], at); at += (list[j] && list[j].length) || 0; }
					return out;
				},
				byteLength: function (value) { return String(value).length; }
			};
		}
		if (typeof globalThis.setTimeout === 'undefined') {
			globalThis.setTimeout = function (fn) { fn(); return 0; };
		}
		if (typeof globalThis.clearTimeout === 'undefined') {
			globalThis.clearTimeout = function () {};
		}
	`)
	return err
}

// catVodLibFile lib/cat.js 的缓存文件名（按 URL 哈希）。
func catVodLibFile(address string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(address)))
	return catVodLibDir + "/" + hex.EncodeToString(sum[:])[:16] + ".js"
}

// ensureCatVodLib 确保 lib/cat.js 已下载、转换并缓存；返回缓存文件路径。
func (d *Downloader) ensureCatVodLib(ctx context.Context, address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", errors.New("没有找到 lib/cat.js 地址（蜘蛛的 import 语句）")
	}
	if d.drpy == nil {
		return "", errors.New("脚本运行环境不可用")
	}
	file := catVodLibFile(address)
	path := filepath.Join(d.drpy.directory, filepath.FromSlash(file))
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return file, nil
	}
	fetched, err := d.fetchProviderText(ctx, address, address)
	if err != nil {
		return "", fmt.Errorf("lib/cat.js 下载失败：%w", publicError(err))
	}
	if len(fetched) > drpyScriptMaxBytes {
		return "", errors.New("lib/cat.js 过大")
	}
	transformed, err := catVodTransformLib(fetched)
	if err != nil {
		return "", fmt.Errorf("lib/cat.js 处理失败：%w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("lib/cat.js 目录创建失败：%w", publicError(err))
	}
	if err := os.WriteFile(path, []byte(transformed), 0o600); err != nil {
		return "", fmt.Errorf("lib/cat.js 保存失败：%w", publicError(err))
	}
	catVodProgramCache.Delete(path)
	return file, nil
}

// catVodLibProgram 取（或编译并缓存）lib/cat.js 的 Program。
func (d *Downloader) catVodLibProgram(file string) (*goja.Program, error) {
	path := filepath.Join(d.drpy.directory, filepath.FromSlash(file))
	if cached, ok := catVodProgramCache.Load(path); ok {
		if program, ok := cached.(*goja.Program); ok {
			return program, nil
		}
	}
	script, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("lib/cat.js 读取失败，请重新导入该源")
	}
	if len(script) > drpyScriptMaxBytes {
		return nil, errors.New("lib/cat.js 过大")
	}
	program, err := goja.Compile("cat-lib.js", string(script), false)
	if err != nil {
		return nil, fmt.Errorf("lib/cat.js 语法错误：%s", drpyCompileError(err))
	}
	catVodProgramCache.Store(path, program)
	return program, nil
}

// catVodInvoke 在全新 Runtime 里加载 lib/cat.js + 蜘蛛脚本，
// 取到 __jsEvalReturn() 的 API 对象后交给 invoke。
func (d *Downloader) catVodInvoke(ctx context.Context, site drpySite, invoke func(vm *goja.Runtime) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if d.drpy == nil {
		return errors.New("脚本运行环境不可用")
	}
	program, err := d.drpy.programFor(site)
	if err != nil {
		return err
	}
	if strings.TrimSpace(site.CatLib) == "" {
		return errors.New("该源缺少 CatVod 运行时库（lib/cat.js），请删除后重新导入")
	}
	libProgram, err := d.catVodLibProgram(site.CatLib)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, drpyCallTimeout)
	defer cancel()
	vm := goja.New()
	host := &drpyHost{downloader: d, ctx: callCtx, vm: vm, items: map[string]string{}}
	installDrpyHost(vm, host)
	stop := time.AfterFunc(drpyCallTimeout, func() {
		vm.Interrupt("CatVod 脚本执行超时")
		cancel()
	})
	defer stop.Stop()
	if err := catVodInstallShims(vm); err != nil {
		return err
	}
	if _, err := vm.RunProgram(libProgram); err != nil {
		return fmt.Errorf("lib/cat.js 执行失败：%w", drpyWrapError(err))
	}
	if err := installDrpyCallHelper(vm); err != nil {
		return err
	}
	if err := installCatVodCallHelper(vm); err != nil {
		return err
	}
	// 蜘蛛脚本运行前注入 import 的绑定名。
	if err := catVodInjectBindings(vm, catVodSiteBindings(site)); err != nil {
		return err
	}
	if _, err := vm.RunProgram(program); err != nil {
		return drpyWrapError(err)
	}
	if _, err := vm.RunString(
		"globalThis.__catApi = (typeof __jsEvalReturn === 'function') ? __jsEvalReturn() : null;",
	); err != nil {
		return drpyWrapError(err)
	}
	if value := vm.Get("__catApi"); value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return errors.New("蜘蛛没有提供 __jsEvalReturn 入口")
	}
	if err := invoke(vm); err != nil {
		return drpyWrapError(err)
	}
	return nil
}

// catVodMetaPattern CatVod 蜘蛛顶部的 `let key = 'xx'` / `const HOST = 'xx'`。
var (
	catVodKeyPattern  = regexp.MustCompile(`(?m)^\s*(?:let|var|const)\s+key\s*=\s*['"]([^'"]{1,48})['"]`)
	catVodHostPattern = regexp.MustCompile(`(?m)^\s*(?:let|var|const)\s+HOST\s*=\s*['"]([^'"]{1,120})['"]`)
)

// catVodExtractMeta 静态提取蜘蛛的展示名与站点 host。
func catVodExtractMeta(script string) (name, host string) {
	if match := catVodKeyPattern.FindStringSubmatch(script); match != nil {
		name = strings.TrimSpace(match[1])
	}
	if match := catVodHostPattern.FindStringSubmatch(script); match != nil {
		host = strings.TrimSpace(match[1])
	}
	return name, host
}

// importCatVodScript 导入 CatVod 蜘蛛：
//  1. 按蜘蛛地址解析 ./lib/cat.js 并下载、转换、缓存；
//  2. 蜘蛛脚本剥离 import / export 后保存（programFor 按普通脚本编译）；
//  3. 记录 import 绑定名，运行前注入。
func (engine *nativeEngine) importCatVodScript(
	ctx context.Context,
	script string,
	scriptURL string,
) (any, error) {
	if scriptURL == "" {
		return nil, errors.New(
			"CatVod 蜘蛛请粘贴脚本地址：需要按相对路径取 ./lib/cat.js，" +
				"直接粘贴脚本文本无法定位依赖库",
		)
	}
	libURL := catVodResolveURL(scriptURL, catVodLibPath(script))
	if libURL == "" {
		return nil, errors.New("没有从蜘蛛脚本里解析出 lib/cat.js 的地址")
	}
	catLib, err := engine.downloader.ensureCatVodLib(ctx, libURL)
	if err != nil {
		return nil, err
	}
	body, bindings := catVodTransformSpider(script)
	if _, err := goja.Compile("catvod.js", body, false); err != nil {
		return nil, fmt.Errorf("蜘蛛脚本语法错误：%s", drpyCompileError(err))
	}
	name, host := catVodExtractMeta(script)
	if name == "" {
		name = "CatVod 蜘蛛"
	}
	key := drpyKeyFromScript(script)
	site := drpySite{
		Key:       key,
		Name:      name,
		ScriptURL: scriptURL,
		File:      drpyScriptsDir + "/" + key + ".js",
		Host:      host,
		Engine:    catVodEngine,
		CatLib:    catLib,
		LibURL:    libURL,
		Bindings:  strings.Join(bindings, ","),
	}
	if err := os.MkdirAll(filepath.Join(engine.directory, drpyScriptsDir), 0o755); err != nil {
		return nil, fmt.Errorf("脚本目录创建失败：%w", publicError(err))
	}
	if err := os.WriteFile(
		filepath.Join(engine.directory, filepath.FromSlash(site.File)),
		[]byte(body), 0o600,
	); err != nil {
		return nil, fmt.Errorf("脚本保存失败：%w", publicError(err))
	}
	added, err := engine.downloader.drpy.put(site)
	if err != nil {
		return nil, fmt.Errorf("脚本源保存失败：%w", publicError(err))
	}
	engine.downloader.drpy.invalidateProgram(key)
	return map[string]any{
		"key":    site.Key,
		"name":   site.Name,
		"title":  site.Name,
		"host":   site.Host,
		"file":   site.File,
		"engine": catVodEngine,
		"added":  added,
		"total":  1,
	}, nil
}

// catVodDecodeMacCMS CatVod 的方法返回的是 **JSON 字符串**（drpy2 直接返回
// 对象），先解一层再复用 drpy 的 MacCMS 结构映射。
func catVodDecodeMacCMS(raw any) (*macCMSResponse, error) {
	if text, ok := raw.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(strings.TrimSpace(text)), &decoded) != nil {
			return nil, errors.New("蜘蛛返回的内容不是 JSON")
		}
		raw = decoded
	}
	return drpyDecodeMacCMS(raw)
}

// catVodInit CatVod 的 init(cfg)：我们是从订阅里单独导入的蜘蛛，没有
// TVBox 的站点配置，用空配置调用即可；init 缺失或失败不致命。
func catVodInit(vm *goja.Runtime) {
	_, _ = catVodCall(vm, "init", map[string]any{
		"skey":  "",
		"stype": 0,
		"ext":   map[string]any{},
	})
}

// fetchCatVodCategories 跑 home(filter) 取分类。
func (d *Downloader) fetchCatVodCategories(ctx context.Context, site drpySite) ([]nativeCategory, error) {
	var categories []nativeCategory
	err := d.catVodInvoke(ctx, site, func(vm *goja.Runtime) error {
		catVodInit(vm)
		raw, callErr := catVodCall(vm, "home", true)
		if callErr != nil {
			return callErr
		}
		response, decodeErr := catVodDecodeMacCMS(raw)
		if decodeErr != nil {
			return decodeErr
		}
		for _, entry := range response.Class {
			id := strings.TrimSpace(nativeText(entry.TypeID))
			name := strings.TrimSpace(entry.TypeName)
			if id == "" || name == "" || len([]rune(name)) > 24 {
				continue
			}
			categories = append(categories, nativeCategory{ID: id, Name: name})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return categories, nil
}

// fetchCatVodCatalogPage 跑 homeVod / category / search 取一页列表。
func (d *Downloader) fetchCatVodCatalogPage(
	ctx context.Context,
	site drpySite,
	page int,
	category string,
	query string,
) ([]Drama, bool, error) {
	var dramas []Drama
	var hasMore bool
	err := d.catVodInvoke(ctx, site, func(vm *goja.Runtime) error {
		catVodInit(vm)
		var raw any
		var callErr error
		switch {
		case query != "":
			raw, callErr = catVodCall(vm, "search", query, false)
		case strings.TrimSpace(category) == "":
			// 首页：优先 homeVod；没有实现时退到 home() 的第一个分类，
			// 否则「全部 / 推荐」页会直接报 missing homeVod。
			if catVodHasFunction(vm, "homeVod") {
				raw, callErr = catVodCall(vm, "homeVod")
				break
			}
			firstTid := catVodFirstClassID(vm)
			if firstTid == "" {
				return errors.New("蜘蛛既没有 homeVod，也没有可用分类")
			}
			raw, callErr = catVodCall(vm, "category", firstTid, page, true, map[string]any{})
		default:
			raw, callErr = catVodCall(vm, "category", category, page, true, map[string]any{})
		}
		if callErr != nil {
			return callErr
		}
		response, decodeErr := catVodDecodeMacCMS(raw)
		if decodeErr != nil {
			return decodeErr
		}
		source := drpySourceID(site.Key)
		for _, row := range response.rows() {
			if row.Name == "" {
				continue
			}
			dramas = append(dramas, drpyDramaFromRow(source, row, ""))
		}
		hasMore = drpyHasMore(response, page)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return dramas, hasMore, nil
}

// catVodHasFunction 蜘蛛是否实现了某个 API 方法。
func catVodHasFunction(vm *goja.Runtime, name string) bool {
	value, err := vm.RunString(
		"(typeof __catApi === 'object' && __catApi !== null && typeof __catApi[" +
			strconv.Quote(name) + "] === 'function')",
	)
	if err != nil || value == nil {
		return false
	}
	return value.ToBoolean()
}

// catVodFirstClassID 取 home() 里第一个分类 ID（首页回退用）。
func catVodFirstClassID(vm *goja.Runtime) string {
	raw, err := catVodCall(vm, "home", true)
	if err != nil {
		return ""
	}
	response, decodeErr := catVodDecodeMacCMS(raw)
	if decodeErr != nil {
		return ""
	}
	for _, entry := range response.Class {
		if id := strings.TrimSpace(nativeText(entry.TypeID)); id != "" {
			return id
		}
	}
	return ""
}

// fetchCatVodDetail 跑 detail(id) 取剧集元数据与分集（返回结构与 MacCMS
// 同构，直接复用 drpy 的分集解析）。
func (d *Downloader) fetchCatVodDetail(
	ctx context.Context,
	site drpySite,
	vodID string,
) (Drama, []Chapter, error) {
	decoded, ok := drpyDecodeSegment(vodID)
	if !ok || strings.TrimSpace(decoded) == "" {
		return Drama{}, nil, errors.New("片源 ID 无效")
	}
	var drama Drama
	var chapters []Chapter
	err := d.catVodInvoke(ctx, site, func(vm *goja.Runtime) error {
		catVodInit(vm)
		raw, callErr := catVodCall(vm, "detail", decoded)
		if callErr != nil {
			return callErr
		}
		response, decodeErr := catVodDecodeMacCMS(raw)
		if decodeErr != nil {
			return decodeErr
		}
		rows := response.rows()
		if len(rows) == 0 {
			return errors.New("蜘蛛没有返回该剧的详情")
		}
		row := rows[0]
		first := response.List[0]
		source := drpySourceID(site.Key)
		drama = drpyDramaFromRow(source, row, libretvHTMLText(first.VodContent))
		playFrom := strings.Split(first.VodPlayFrom, "$$$")
		playGroups := strings.Split(first.VodPlayURL, "$$$")
		chapters = drpyChaptersFromGroups(source, decoded, drpySiteReferer(site), playFrom, playGroups)
		return nil
	})
	if err != nil {
		return Drama{}, nil, err
	}
	return drama, chapters, nil
}

// catVodCallPlay 跑 play(flag, id, flags) 取播放地址；
// 返回 {url, parse} 与 drpy 的 play 结果结构一致，后续分流复用同一套。
func catVodCallPlay(vm *goja.Runtime, flag, id string) (drpyPlayResult, error) {
	raw, err := catVodCall(vm, "play", flag, id, []string{})
	if err != nil {
		return drpyPlayResult{}, err
	}
	if raw == nil {
		return drpyPlayResult{}, errors.New("蜘蛛未返回播放地址")
	}
	return drpyPlayResultFrom(raw), nil
}

// catVodSiteBindings 导入时记录的 import 绑定名（存于 site.Bindings 字段）。
func catVodSiteBindings(site drpySite) []string {
	raw := strings.TrimSpace(site.Bindings)
	if raw == "" {
		return nil
	}
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
}

// installCatVodCallHelper 安装 __catCall（调 API 对象上的方法，
// 结果与 drpy 共用 __drpyResult/__drpyDone/__drpyError 通道）。
func installCatVodCallHelper(vm *goja.Runtime) error {
	_, err := vm.RunString(`
		globalThis.__catCall = function (fnName, args) {
			var api = globalThis.__catApi;
			if (!api || typeof api[fnName] !== 'function') {
				globalThis.__drpyError = '蜘蛛没有实现 ' + fnName + ' 方法';
				globalThis.__drpyDone = true;
				return;
			}
			Promise.resolve().then(function () {
				return api[fnName].apply(api, args || []);
			}).then(function (value) {
				globalThis.__drpyResult = value;
				globalThis.__drpyDone = true;
			}, function (error) {
				globalThis.__drpyError = String((error && error.message) || error);
				globalThis.__drpyDone = true;
			});
		};
	`)
	return err
}

// catVodCall 调用蜘蛛 API 的方法并等待结果（空 program 泵微任务）。
func catVodCall(vm *goja.Runtime, name string, args ...any) (any, error) {
	// 与 drpyCall 同理：每次调用前复位完成标记。
	if _, err := vm.RunString(
		"globalThis.__drpyDone = false; globalThis.__drpyError = null; globalThis.__drpyResult = null;",
	); err != nil {
		return nil, drpyWrapError(err)
	}
	if _, err := vm.RunString(fmt.Sprintf("__catCall(%q, %s)", name, drpyArgsLiteral(args))); err != nil {
		return nil, drpyWrapError(err)
	}
	noop, err := goja.Compile("", "", false)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(drpyCallTimeout)
	for !vm.Get("__drpyDone").ToBoolean() {
		if time.Now().After(deadline) {
			return nil, errors.New("CatVod 脚本执行超时")
		}
		if _, err := vm.RunProgram(noop); err != nil {
			return nil, drpyWrapError(err)
		}
	}
	if errorMessage := vm.Get("__drpyError"); errorMessage != nil && !goja.IsUndefined(errorMessage) && !goja.IsNull(errorMessage) {
		return nil, errors.New(strings.TrimSpace(errorMessage.String()))
	}
	return vm.Get("__drpyResult").Export(), nil
}
