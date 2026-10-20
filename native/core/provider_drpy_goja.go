package core

// drpy 脚本的 goja 运行时桥接。
//
// 架构（M0 spike 实证结论）：
//   - req 同步阻塞：host 函数内在当前 JS 线程用 channel 等后台 goroutine 的
//     HTTP 结果，再包成已决断的 Promise 返回。规避 NewPromise 的 resolve 跨
//     goroutine 线程安全警告，同时兼容脚本里 `await req(url)` 与
//     `req(url).then(...)` 两种写法；
//   - 调用桥接：JS 侧 helper __drpyCall 把目标函数的返回（含 async/Promise）
//     收敛到全局变量 __drpyDone/__drpyResult/__drpyError，Go 侧用空 program
//     RunProgram 泵微任务（leave() 执行 jobQueue）直到完成；
//   - 看门狗：每次调用 AfterFunc(drpyCallTimeout) 触发 vm.Interrupt，
//     InterruptedError 文案脱敏后透出；
//   - Runtime 用后即弃（全局态隔离），Program 按 key 缓存（解析 2~8ms）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"
)

const drpyMobileUA = "Mozilla/5.0 (Linux; Android 12; Pixel 6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"

// drpyHost 一次脚本调用的宿主环境。
type drpyHost struct {
	downloader *Downloader
	ctx        context.Context
	vm         *goja.Runtime
	items      map[string]string
}

// drpyResponse req 的返回结构（drpy2 标准：{status, headers, content}）。
type drpyResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Content string            `json:"content"`
	// Data 兼容部分脚本读 r.data 的变体（与 content 同值）。
	Data string `json:"data,omitempty"`
}

// drpyInvoke 在全新 Runtime 里执行脚本并调用 invoke 回调（回调内用 drpyCall
// 触发脚本函数）。Program 命中缓存；看门狗覆盖整个 invoke 生命周期
// （Interrupt 中断 JS + cancel ctx 唤醒阻塞在 req 的当前线程）。
func (d *Downloader) drpyInvoke(ctx context.Context, site drpySite, invoke func(vm *goja.Runtime) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	program, err := d.drpy.programFor(site)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, drpyCallTimeout)
	defer cancel()
	vm := goja.New()
	host := &drpyHost{downloader: d, ctx: callCtx, vm: vm, items: map[string]string{}}
	installDrpyHost(vm, host)
	stop := time.AfterFunc(drpyCallTimeout, func() {
		vm.Interrupt("drpy 脚本执行超时")
		cancel()
	})
	defer stop.Stop()
	if _, err := vm.RunProgram(program); err != nil {
		return drpyWrapError(err)
	}
	if err := installDrpyCallHelper(vm); err != nil {
		return err
	}
	if err := invoke(vm); err != nil {
		return drpyWrapError(err)
	}
	return nil
}

// installDrpyHost 安装宿主函数（req/pdfh/pdfa/pd/joinUrl/MOBILE_UA/getItem/setItem/log）。
func installDrpyHost(vm *goja.Runtime, host *drpyHost) {
	// req：同步阻塞拉取后返回已决断的 Promise。resolve/reject 都在当前 JS
	// 线程调用（NewPromise 的跨 goroutine 限制），同时兼容脚本里
	// `await req(url)` 与 `req(url).then(...)` 两种写法。
	vm.Set("req", func(call goja.FunctionCall) goja.Value {
		target := strings.TrimSpace(call.Argument(0).String())
		if target == "" {
			promise, _, reject := vm.NewPromise()
			reject("req 缺少 url 参数")
			return vm.ToValue(promise)
		}
		options := drpyExportOptions(call.Argument(1))
		promise, resolve, reject := vm.NewPromise()
		response, err := host.fetch(target, options)
		if err != nil {
			reject(publicError(err).Error())
			return vm.ToValue(promise)
		}
		// resolve 用 map 而非 drpyResponse 结构体：goja 把 Go 结构体转成
		// JS 对象时用的是 Go 字段名（Content），drpy 脚本惯例读小写的
		// res.content / res.data（drpy2 标准响应形态），必须对齐。
		resolve(map[string]any{
			"status":  response.Status,
			"headers": response.Headers,
			"content": response.Content,
			"data":    response.Content,
		})
		return vm.ToValue(promise)
	})
	vm.Set("pdfh", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(drpyPDFH(call.Argument(0).String(), call.Argument(1).String()))
	})
	vm.Set("pdfa", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(drpyPDFA(call.Argument(0).String(), call.Argument(1).String()))
	})
	vm.Set("pd", func(call goja.FunctionCall) goja.Value {
		rule := call.Argument(1).String()
		if extra := strings.TrimSpace(call.Argument(2).String()); extra != "" && extra != "undefined" {
			rule += "&&" + extra
		}
		return vm.ToValue(drpyPD(call.Argument(0).String(), rule))
	})
	vm.Set("joinUrl", func(call goja.FunctionCall) goja.Value {
		base := call.Argument(0).String()
		path := call.Argument(1).String()
		return vm.ToValue(drpyJoinURL(base, path))
	})
	vm.Set("MOBILE_UA", drpyMobileUA)
	vm.Set("getItem", func(call goja.FunctionCall) goja.Value {
		key := strings.TrimSpace(call.Argument(0).String())
		value, ok := host.items[key]
		if !ok {
			return goja.Null()
		}
		return vm.ToValue(value)
	})
	vm.Set("setItem", func(call goja.FunctionCall) goja.Value {
		key := strings.TrimSpace(call.Argument(0).String())
		host.items[key] = call.Argument(1).String()
		return goja.Undefined()
	})
	vm.Set("log", func(call goja.FunctionCall) goja.Value {
		return goja.Undefined()
	})
	// setResult rule 驱动型脚本的「一级 / 搜索 / 推荐」片段用它交回列表
	// （见 provider_drpy_rule.go）。
	vm.Set("setResult", func(call goja.FunctionCall) goja.Value {
		_ = vm.Set("__drpyRuleResult", call.Argument(0))
		return goja.Undefined()
	})
}

