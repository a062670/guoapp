package core

// 红果取流健康度：线路级与端点级共用一套记账。
//
// 【管什么】两类对象，用复合键区分：
//   - 线路级：drpys / app / web / djapi（键 = 线路名）
//   - 端点级：线路@端点（键见 hongguoHostKey）
// 两者共用同一份状态表，因此下面所有机制（分类、熔断、半开、持久化）
// 对线路与端点**同时生效**，不存在「只做了一半」的情况。
//
// 【红果风控视角下的取舍——与通用 HA 的三点不同】
//
//  1. 绝不主动探测。通用 HA 会做 health check（定时 ping 节点确认存活），
//     这里**没有**：红果对匿名请求有设备级门禁（2026-10-15 起实测 200 空 body），
//     任何额外探测都在给风控加压。健康度只消费真实取流请求的结果。
//     这条沿用 lib/playback_route_memory.dart 与 app_preload.go 已确立的原则。
//
//  2. 失败要分类，且风控类**禁止原地重试**。通用 HA 主张「重试直到成功」，
//     但在红果这里，空响应意味着服务端已判定该设备/请求不可信，
//     原地重试只会加重风控。分类见 hongguoFailKind：只有网络类可重试。
//
//  3. 熔断必须能自动恢复，且恢复要**试探**。风控可能是暂时的（换网络、
//     换身份后恢复），因此冷却期结束后不是直接全量恢复，而是放行一次试探
//     （半开）：试探成功才完全恢复，失败则重新熔断。这样既不会永久排除已
//     恢复的对象，也不会在它仍不可用时白白连发多次请求。

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 健康度参数（保守默认值）。正常对象不会连续失败到阈值，
// 因此默认行为与「不做健康度」一致；只有真出问题时才生效。
const (
	// hongguoHealthEnabled 总开关。置 false 即退化为纯顺序回退（等同改造前）。
	hongguoHealthEnabled = true
	// hongguoHealthFailFastStreak 连续失败多少次后熔断。
	hongguoHealthFailFastStreak = 3
	// hongguoHealthCooldownDefault 未分类/其它类别的熔断时长。
	// 到期后进入半开（放行一次试探），而非直接全量恢复。
	hongguoHealthCooldownDefault = 5 * time.Minute

	// ---- 分类冷却：不同失败「自行恢复」的速度差了一个数量级，
	//      用同一个冷却值会同时犯两种错：该快的等太久、该慢的反复试 ----
	//
	// hongguoHealthCooldownRisk 风控类（200 空 body / 明确拒绝）。
	// 取**短**值：风控随登录态、网络出口、设备身份变化而解除——
	// 用户登录后往往立刻恢复，冷却太长就白白等掉这段时间。
	// 半开试探只在用户主动触发取流时才发生，不是额外探测，故短冷却安全。
	hongguoHealthCooldownRisk = 1 * time.Minute
	// hongguoHealthCooldownNetwork 网络类（超时/连接失败/DNS）。抖动通常较快自愈。
	hongguoHealthCooldownNetwork = 3 * time.Minute
	// hongguoHealthCooldownHard 签名被拒 / 解析失败。这两类不会自行恢复
	// （服务端不认这套签名，或响应结构已改版），频繁重试纯属浪费并加重风控压力，
	// 因此取**长**值。
	hongguoHealthCooldownHard = 30 * time.Minute
	// hongguoHealthFile 持久化文件名（落在 cfg.dataDir 下，0700 原子写）。
	hongguoHealthFile = "hongguo_route_health.json"
)

// hongguoFailKind 失败类别。
//
// 【为什么分类】不同失败的正确处置**相反**：网络抖动该重试，风控拒绝绝不能重试。
// 不分类就只能套用统一策略——要么重试风控请求（加重风控），
// 要么放弃本可重试的网络抖动（白白失败）。
type hongguoFailKind string

const (
	hongguoFailNone hongguoFailKind = ""
	// hongguoFailRisk 设备级风控：HTTP 200 空 body / 明确拒绝。
	// 处置：换线路或换端点，**绝不原地重试**。
	hongguoFailRisk hongguoFailKind = "risk"
	// hongguoFailNetwork 网络类：超时、连接失败、DNS。
	// 处置：可退避重试（同一对象）。
	hongguoFailNetwork hongguoFailKind = "network"
	// hongguoFailSignature 签名/鉴权被拒（如 403、签名校验失败）。
	// 处置：换线路（换端点无意义，签名绑定 URL，重试也一样被拒）。
	hongguoFailSignature hongguoFailKind = "signature"
	// hongguoFailParse 响应拿到但解析不出期望结构。
	// 处置：换线路（服务端可能改版）。
	hongguoFailParse hongguoFailKind = "parse"
	hongguoFailOther hongguoFailKind = "other"
)

// retryable 该类别是否允许**原地重试**（同一线路/端点再来一次）。
// 红果风控视角下只有网络类可以；风控类重试等于给门禁加压。
func (kind hongguoFailKind) retryable() bool {
	return kind == hongguoFailNetwork
}

