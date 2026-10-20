package core

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// drpyCall 桥接单测：__drpyCall helper + 空 program 泵微任务的生产形态。
// 覆盖 M0 spike 的三项关键断言，并防护「req resolve 值字段名」回归
// （goja 把 Go 结构体转 JS 对象用字段名 Content，脚本读小写 content 会拿到
// undefined——曾导致 JSON.parse 失败）。

func TestDrpyCallDrivesAsyncFunction(t *testing.T) {
	vm := goja.New()
	vm.Set("fakeReq", func(call goja.FunctionCall) goja.Value {
		// 与生产 req 同构：返回已决断的 Promise，resolve 值为 map
		// （小写字段名，对齐 drpy2 响应形态）。
		promise, resolve, _ := vm.NewPromise()
		resolve(map[string]any{"status": 200, "content": "<html>ok</html>"})
		return vm.ToValue(promise)
	})
	if _, err := vm.RunString(`
		globalThis.fakeHome = async function(){
			var r = await fakeReq("http://x");
			return {class:[{type_id:"1",type_name:r.content+":"+r.status}]};
		};
	`); err != nil {
		t.Fatalf("RunString: %v", err)
	}
	if err := installDrpyCallHelper(vm); err != nil {
		t.Fatalf("installDrpyCallHelper: %v", err)
	}
	raw, err := drpyCall(vm, "fakeHome")
	if err != nil {
		t.Fatalf("drpyCall: %v", err)
	}
	data, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("结果不是 map: %T %v", raw, raw)
	}
	class, ok := data["class"].([]any)
	if !ok || len(class) != 1 {
		t.Fatalf("class 异常: %v", data["class"])
	}
	first, _ := class[0].(map[string]any)
	if first["type_name"] != "<html>ok</html>:200" {
		t.Fatalf("await 已决断 Promise 的解析值异常: %v", first)
	}
}

func TestDrpyCallMissingFunction(t *testing.T) {
	vm := goja.New()
	if err := installDrpyCallHelper(vm); err != nil {
		t.Fatalf("installDrpyCallHelper: %v", err)
	}
	if _, err := drpyCall(vm, "absentFunction"); err == nil || !strings.Contains(err.Error(), "absentFunction") {
		t.Fatalf("调用不存在的函数应报错，得到 %v", err)
	}
}

// goja 版本行为回归防护：Interrupt 可打断死循环脚本且文案可读
// （drpyInvoke 看门狗依赖此行为；goja 升级时优先看这条）。
func TestDrpyGojaInterruptWatchdog(t *testing.T) {
	vm := goja.New()
	stop := time.AfterFunc(50*time.Millisecond, func() { vm.Interrupt("drpy 脚本执行超时") })
	defer stop.Stop()
	_, err := vm.RunString(`while(true){}`)
	var interrupted *goja.InterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("期望 InterruptedError，得到 %v", err)
	}
	if interrupted.Value() != "drpy 脚本执行超时" {
		t.Fatalf("InterruptedError.Value()=%v", interrupted.Value())
	}
}

func TestDrpyWrapError(t *testing.T) {
	// 真实中断产生的 InterruptedError：只透出 Interrupt 时传入的文案。
	vm := goja.New()
	stop := time.AfterFunc(20*time.Millisecond, func() { vm.Interrupt("自定义中断文案") })
	_, err := vm.RunString(`while(true){}`)
	stop.Stop()
	var interrupted *goja.InterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("期望 InterruptedError，得到 %v", err)
	}
	if wrapped := drpyWrapError(interrupted); wrapped == nil || wrapped.Error() != "自定义中断文案" {
		t.Fatalf("中断文案未透出: %v", wrapped)
	}
	// 无文案的中断回落默认文案。
	if err := drpyWrapError(&goja.InterruptedError{}); err == nil || err.Error() != "drpy 脚本执行超时" {
		t.Fatalf("无文案的中断应回落默认文案，得到 %v", err)
	}
	// 普通错误去掉 goja 的 " at " 栈噪音。
	noisy := errors.New("TypeError: bad\n    at foo (file.js:1:1)\n    at bar (file.js:2:2)")
	if err := drpyWrapError(noisy); err == nil || strings.Contains(err.Error(), "file.js") {
		t.Fatalf("栈噪音未脱除: %v", err)
	}
	// 超长文案截断到 200 runes。
	long := errors.New(strings.Repeat("x", 500))
	if err := drpyWrapError(long); err == nil || len([]rune(err.Error())) != 201 {
		t.Fatalf("超长错误未截断: %d runes", len([]rune(err.Error())))
	}
	if drpyWrapError(nil) != nil {
		t.Fatal("nil 应原样返回")
	}
}

func TestDrpyJoinURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://a.test/api/vod", "/vod/list", "https://a.test/vod/list"},
		{"https://a.test", "detail/1", "https://a.test/detail/1"},
		{"https://a.test/", "detail/1", "https://a.test/detail/1"},
		{"https://a.test", "https://b.test/x", "https://b.test/x"},
		{"https://a.test", "//cdn.test/x", "//cdn.test/x"},
		{"", "detail/1", "detail/1"},
		{"https://a.test", "", "https://a.test"},
	}
	for _, item := range cases {
		if got := drpyJoinURL(item.base, item.path); got != item.want {
			t.Fatalf("joinUrl(%q, %q)=%q，期望 %q", item.base, item.path, got, item.want)
		}
	}
}

// req reject 链路：脚本 await 的请求失败时，错误经 Promise reject →
// __drpyError → drpyCall → fetchDrpyCatalogPage 透出（不静默返回空）。
func TestDrpyReqFailurePropagates(t *testing.T) {
	engine := sourceFixtureEngine(t, func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("fixture network down")
	})
	script := strings.ReplaceAll(drpyBrowseScript, "__HOST__", "http://unreachable.test")
	if _, err := engine.importDrpyScript(context.Background(), script); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	site, ok := engine.downloader.drpySite(drpySourceID(""))
	if ok {
		t.Fatalf("空 key 不应命中站点: %+v", site)
	}
	list := engine.drpySiteList()
	if len(list) != 1 {
		t.Fatalf("导入后应有 1 个站点: %+v", list)
	}
	key := list[0]["key"].(string)
	site, ok = engine.downloader.drpySite(drpySourceID(key))
	if !ok {
		t.Fatalf("找不到导入的站点 %q", key)
	}
	_, _, err := engine.downloader.fetchDrpyCatalogPage(context.Background(), site, 1, "1", "")
	if err == nil || !strings.Contains(err.Error(), "fixture network down") {
		t.Fatalf("req 失败应透出，得到 %v", err)
	}
}
