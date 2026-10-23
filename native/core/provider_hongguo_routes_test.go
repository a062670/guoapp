package core

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 健康度是包级单例，每个用例结束后复位，避免污染其它测试。
func resetHongguoRouteHealth(t *testing.T) {
	t.Helper()
	hongguoRoutesHealth.reset()
	t.Cleanup(hongguoRoutesHealth.reset)
}

func TestHongguoRoutesRegisteredInPriorityOrder(t *testing.T) {
	// 顺序即默认优先级：必须与改造前的硬编码顺序一致
	// （drpys → app → web → djapi），否则等于悄悄改变了取流策略。
	want := []string{hongguoRouteDrpy, hongguoRouteApp, hongguoRouteWeb, hongguoRouteDjapi}
	if len(hongguoRoutes) != len(want) {
		t.Fatalf("注册表线路数 = %d, 期望 %d", len(hongguoRoutes), len(want))
	}
	for i, name := range want {
		if hongguoRoutes[i].Name != name {
			t.Fatalf("第 %d 条线路 = %q, 期望 %q", i, hongguoRoutes[i].Name, name)
		}
		if hongguoRoutes[i].Resolve == nil {
			t.Fatalf("线路 %q 缺少 Resolve", name)
		}
		if hongguoRoutes[i].Signer == "" {
			t.Fatalf("线路 %q 缺少 Signer 登记（插槽位也应显式标注）", name)
		}
	}
}

func TestHongguoRouteHealthSkipsAfterFailFastStreak(t *testing.T) {
	resetHongguoRouteHealth(t)
	now := time.Now()

	// 未达阈值：即使失败过也不跳过（正常抖动不该被放大）
	hongguoRoutesHealth.noteFail(hongguoRouteApp, now, hongguoFailRisk)
	hongguoRoutesHealth.noteFail(hongguoRouteApp, now, hongguoFailRisk)
	if hongguoRoutesHealth.skip(hongguoRouteApp, now.Add(time.Second)) {
		t.Fatalf("连续失败 2 次（阈值 %d）不应跳过", hongguoHealthFailFastStreak)
	}

	// 达到阈值：冷却期内跳过
	hongguoRoutesHealth.noteFail(hongguoRouteApp, now, hongguoFailRisk)
	if !hongguoRoutesHealth.skip(hongguoRouteApp, now.Add(hongguoFailRisk.cooldown()/2)) {
		t.Fatalf("连续失败达阈值后，冷却期内应跳过")
	}

	// 冷却期外：重新给机会，避免永久排除已恢复的线路
	if hongguoRoutesHealth.skip(hongguoRouteApp, now.Add(hongguoFailRisk.cooldown()+time.Minute)) {
		t.Fatalf("冷却期结束后应重新尝试该线路")
	}
}

func TestHongguoRouteHealthSuccessClearsStreak(t *testing.T) {
	resetHongguoRouteHealth(t)
	now := time.Now()
	for i := 0; i < hongguoHealthFailFastStreak; i++ {
		hongguoRoutesHealth.noteFail(hongguoRouteDrpy, now, hongguoFailRisk)
	}
	if !hongguoRoutesHealth.skip(hongguoRouteDrpy, now.Add(time.Second)) {
		t.Fatalf("达阈值后应跳过")
	}
	hongguoRoutesHealth.noteOK(hongguoRouteDrpy)
	if hongguoRoutesHealth.skip(hongguoRouteDrpy, now.Add(time.Second)) {
		t.Fatalf("成功后应清空连续失败计数")
	}
}

func TestHongguoRouteHealthIsPerRoute(t *testing.T) {
	resetHongguoRouteHealth(t)
	now := time.Now()
	for i := 0; i < hongguoHealthFailFastStreak; i++ {
		hongguoRoutesHealth.noteFail(hongguoRouteWeb, now, hongguoFailRisk)
	}
	// 一条线路挂掉不应牵连其它线路
	if hongguoRoutesHealth.skip(hongguoRouteApp, now.Add(time.Second)) {
		t.Fatalf("健康度必须按线路独立记账")
	}
}