// drpyExportOptions 解析 req 的第二参（method/headers/body）。
func drpyExportOptions(value goja.Value) map[string]any {
	options := map[string]any{}
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return options
	}
	exported := value.Export()
	data, ok := exported.(map[string]any)
	if !ok {
		return options
	}
	for key, item := range data {
		options[key] = item
	}
	return options
}

// drpyFetch 同步阻塞拉取：HTTP 走 Downloader client（代理/UA/限流全继承），
// 当前 JS 线程阻塞等待结果，全程不跨 goroutine 操作 vm。ctx 超时由
// drpyInvoke 的看门狗 cancel 触发。
func (host *drpyHost) fetch(target string, options map[string]any) (drpyResponse, error) {
	method := strings.ToUpper(drpyOptionString(options, "method"))
	if method == "" {
		method = http.MethodGet
	}
	headers := map[string]string{}
	if raw, ok := options["headers"]; ok {
		if data, valid := raw.(map[string]any); valid {
			for key, item := range data {
				headers[key] = fmt.Sprint(item)
			}
		}
	}
	body, contentType := drpyRequestBody(options)
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(host.ctx, method, target, bodyReader)
	if err != nil {
		return drpyResponse{}, err
	}
	request.Header.Set("User-Agent", drpyMobileUA)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if referer := drpyOptionString(options, "Referer"); referer != "" {
		request.Header.Set("Referer", referer)
	}
	if host.downloader.limiter != nil {
		release, err := host.downloader.limiter.acquire(host.ctx, request)
		if err != nil {
			return drpyResponse{}, err
		}
		defer release()
	}
	response, err := host.downloader.client.Do(request)
	if err != nil {
		return drpyResponse{}, err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, drpyRespMaxBytes)
	content, err := io.ReadAll(limited)
	if err != nil {
		return drpyResponse{}, err
	}
	flat := map[string]string{}
	for key, values := range response.Header {
		if len(values) > 0 {
			flat[key] = values[0]
		}
	}
	return drpyResponse{Status: response.StatusCode, Headers: flat, Content: string(content), Data: string(content)}, nil
}

func drpyOptionString(options map[string]any, key string) string {
	if value, ok := options[key]; ok && value != nil {
		if text, valid := value.(string); valid {
			return strings.TrimSpace(text)
		}
		return strings.TrimSpace(fmt.Sprint(value))
	}
	return ""
}

// installDrpyCallHelper 安装 __drpyCall 桥接 helper（M0 spike 验证的形态）。
func installDrpyCallHelper(vm *goja.Runtime) error {
	_, err := vm.RunString(`
		globalThis.__drpyDone = false;
		globalThis.__drpyResult = null;
		globalThis.__drpyError = null;
		globalThis.__drpyCall = function(fnName, args){
			var fn = globalThis[fnName];
			if (typeof fn !== 'function') {
				globalThis.__drpyError = '脚本没有定义 ' + fnName + ' 函数';
				globalThis.__drpyDone = true;
				return;
			}
			Promise.resolve().then(function(){ return fn.apply(null, args || []); }).then(function(value){
				globalThis.__drpyResult = value;
				globalThis.__drpyDone = true;
			}, function(error){
				globalThis.__drpyError = String(error && error.message || error);
				globalThis.__drpyDone = true;
			});
		};
	`)
	return err
}