// cooldown 该类别的熔断时长。
//
// 【为什么按类别区分】不同失败的「自行恢复速度」差一个数量级：
// 风控可能随登录/换网络立刻解除，而签名被拒或响应改版不会自己变好。
// 统一冷却会同时犯两种错——风控类等太久（用户明明已登录却还要等），
// 签名类反复试探（每次都必然失败，还多给服务端添压力）。
func (kind hongguoFailKind) cooldown() time.Duration {
	switch kind {
	case hongguoFailRisk:
		return hongguoHealthCooldownRisk
	case hongguoFailNetwork:
		return hongguoHealthCooldownNetwork
	case hongguoFailSignature, hongguoFailParse:
		return hongguoHealthCooldownHard
	}
	return hongguoHealthCooldownDefault
}

// errHongguoEmptyBody 红果返回 HTTP 200 但 body 为 0 字节——设备级风控的匿名拒绝信号。
// 各取流线路拿到 200 响应后应先用 hongguoMediaBodyOrRisk 归一到本错误，
// 否则空响应会被当成解析失败（"不会自愈"类）而误取长冷却。
var errHongguoEmptyBody = errors.New("红果接口返回空内容")

// hongguoMediaBodyOrRisk 校验取流响应体：内容为空即归一为设备级风控错误。
//
// 【调用点】各取流线路拿到 200 响应后先过这一层。app 线路早已有
// errHongguoEmptyBody，drpys 原先缺这一层——空 body 会掉进 JSON 解析失败，
// 被归成 parse（"不会自愈"，30 分钟冷却）而非 risk（1 分钟）。
//
// 【为什么抽成纯函数】drpys 有域名白名单（hgDrpyAPIDomains），
// 本地 httptest 地址会被拒绝，无法走网络路径验证；抽成纯函数后可脱离网络单测。
func hongguoMediaBodyOrRisk(payload []byte) error {
	if hongguoEmptyJSONBody(payload) {
		return errHongguoEmptyBody
	}
	return nil
}

// hongguoClassifyFailure 把取流错误归入失败类别。
//
// 判定顺序有讲究：先判最特异的（空 body 风控），再判网络（error 链），
// 最后才按文本兜底——文本匹配最不可靠，只用来兜住没有类型信息的错误。
func hongguoClassifyFailure(err error) hongguoFailKind {
	if err == nil {
		return hongguoFailNone
	}
	// 1) 设备级风控：空 body（app 线路已用 errHongguoEmptyBody 表达）
	if errors.Is(err, errHongguoEmptyBody) {
		return hongguoFailRisk
	}
	// 2) 网络类：超时 / 连接错误 / DNS
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return hongguoFailNetwork
	}
	var netError net.Error
	if errors.As(err, &netError) {
		return hongguoFailNetwork
	}
	// 3) 文本兜底
	message := err.Error()
	switch {
	case strings.Contains(message, "空内容") || strings.Contains(message, "空响应"):
		return hongguoFailRisk
	case strings.Contains(message, "context deadline exceeded") || strings.Contains(message, "timeout"):
		return hongguoFailNetwork
	case strings.Contains(message, "403") || strings.Contains(message, "签名"):
		return hongguoFailSignature
	case strings.Contains(message, "格式异常") || strings.Contains(message, "解析"):
		return hongguoFailParse
	}
	return hongguoFailOther
}

// hongguoHealthEntry 单个记账对象（线路或 线路@端点）的状态。
type hongguoHealthEntry struct {
	Fails     int            `json:"fails"`     // 连续失败次数
	LastFail  time.Time      `json:"lastFail"`  // 最近失败时间
	OpenUnitl time.Time      `json:"openUntil"` // 熔断截止（零值=未熔断）
	Probing   bool           `json:"probing"`   // 半开：已放行一次试探，等待结果
	LastKind  string         `json:"lastKind"`  // 触发熔断的类别（决定冷却时长）
	Kinds     map[string]int `json:"kinds"`     // 分类计数，供阈值调优与诊断
}

// tripped 是否处于熔断中（含「半开但试探尚未回来」之外的判定）。
func (entry *hongguoHealthEntry) tripped(now time.Time) bool {
	return !entry.OpenUnitl.IsZero() && now.Before(entry.OpenUnitl)
}

// hongguoRouteHealth 健康度总表：线路级与端点级共用。
//
// 【为什么是包级单例】「某个线路/端点现在通不通」是进程级的共同认知，
// 所有下载任务共享同一份判断才有意义；同时避免改动 Downloader 结构体。
type hongguoRouteHealth struct {
	mu       sync.Mutex
	loaded   bool
	dirty    bool
	entries  map[string]*hongguoHealthEntry
	onChange func() // 状态跃变时回调（用于触发持久化）
}

var hongguoRoutesHealth = &hongguoRouteHealth{entries: map[string]*hongguoHealthEntry{}}