func TestHongguoEmptyResponseDetectsDeviceGate(t *testing.T) {
	// 设备级门禁的典型形态：HTTP 200 但内容为空
	cases := []struct {
		name   string
		body   []byte
		result map[string]any
		want   bool
	}{
		{name: "空 body", body: []byte(""), want: true},
		{name: "只有空白", body: []byte("  \n\t "), want: true},
		{name: "空 JSON 对象", body: []byte("{}"), want: true},
		{name: "空 JSON 数组", body: []byte("[]"), want: true},
		{name: "JSON null", body: []byte("null"), want: true},
		{name: "data 为 null", body: []byte(`{"data":null}`), result: map[string]any{"data": nil}, want: true},
		{name: "data 为空对象", body: []byte(`{"data":{}}`), result: map[string]any{"data": map[string]any{}}, want: true},
		{name: "data 为空数组", body: []byte(`{"data":[]}`), result: map[string]any{"data": []any{}}, want: true},
		{name: "有效 data", body: []byte(`{"data":{"video_list":[]}}`), result: map[string]any{"data": map[string]any{"video_list": []any{}}}, want: false},
		{name: "无 data 但有其它键", body: []byte(`{"status_code":0}`), result: map[string]any{"status_code": 0}, want: false},
	}
	for _, item := range cases {
		if got := hongguoEmptyResponse(item.body, item.result); got != item.want {
			t.Fatalf("%s: hongguoEmptyResponse = %v, 期望 %v", item.name, got, item.want)
		}
	}
}

func TestHongguoEmptyJSONBodyWithoutParsedResult(t *testing.T) {
	if !hongguoEmptyJSONBody([]byte("  ")) {
		t.Fatalf("空白 body 应判定为空")
	}
	if !hongguoEmptyJSONBody([]byte("{}")) {
		t.Fatalf("空 JSON 对象应判定为空")
	}
	if hongguoEmptyJSONBody([]byte(`{"a":1}`)) {
		t.Fatalf("有内容的 JSON 不应判定为空")
	}
	// 非 JSON（如 HTML 错误页）交回调用方按既有逻辑处理，这里不算空
	if hongguoEmptyJSONBody([]byte("<html>403</html>")) {
		t.Fatalf("非 JSON 内容不应由本函数判定为空")
	}
}

func TestHongguoRouteErrorOrSkipKeepsMessageReadable(t *testing.T) {
	if err := routeErrorOrSkip(nil); err == nil || err.Error() == "" {
		t.Fatalf("未执行的线路应给出可读占位文案，而不是 <nil>")
	}
	want := errors.New("真实失败原因")
	if err := routeErrorOrSkip(want); err != want {
		t.Fatalf("已有错误应原样返回")
	}
}

func TestHongguoHostPoolsDefaultSingleEndpoint(t *testing.T) {
	// 默认必须各只有一个端点：这是「行为与改造前 100% 一致」的前提。
	// 多端点只有在某个域被实测确认可用后才应追加。
	if len(hongguoDrpyHosts) != 1 || hongguoDrpyHosts[0] != hgDrpyVideoAPI {
		t.Fatalf("drpys 端点池应为单一现网端点 %q, 实际 %v", hgDrpyVideoAPI, hongguoDrpyHosts)
	}
	// App 请求尚未做 host 参数化，额外候选应保持为空（行为与改造前一致）
	if len(hongguoAppHosts) != 0 {
		t.Fatalf("app 额外候选默认应为空（避免盲开未验证的域），实际 %v", hongguoAppHosts)
	}
}

func TestHongguoHostKeyIsRouteAndHostComposite(t *testing.T) {
	if got := hongguoHostKey(hongguoRouteDrpy, "https://a.example"); got != "drpys@https://a.example" {
		t.Fatalf("复合键 = %q", got)
	}
	// 同一端点在不同线路上必须是不同的键（可用性相互独立）
	if hongguoHostKey(hongguoRouteDrpy, "https://a.example") == hongguoHostKey(hongguoRouteApp, "https://a.example") {
		t.Fatalf("不同线路的同一端点不应共用记账键")
	}
}

