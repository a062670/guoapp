package core

// 红果取流线路注册表 + 线路健康度。
//
// 【背景：为什么抽出注册表】
// 改造前 resolveHongguoMedia 是硬编码的四段串行：
//   drpys 两段式 → App（video_model 单段）→ 网页播放页 → djapi 备用
// 顺序写死在函数体里，新增一条线路要改核心函数，签名也无法按线路插拔。
// 本文件把它拆成两层：
//   - 注册表（hongguoRoutes）：线路声明成表项，新增线路 = 加一行
//   - 健康度（hongguoRoutesHealth）：消费既有取流请求的成败，
//     连续失败到阈值后在冷却期内跳过该线路
//
// 【设计约束：绝不额外发起探测请求】
// 这一条沿用 lib/playback_route_memory.dart 与 app_preload.go 已确立的原则
// （注释原文：「完全不产生请求」「对风控零额外压力」「切线路本身是本地操作」）。
// 红果自 2026-10-15 起对匿名请求施加设备级门禁（实测 HTTP 200 + 空 body），
// 任何额外探测都会加重风控压力，因此健康度**只记录**既有请求的结果，
// 不主动 ping 端点、不做预热探测、不为了「确认恢复」而发试探请求。
//
// 【与「播放线路」是两层，不要混淆】
//   - 本文件的「线路」= 取流方式（用什么办法拿到 media 列表）
//   - app_playback_routes.go 的 routeIndex = 解析后 candidate media 数组的索引
//     （同一集的多个候选地址），由预加载探活 + PlaybackRouteMemory 选择
// 两者是上下游：本文件产出 media 列表 → 那边从中挑一个播放。
// 那边已有自己的记忆与 TTL（15 分钟），本文件不重复实现、也不改动它。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
)

// 线路标识：用于健康度记账与错误聚合。
const (
	hongguoRouteDrpy = "drpys" // drpys 六神签名 + 两段式取址（multi_video_model + fallback）
	hongguoRouteApp  = "app"   // App video_model 单段
	hongguoRouteWeb  = "web"   // 网页播放页解析
	// hongguoRouteDjapi 第三方备用接口。
	// 注：provider_hongguo_drpy_media.go 的注释记载该上游「已断」，
	// 保留在末位仅作兜底，不因它失败而影响其它线路的记账。
	hongguoRouteDjapi = "djapi"
)

// hongguoRoute 一条取流线路。
type hongguoRoute struct {
	// Name 线路标识，用于健康度记账与错误聚合。
	Name string
	// Signer 该线路期望使用的签名方案名（插槽位，见 hongguoSigner 说明）。
	// 在分发逻辑落地前，本字段仅作登记用途，不改变任何请求行为。
	Signer string
	// NeedsSession 该线路是否必须有登录态才可能成功。
	//
	// 【风控视角特有】红果自 2026-10-15 起对匿名请求施加设备级门禁：
	// App 线路（video_model）在匿名下**必然**返回 200 空 body。
	// 未登录时去试它，等于白花一次请求 + 拖慢到达网页线路的时间；
	// 更糟的是它会被记一次 risk 失败，把「没登录」误记成「线路坏了」。
	// 因此未登录时跳过，并把原因写进错误聚合，而不是假装它失败过。
	NeedsSession bool
	// Resolve 执行取流。统一签名为 (seriesID, videoID)，
	// 各线路实际用不到的参数在下面适配时忽略。
	Resolve func(ctx context.Context, d *Downloader, seriesID, videoID string) (providerMedia, error)
}