func (h *hongguoRouteHealth) entry(name string) *hongguoHealthEntry {
	if h.entries == nil {
		h.entries = map[string]*hongguoHealthEntry{}
	}
	item, ok := h.entries[name]
	if !ok {
		item = &hongguoHealthEntry{Kinds: map[string]int{}}
		h.entries[name] = item
	}
	return item
}

// noteOK 成功：清空该对象的全部状态（连续计数、熔断、半开标记）。
func (h *hongguoRouteHealth) noteOK(name string) {
	h.mu.Lock()
	changed := h.noteOKLocked(name)
	h.mu.Unlock()
	if changed {
		h.notifyChange()
	}
}

func (h *hongguoRouteHealth) noteOKLocked(name string) bool {
	item, ok := h.entries[name]
	if !ok || (item.Fails == 0 && item.OpenUnitl.IsZero() && !item.Probing) {
		return false
	}
	delete(h.entries, name)
	h.dirty = true
	return true
}

// noteFail 失败：记录类别并计数；达到阈值或半开试探失败时熔断。
func (h *hongguoRouteHealth) noteFail(name string, at time.Time, kind hongguoFailKind) {
	h.mu.Lock()
	changed := h.noteFailLocked(name, at, kind)
	h.mu.Unlock()
	if changed {
		h.notifyChange()
	}
}

func (h *hongguoRouteHealth) noteFailLocked(name string, at time.Time, kind hongguoFailKind) bool {
	item := h.entry(name)
	if kind != hongguoFailNone {
		item.Kinds[string(kind)]++
	}
	item.Fails++
	item.LastFail = at
	if kind != hongguoFailNone {
		item.LastKind = string(kind)
	}
	h.dirty = true
	// 半开试探失败：立刻重新熔断（不再等累计到阈值）
	// 冷却已过但计数仍达阈值 = 正处于半开窗口
	if item.Fails >= hongguoHealthFailFastStreak || item.Probing {
		// 冷却时长按类别取：风控短（可能随登录立刻恢复）、签名/解析长（不会自愈）
		item.OpenUnitl = at.Add(kind.cooldown())
		item.Probing = false
		return true
	}
	return false
}

// skip 判断本次是否应跳过该对象。
//
// 状态机：closed（正常）→ 连续失败达阈值 → open（熔断，跳过）
//
//	→ 冷却到期 → half-open（放行一次试探）→ 成功回 closed / 失败回 open
func (h *hongguoRouteHealth) skip(name string, now time.Time) bool {
	if !hongguoHealthEnabled {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	item, ok := h.entries[name]
	if !ok {
		return false
	}
	if item.tripped(now) {
		return true
	}
	// 冷却已过但计数仍达阈值 → 半开：放行一次，并标记等待试探结果
	if item.Fails >= hongguoHealthFailFastStreak && !item.Probing {
		item.Probing = true
		h.dirty = true
	}
	return false
}

func (h *hongguoRouteHealth) notifyChange() {
	if h.onChange != nil {
		h.onChange()
	}
}

// reset 清空全部记账（仅供测试）。
func (h *hongguoRouteHealth) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = map[string]*hongguoHealthEntry{}
	h.loaded = false
	h.dirty = false
}

// ---- 持久化：进程重启后不丢，避免冷启动又白试一轮 ----

// hongguoHealthSnapshot 落盘结构。只存必要字段，损坏/越界一律忽略
// （与会话缓存同样的策略：宁可丢掉重来，也不能让坏数据卡死取流）。
type hongguoHealthSnapshot struct {
	Version int                            `json:"version"`
	Entries map[string]*hongguoHealthEntry `json:"entries"`
}

const hongguoHealthSnapshotVersion = 1

// loadFrom 从目录加载。幂等：只加载一次，重复调用无副作用。
func (h *hongguoRouteHealth) loadFrom(dir string) {
	if dir == "" {
		return
	}
	h.mu.Lock()
	if h.loaded {
		h.mu.Unlock()
		return
	}
	h.loaded = true
	h.mu.Unlock()

	body, err := os.ReadFile(filepath.Join(dir, hongguoHealthFile))
	if err != nil {
		return
	}
	var snapshot hongguoHealthSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil || snapshot.Version != hongguoHealthSnapshotVersion {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, item := range snapshot.Entries {
		if item == nil || name == "" {
			continue
		}
		if item.Kinds == nil {
			item.Kinds = map[string]int{}
		}
		h.entries[name] = item
	}
}

// saveTo 落盘（0700 原子写）。只在状态跃变后调用，不做定时轮询写盘。
func (h *hongguoRouteHealth) saveTo(dir string) error {
	if dir == "" {
		return nil
	}
	h.mu.Lock()
	if !h.dirty {
		h.mu.Unlock()
		return nil
	}
	snapshot := hongguoHealthSnapshot{Version: hongguoHealthSnapshotVersion, Entries: map[string]*hongguoHealthEntry{}}
	for name, item := range h.entries {
		clone := *item
		snapshot.Entries[name] = &clone
	}
	h.dirty = false
	h.mu.Unlock()

	body, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return writeNativeCacheFile(filepath.Join(dir, hongguoHealthFile), body)
}