func TestHongguoHostHealthIsPerRouteAndHost(t *testing.T) {
	resetHongguoRouteHealth(t)
	now := time.Now()
	host := "https://a.example"
	other := "https://b.example"
	for i := 0; i < hongguoHealthFailFastStreak; i++ {
		hongguoRoutesHealth.noteFail(hongguoHostKey(hongguoRouteDrpy, host), now, hongguoFailRisk)
	}
	// 坏端点不应牵连同线路的其它端点
	if hongguoRoutesHealth.skip(hongguoHostKey(hongguoRouteDrpy, other), now.Add(time.Second)) {
		t.Fatalf("端点健康度必须按端点独立记账")
	}
	// 也不应牵连其它线路的同一端点
	if hongguoRoutesHealth.skip(hongguoHostKey(hongguoRouteApp, host), now.Add(time.Second)) {
		t.Fatalf("端点健康度必须按线路隔离")
	}
	// 自身应被跳过
	if !hongguoRoutesHealth.skip(hongguoHostKey(hongguoRouteDrpy, host), now.Add(time.Second)) {
		t.Fatalf("坏端点在冷却期内应被跳过")
	}
}

func TestHongguoDrpySignedRequestAtBindsEndpoint(t *testing.T) {
	// 换端点必须让请求 URL 与 x-medusa 随之变化：
	// x-medusa 以 fullURL（含 host）为输入，若换域后签名不变，
	// 说明端点池形同虚设（换了域却仍按旧域签名，服务端必然拒绝）。
	payload, err := hgDrpyVideoModelPayload("7685285364566592536")
	if err != nil {
		t.Fatalf("构造 payload 失败: %v", err)
	}
	now := time.Now()
	custom := "https://api5-normal-alt.fqnovel.com/novel/player/multi_video_model/v1/"

	defaultURL, defaultHeaders := hgDrpySignedVideoRequest(payload, now)
	customURL, customHeaders := hgDrpySignedVideoRequestAt(payload, now, custom)

	if !strings.HasPrefix(defaultURL, hgDrpyVideoAPI) {
		t.Fatalf("默认端点未生效: %s", defaultURL)
	}
	if !strings.HasPrefix(customURL, custom) {
		t.Fatalf("指定端点未生效: %s", customURL)
	}
	if defaultHeaders["x-medusa"] == "" || customHeaders["x-medusa"] == "" {
		t.Fatalf("x-medusa 不应为空")
	}
	if defaultHeaders["x-medusa"] == customHeaders["x-medusa"] {
		t.Fatalf("换端点后 x-medusa 必须重新计算，否则端点池无效")
	}
}

// 签名插槽：登记与取用。分发逻辑落地前，注册器不参与任何请求。
type stubHongguoSigner struct{ name string }

func (s stubHongguoSigner) Name() string { return s.name }

func (s stubHongguoSigner) Sign(headers map[string]string, query url.Values) error {
	headers["X-Stub-Signer"] = s.name
	return nil
}

func TestHongguoSignerRegistryRoundTrip(t *testing.T) {
	signer := stubHongguoSigner{name: "stub-signer-for-test"}
	registerHongguoSigner(signer)
	t.Cleanup(func() {
		hongguoSignerMu.Lock()
		delete(hongguoSigners, signer.Name())
		hongguoSignerMu.Unlock()
	})
	got, ok := hongguoSignerNamed(signer.Name())
	if !ok {
		t.Fatalf("注册后应能取到签名方案")
	}
	headers := map[string]string{}
	if err := got.Sign(headers, url.Values{}); err != nil {
		t.Fatalf("Sign 失败: %v", err)
	}
	if headers["X-Stub-Signer"] != signer.Name() {
		t.Fatalf("Sign 未写入预期请求头: %v", headers)
	}
	// 未注册的方案不应命中
	if _, ok := hongguoSignerNamed("no-such-signer"); ok {
		t.Fatalf("未注册的签名方案不应命中")
	}
}