// drpyCall 调用脚本函数并等待结果（空 program 泵微任务）。
// 返回 Export() 后的任意值；调用方负责转成目标结构。
func drpyCall(vm *goja.Runtime, name string, args ...any) (any, error) {
	// 复位完成标记：上一次调用已经把它置为 true，不复位的话下一调用会立刻
	// 读到上一次的结果（play 失败后再调 lazy 就会拿到 play 的错误）。
	if _, err := vm.RunString(
		"globalThis.__drpyDone = false; globalThis.__drpyError = null; globalThis.__drpyResult = null;",
	); err != nil {
		return nil, drpyWrapError(err)
	}
	if _, err := vm.RunString(fmt.Sprintf("__drpyCall(%q, %s)", name, drpyArgsLiteral(args))); err != nil {
		return nil, drpyWrapError(err)
	}
	noop, err := goja.Compile("", "", false)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(drpyCallTimeout)
	for !vm.Get("__drpyDone").ToBoolean() {
		if time.Now().After(deadline) {
			return nil, errors.New("drpy 脚本执行超时")
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

// drpyArgsLiteral 把参数序列化成 JS 字面量（经 JSON，避免注入）。
func drpyArgsLiteral(args []any) string {
	if len(args) == 0 {
		return "[]"
	}
	body, err := json.Marshal(args)
	if err != nil {
		return "[]"
	}
	return string(body)
}

// drpyWrapError 脱敏 goja 错误：InterruptedError 只透出自定义文案，
// 其余去掉 goja 的栈与文件名噪音。
func drpyWrapError(err error) error {
	if err == nil {
		return nil
	}
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		if text, ok := interrupted.Value().(string); ok && strings.TrimSpace(text) != "" {
			return errors.New(text)
		}
		return errors.New("drpy 脚本执行超时")
	}
	text := strings.TrimSpace(err.Error())
	if index := strings.Index(text, " at "); index > 0 {
		text = text[:index]
	}
	if len([]rune(text)) > 200 {
		text = string([]rune(text)[:200]) + "…"
	}
	if text == "" {
		return errors.New("drpy 脚本执行失败")
	}
	return errors.New(text)
}

// drpyJoinURL 拼接相对地址（drpy 的 joinUrl 语义）。
func drpyJoinURL(base, path string) string {
	base = strings.TrimSpace(base)
	path = strings.TrimSpace(path)
	if path == "" {
		return base
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "//") {
		return path
	}
	if base == "" {
		return path
	}
	if strings.HasPrefix(path, "/") {
		if index := strings.Index(base, "://"); index >= 0 {
			if slash := strings.Index(base[index+3:], "/"); slash >= 0 {
				return base[:index+3+slash] + path
			}
			return base + path
		}
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + strings.TrimPrefix(path, "/")
}

// drpyString 安全取 map 里的字符串字段。
func drpyString(source map[string]any, key string) string {
	if value, ok := source[key]; ok && value != nil {
		return strings.TrimSpace(fmt.Sprint(value))
	}
	return ""
}

// drpyInt 安全取 map 里的整数字段。goja 的 Export() 对 JS 整数返回
// int64（非 float64），字符串数字（部分脚本）也要认。
func drpyInt(source map[string]any, key string) int {
	value, ok := source[key]
	if !ok || value == nil {
		return 0
	}
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case float32:
		return int(typed)
	case int64:
		return int(typed)
	case int32:
		return int(typed)
	case int:
		return typed
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0
		}
		return parsed
	}
	return 0
}

// drpyRequestBody 组装请求体。
//
// drpy2 用 body（字符串），CatVod 蜘蛛普遍用 data + postType：
//
//	req(url, {method: 'post', data: {a: 1}, postType: 'json'})
//
// 以前只认 body，POST 实际发的是空体，服务端返回错误 JSON，脚本里
// 读 result/items 就报 "Cannot read property 'items' of undefined"。
func drpyRequestBody(options map[string]any) (body, contentType string) {
	postType := strings.ToLower(strings.TrimSpace(drpyOptionString(options, "postType")))
	if raw := drpyOptionString(options, "body"); raw != "" {
		switch postType {
		case "json":
			return raw, "application/json"
		case "form":
			return raw, "application/x-www-form-urlencoded"
		}
		return raw, ""
	}
	data, ok := options["data"]
	if !ok || data == nil {
		return "", ""
	}
	if text, isText := data.(string); isText {
		switch postType {
		case "form":
			return text, "application/x-www-form-urlencoded"
		case "json", "":
			return text, "application/json"
		}
		return text, ""
	}
	if postType == "form" {
		form := url.Values{}
		for key, value := range drpyAnyMap(data) {
			form.Set(key, fmt.Sprint(value))
		}
		return form.Encode(), "application/x-www-form-urlencoded"
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return "", ""
	}
	return string(encoded), "application/json"
}

// drpyAnyMap 把 Export 出来的对象统一成 map[string]any（数组等返回空）。
func drpyAnyMap(value any) map[string]any {
	if data, ok := value.(map[string]any); ok {
		return data
	}
	return map[string]any{}
}