// hongguoRoutes 线路注册表。**顺序即默认优先级**，与改造前的硬编码顺序一致：
// drpys → app → web → djapi。新增线路在此追加一行即可，无需改 resolveHongguoMedia。
//
// 每条线路用闭包适配到统一签名：
//   - drpys 只需要 videoID（清晰度固定 "auto"，由上层再由 Variants 过滤）
//   - app 只需要 videoID
//   - web / djapi 需要 seriesID + videoID
var hongguoRoutes = []hongguoRoute{
	{
		Name:   hongguoRouteDrpy,
		Signer: "drpys-sixhead",
		Resolve: func(ctx context.Context, d *Downloader, seriesID, videoID string) (providerMedia, error) {
			return d.resolveHongguoDrpyMedia(ctx, videoID, "auto")
		},
	},
	{
		Name:         hongguoRouteApp,
		Signer:       "app-sixhead",
		NeedsSession: true,
		Resolve: func(ctx context.Context, d *Downloader, seriesID, videoID string) (providerMedia, error) {
			return d.resolveHongguoAppMedia(ctx, videoID)
		},
	},
	{
		Name:   hongguoRouteWeb,
		Signer: "none",
		Resolve: func(ctx context.Context, d *Downloader, seriesID, videoID string) (providerMedia, error) {
			return d.resolveHongguoWebMedia(ctx, seriesID, videoID)
		},
	},
	{
		Name:   hongguoRouteDjapi,
		Signer: "none",
		Resolve: func(ctx context.Context, d *Downloader, seriesID, videoID string) (providerMedia, error) {
			return d.resolveHongguoPlaybackAPI(ctx, seriesID, videoID)
		},
	},
}

// hongguoSigner 签名方案的接口（插槽位）。
//
// 【当前状态】签名栈仍由 provider_hongguo_sign.go 的 const 开关直接驱动
// （hongguoUseRealGorgon / hongguoUseRealHelios / hongguoUseRealMedusa
// → xgorgonWith / HeliosFor / BuildMedusaFor），本接口暂不参与请求分发。
//
// 【如何启用】为「把签名也做成可注册项」预留的落地步骤：
//  1. 实现本接口（例如 drpys 那套、App 那套、未来新增的第 N 套签名）
//  2. 在包 init 或首次使用时调用 registerHongguoSigner 注册
//  3. 把 Name() 的返回值填进 route.Signer，再按 route.Signer 取用即可按线路分发
//
// 在分发逻辑落地前，注册器只是空转的目录，对任何现有请求零影响。
type hongguoSigner interface {
	// Name 签名方案名，对应 hongguoRoute.Signer。
	Name() string
	// Sign 就地写入签名结果（请求头 / 查询串）。
	Sign(headers map[string]string, query url.Values) error
}

var (
	hongguoSignerMu sync.RWMutex
	hongguoSigners  = map[string]hongguoSigner{}
)

// registerHongguoSigner 登记一套签名方案。同名覆盖，便于测试替换。
func registerHongguoSigner(signer hongguoSigner) {
	if signer == nil {
		return
	}
	hongguoSignerMu.Lock()
	defer hongguoSignerMu.Unlock()
	hongguoSigners[signer.Name()] = signer
}

// hongguoSignerNamed 取指定名称的签名方案。
func hongguoSignerNamed(name string) (hongguoSigner, bool) {
	hongguoSignerMu.RLock()
	defer hongguoSignerMu.RUnlock()
	signer, ok := hongguoSigners[name]
	return signer, ok
}

// 端点池：每条取流线路的候选 API 端点。
//
// 【为什么默认各只有一个】红果存在多个 API 域，但我们**并未实测确认**
// 其它域接受同一套签名与路径。盲开等于把「已验证可用的单域」换成
// 「可能不通的多域」，风险大于收益。因此默认列表保持现网唯一实测可用的
// 端点（行为与改造前 100% 一致），其余域待实测通过后再追加。
//
// 【扩展方式】验证某个域可用后，直接往对应列表追加一项即可，无需改调用逻辑：
//   - drpys：完整端点（含 path），因为签名以 fullURL 为输入
//   - app：base（不含 path），请求时再拼 path
//
// 追加后自动获得「按序尝试 + 失败记账 + 冷却跳过」的全部行为。
var (
	// hongguoDrpyHosts drpys 第一段（multi_video_model）的候选端点（完整 URL，含 path）。
	hongguoDrpyHosts = []string{hgDrpyVideoAPI}
	// hongguoAppHosts App video_model 的额外候选端点（暂未启用：
	// App 请求尚未做 host 参数化，待具备后再开启；当前为空即行为与改造前一致）。
	hongguoAppHosts []string
)