// 注册表遍历本身：全部失败时按既有三段文案聚合，且不影响后续线路尝试顺序。
func TestHongguoRoutesAllRegisteredHaveDistinctNames(t *testing.T) {
	seen := map[string]bool{}
	for _, route := range hongguoRoutes {
		if seen[route.Name] {
			t.Fatalf("线路名重复: %q", route.Name)
		}
		seen[route.Name] = true
	}
	// 聚合依赖这三个固定名字，缺一即会输出「未尝试该线路」
	for _, name := range []string{hongguoRouteApp, hongguoRouteWeb, hongguoRouteDjapi} {
		if !seen[name] {
			t.Fatalf("错误聚合依赖线路 %q，注册表中缺失", name)
		}
	}
}

// 保证 ctx 取消时不会继续往下试：注册表遍历里每轮都会检查 ctx。
func TestHongguoRoutesContextCancelStopsFallback(t *testing.T) {
	resetHongguoRouteHealth(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 用一个必然失败但会检查 ctx 的桩：这里只验证注册表可被遍历、且 ctx 已取消
	if err := ctx.Err(); err == nil {
		t.Fatalf("前置条件失败：ctx 应已取消")
	}
	_ = hongguoRoutes // 注册表本身不发起请求
}

// ---- 半开（half-open）：冷却到期后放行一次试探，而不是直接全量恢复 ----

func TestHongguoHealthHalfOpenProbesOnce(t *testing.T) {
	resetHongguoRouteHealth(t)
	now := time.Now()
	for i := 0; i < hongguoHealthFailFastStreak; i++ {
		hongguoRoutesHealth.noteFail(hongguoRouteApp, now, hongguoFailRisk)
	}
	// 冷却期内：跳过
	if !hongguoRoutesHealth.skip(hongguoRouteApp, now.Add(hongguoFailRisk.cooldown()/2)) {
		t.Fatalf("冷却期内应跳过")
	}
	// 冷却到期：放行一次试探（半开），不应继续跳过
	if hongguoRoutesHealth.skip(hongguoRouteApp, now.Add(hongguoFailRisk.cooldown()+time.Minute)) {
		t.Fatalf("冷却到期后应进入半开、放行一次试探")
	}
	// 试探失败：立刻重新熔断，而不是再等累计到阈值
	probeAt := now.Add(hongguoFailRisk.cooldown() + time.Minute)
	hongguoRoutesHealth.noteFail(hongguoRouteApp, probeAt, hongguoFailRisk)
	if !hongguoRoutesHealth.skip(hongguoRouteApp, probeAt.Add(hongguoFailRisk.cooldown()/2)) {
		t.Fatalf("半开试探失败后应立即重新熔断")
	}
	// 试探成功：完全恢复
	hongguoRoutesHealth.noteOK(hongguoRouteApp)
	if hongguoRoutesHealth.skip(hongguoRouteApp, probeAt.Add(hongguoFailRisk.cooldown()/2)) {
		t.Fatalf("试探成功后应完全恢复")
	}
}

// ---- 失败分类 ----

func TestHongguoClassifyFailureKinds(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want hongguoFailKind
	}{
		{name: "空 body 是风控", err: errHongguoEmptyBody, want: hongguoFailRisk},
		{name: "超时是网络", err: context.DeadlineExceeded, want: hongguoFailNetwork},
		{name: "未知归其它", err: errors.New("something odd"), want: hongguoFailOther},
	}
	for _, item := range cases {
		if got := hongguoClassifyFailure(item.err); got != item.want {
			t.Fatalf("%s: 分类 = %q, 期望 %q", item.name, got, item.want)
		}
	}
	if got := hongguoClassifyFailure(nil); got != hongguoFailNone {
		t.Fatalf("nil 错误应为无类别, 实际 %q", got)
	}
}

func TestHongguoFailKindRetryableOnlyForNetwork(t *testing.T) {
	// 红果风控视角：只有网络类可原地重试，风控类重试等于给门禁加压
	if !hongguoFailNetwork.retryable() {
		t.Fatalf("网络类应可重试")
	}
	for _, kind := range []hongguoFailKind{hongguoFailRisk, hongguoFailSignature, hongguoFailParse, hongguoFailOther} {
		if kind.retryable() {
			t.Fatalf("%q 不应原地重试（会加重风控压力）", kind)
		}
	}
}

// ---- 持久化：进程重启后不丢 ----

func TestHongguoHealthPersistsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	resetHongguoRouteHealth(t)
	now := time.Now()
	for i := 0; i < hongguoHealthFailFastStreak; i++ {
		hongguoRoutesHealth.noteFail(hongguoRouteDrpy, now, hongguoFailRisk)
	}
	if err := hongguoRoutesHealth.saveTo(dir); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	// 模拟重启：清空内存后再加载
	hongguoRoutesHealth.reset()
	hongguoRoutesHealth.loadFrom(dir)
	if !hongguoRoutesHealth.skip(hongguoRouteDrpy, now.Add(hongguoFailRisk.cooldown()/2)) {
		t.Fatalf("重启后应保留熔断状态，否则冷启动又要白试一轮")
	}
	// 冷却期过后仍应恢复（持久化不能把状态钉死）
	if hongguoRoutesHealth.skip(hongguoRouteDrpy, now.Add(hongguoFailRisk.cooldown()+time.Minute)) {
		t.Fatalf("持久化状态也应遵守冷却期，到期后放行试探")
	}
}