// hongguoHostKey 端点级记账的键：线路@端点。
//
// 【为什么是复合键】端点健康度必须按「线路 × 端点」分别记账：
// 同一端点在不同线路上的可用性相互独立（例如某域接受 multi_video_model
// 但拒绝 video_model），用复合键可直接复用线路级那套 noteOK / noteFail /
// skip，不必再写一套并行的健康度实现。
func hongguoHostKey(route, host string) string {
	return route + "@" + host
}

// 线路未真正执行时的占位错误：用于错误聚合时给出可读说明，
// 避免 publicError(nil) 输出 <nil> 而让日志失去意义。
var (
	// errHongguoRouteSkipped 该线路近期连续失败，命中冷却期被跳过。
	errHongguoRouteSkipped = errors.New("该线路近期连续失败，冷却期内暂不重试")
	// errHongguoRouteNeedsLogin 该线路必须有登录态，而当前未登录。
	// 与「线路坏了」区分开：不计入健康度，避免把缺登录误记成线路故障。
	errHongguoRouteNeedsLogin = errors.New("该线路需要红果登录态，当前未登录")
	// errHongguoRouteEmptyMedia 线路返回成功但没给播放地址。
	// 改造前这种情况会被当成成功返回，导致上层拿着空 media 去下载；
	// 这里一律按失败处理，继续回退到下一条线路。
	errHongguoRouteEmptyMedia = errors.New("红果取流未返回播放地址")
)

// routeErrorOrSkip 把「未执行」的占位错误补成可读文案。
func routeErrorOrSkip(err error) error {
	if err == nil {
		return errors.New("未尝试该线路")
	}
	return err
}

// hongguoEmptyResponse 判定一次红果响应是否为「空响应」。
//
// 【为什么需要】红果自 2026-10-15 起对匿名请求施加设备级门禁：
// HTTP 200 但 body 为空 / data 为空对象。这类响应在 Go 侧不会产生 error，
// 若不判定就会被当成「接口通了但没数据」，掩盖真正的风控原因，
// 也让上层误以为该线路可用而反复重试。
//
// 【当前接入范围】本函数是工具函数，注册表层用它做结果兜底判定；
// 各线路内部若要精确区分「空响应」与「解析失败」，可在拿到 body 后调用它，
// 把返回值转成更明确的错误（如「红果 App 接口返回空内容（疑似设备级风控）」）。
// 改造未改动四个 resolve 函数内部，因此 body 级判定尚未在各线路内接入——
// 空 body 目前仍会以解析错误的形式冒泡，但注册表层会正确记账并在阈值后跳过。
func hongguoEmptyResponse(body []byte, result map[string]any) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	if len(result) == 0 {
		return true
	}
	// 只有 data 且 data 为空 → 视为空响应（门禁的典型形态）
	if data, ok := result["data"]; ok {
		switch value := data.(type) {
		case nil:
			return true
		case map[string]any:
			return len(value) == 0
		case []any:
			return len(value) == 0
		case string:
			return len(bytes.TrimSpace([]byte(value))) == 0
		}
	}
	// 空 JSON 对象/数组
	trimmed := bytes.TrimSpace(body)
	if bytes.Equal(trimmed, []byte("{}")) || bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	return false
}

// hongguoEmptyJSONBody 是 hongguoEmptyResponse 的轻量入口：
// 只有原始 body、没有解析结果时，直接用它判定。
func hongguoEmptyJSONBody(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return true
	}
	var probe any
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		// 非 JSON（例如 HTML 错误页）交给调用方按现有逻辑处理，这里不算空。
		return false
	}
	switch value := probe.(type) {
	case nil:
		return true
	case map[string]any:
		return len(value) == 0
	case []any:
		return len(value) == 0
	case string:
		return len(value) == 0
	}
	return false
}