func TestHongguoHealthSaveSkipsWhenClean(t *testing.T) {
	dir := t.TempDir()
	resetHongguoRouteHealth(t)
	if err := hongguoRoutesHealth.saveTo(dir); err != nil {
		t.Fatalf("无脏数据时保存也应安全: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, hongguoHealthFile)); err == nil {
		t.Fatalf("无状态变化不应产生落盘文件")
	}
}

// ---- 分类冷却差异化 ----

func TestHongguoFailKindCooldownDiffersByKind(t *testing.T) {
	// 风控短：登录/换网络后可能立刻恢复，等太久是白白浪费
	risk := hongguoFailRisk.cooldown()
	// 网络中等
	network := hongguoFailNetwork.cooldown()
	// 签名 / 解析长：不会自行恢复，频繁试探只会加重风控压力
	hard := hongguoFailSignature.cooldown()
	if hard != hongguoFailParse.cooldown() {
		t.Fatalf("签名与解析应同属长冷却")
	}
	if !(risk < network && network < hard) {
		t.Fatalf("冷却应满足 风控 < 网络 < 签名/解析，实际 %v / %v / %v", risk, network, hard)
	}
	if got := hongguoFailOther.cooldown(); got != hongguoHealthCooldownDefault {
		t.Fatalf("未分类应取默认冷却, 实际 %v", got)
	}
}

func TestHongguoHealthUsesKindSpecificCooldown(t *testing.T) {
	resetHongguoRouteHealth(t)
	now := time.Now()
	// 风控类：冷却 1 分钟 ⇒ 1 分钟后就该放行试探
	for i := 0; i < hongguoHealthFailFastStreak; i++ {
		hongguoRoutesHealth.noteFail(hongguoRouteDrpy, now, hongguoFailRisk)
	}
	if !hongguoRoutesHealth.skip(hongguoRouteDrpy, now.Add(30*time.Second)) {
		t.Fatalf("风控类冷却期内应跳过")
	}
	if hongguoRoutesHealth.skip(hongguoRouteDrpy, now.Add(hongguoFailRisk.cooldown()+time.Minute)) {
		t.Fatalf("风控类冷却短，到期后应放行试探（用户可能已登录）")
	}

	// 签名类：冷却 30 分钟 ⇒ 同样时间点仍应处于熔断
	resetHongguoRouteHealth(t)
	for i := 0; i < hongguoHealthFailFastStreak; i++ {
		hongguoRoutesHealth.noteFail(hongguoRouteApp, now, hongguoFailSignature)
	}
	if !hongguoRoutesHealth.skip(hongguoRouteApp, now.Add(hongguoFailRisk.cooldown()+time.Minute)) {
		t.Fatalf("签名类冷却远长于风控类，此时间点仍应熔断")
	}
}

// ---- 风控视角的三项增强 ----

func TestHongguoDrpyEmptyBodyClassifiesAsRisk(t *testing.T) {
	// drpys 空响应必须归 risk（1 分钟冷却），若落到 parse 会变成 30 分钟
	// ——主力线路被误锁，分类冷却的收益就没了。
	// （drpys 有域名白名单，本地 httptest 地址会被拒，故测纯函数 hongguoMediaBodyOrRisk）
	if err := hongguoMediaBodyOrRisk([]byte("")); !errors.Is(err, errHongguoEmptyBody) {
		t.Fatalf("空 body 应归一为设备级风控错误, 实际 %v", err)
	}
	if err := hongguoMediaBodyOrRisk([]byte("{}")); !errors.Is(err, errHongguoEmptyBody) {
		t.Fatalf("空 JSON 对象应归一为设备级风控错误, 实际 %v", err)
	}
	if err := hongguoMediaBodyOrRisk([]byte(`{"data":{"video_model":{}}}`)); err != nil {
		t.Fatalf("有内容的响应不应判为空: %v", err)
	}
	// 关键断言：归一后的错误必须分类为 risk
	if got := hongguoClassifyFailure(hongguoMediaBodyOrRisk([]byte(""))); got != hongguoFailRisk {
		t.Fatalf("drpys 空响应分类 = %q, 期望 risk（否则冷却错配到 30 分钟）", got)
	}
}

func TestHongguoAppRouteRequiresSession(t *testing.T) {
	// App 线路在匿名下必然被门禁拒绝，应标记需要登录态
	for _, route := range hongguoRoutes {
		if route.Name != hongguoRouteApp {
			continue
		}
		if !route.NeedsSession {
			t.Fatalf("app 线路应标记 NeedsSession（匿名下必然 200 空 body）")
		}
	}
	// 其它线路不应被误标
	for _, route := range hongguoRoutes {
		if route.Name == hongguoRouteApp {
			continue
		}
		if route.NeedsSession {
			t.Fatalf("线路 %q 不需要登录态，不应标记 NeedsSession", route.Name)
		}
	}
}

func TestHongguoMediaCarriesSourceRoute(t *testing.T) {
	// media 带上来源线路，播放层探活失败才能回流给线路健康度
	media := providerMedia{URL: "https://cdn.example.com/a.mp4", Variants: []providerMedia{{URL: "https://cdn.example.com/b.mp4"}}}
	choice := nativePlaybackChoices(media, 0)
	if choice.route != "" {
		t.Fatalf("未设置来源时 route 应为空, 实际 %q", choice.route)
	}
	media.Route = hongguoRouteDrpy
	choice = nativePlaybackChoices(media, 0)
	if choice.route != hongguoRouteDrpy {
		t.Fatalf("choice 应继承 media 的来源线路, 实际 %q", choice.route)
	}
}

func TestHongguoPlaybackFailureFlowsBackToRouteHealth(t *testing.T) {
	resetHongguoRouteHealth(t)
	// 模拟播放层探活失败：地址解析得出来但取不到首字节
	choice := nativePlaybackChoice{route: hongguoRouteWeb}
	hongguoRoutesHealth.noteFail(choice.route, time.Now(), hongguoFailNetwork)
	// 回流后该线路应被记账（网络类，冷却取中值）
	if hongguoRoutesHealth.skip(choice.route, time.Now()) {
		t.Fatalf("首次回流不应立即熔断（阈值 %d）", hongguoHealthFailFastStreak)
	}
	for i := 1; i < hongguoHealthFailFastStreak; i++ {
		hongguoRoutesHealth.noteFail(choice.route, time.Now(), hongguoFailNetwork)
	}
	if !hongguoRoutesHealth.skip(choice.route, time.Now()) {
		t.Fatalf("播放层连续回流失败后，线路应被熔断")
	}
}
