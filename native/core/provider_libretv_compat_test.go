package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 真实订阅的「不严格 JSON」兼容测试。
//
// 电视圈的订阅由人工维护，几乎不会是标准 JSON。这里用三份真实订阅的
// 形态做回归（样本取自 2026-10 抓取的高天流云 js.json / maoystv 001.json /
// 小盒子 4k.json），任何一条退化都会让整份订阅在 App 里导不进来。

func subFixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "subs", name))
	if err != nil {
		t.Fatalf("读取样本失败（%s）：%v", name, err)
	}
	return string(body)
}

// TestLibreTVSubscriptionToleratesComments 订阅带 // 与 /* */ 注释。
// maoystv 的 001.json 第 2 行就是 `//数据接口`——不处理会整份解析失败。
func TestLibreTVSubscriptionToleratesComments(t *testing.T) {
	body := subFixture(t, "maoystv_commented.json")
	parsed, err := parseLibreTVSubscription(body, "https://sub.example.com/tv.json")
	if err != nil {
		t.Fatalf("带注释的订阅应能解析：%v", err)
	}
	if len(parsed.Sites) != 2 {
		t.Fatalf("应解析出 2 个采集站，实际 %d：%+v", len(parsed.Sites), parsed.Sites)
	}
	for _, site := range parsed.Sites {
		if !strings.Contains(site.API, "provide/vod") {
			t.Fatalf("站点接口地址不对：%q", site.API)
		}
	}
}

// TestLibreTVSubscriptionToleratesRawNewlineInString 字符串字面量里有裸换行。
// 小盒子 4k.json 的 `"优\n酷"` 中间是真实换行符（作者手滑断行），
// Go 的 encoding/json 会直接报 invalid character '\n' in string literal。
func TestLibreTVSubscriptionToleratesRawNewlineInString(t *testing.T) {
	body := subFixture(t, "xiaohezi_raw_newline.json")
	// 先确认样本确实是「标准 JSON 解析不了」的形态。
	document := libretvExtractJSONObject(body)
	var strict struct {
		Sites []libretvConfigSite `json:"sites"`
	}
	if json.Unmarshal([]byte(document), &strict) == nil {
		t.Fatal("样本无效：标准 JSON 应解析失败，请检查 testdata")
	}
	parsed, err := parseLibreTVSubscription(body, "https://sub.example.com/tv.json")
	if err != nil {
		t.Fatalf("字符串内含裸换行的订阅应能解析：%v", err)
	}
	// 两条站点都要保留（断行那条不能被整份丢弃）。
	if len(parsed.Sites) != 2 {
		t.Fatalf("应解析出 2 个站点，实际 %d：%+v", len(parsed.Sites), parsed.Sites)
	}
	keys := map[string]bool{}
	for _, site := range parsed.Sites {
		keys[site.Key] = true
	}
	if !keys["k1"] || !keys["m3"] {
		t.Fatalf("断行那条站点应被保留，实际 keys=%v", keys)
	}
}

// TestLibreTVSubscriptionRelativeScriptPaths drpy 脚本用相对路径写法。
//
// 高天流云 js.json（298 站）的形态：api 是运行时库 `./lib/drpy2.min.js`，
// ext 才是站点脚本 `./js/drpy.js`。修这个之前，292 个站点脚本全被当成
// 运行时库去重掉，298 站的大仓只能识别出 6 个候选。
func TestLibreTVSubscriptionRelativeScriptPaths(t *testing.T) {
	body := subFixture(t, "gaotianliuyun_relative.json")
	baseURL := "https://cdn.example.com/gh/gao@master/js.json"
	parsed, err := parseLibreTVSubscription(body, baseURL)
	if err != nil {
		t.Fatalf("相对路径订阅应能解析：%v", err)
	}
	// 两条 ext 是相对脚本路径的站点要各自成为独立候选（按补全后地址去重）。
	if len(parsed.DrpyCandidates) != 3 {
		t.Fatalf("应有 3 个脚本候选（2 条 ext + 1 条 api 脚本），实际 %d：%+v",
			len(parsed.DrpyCandidates), parsed.DrpyCandidates)
	}
	want := map[string]bool{
		"https://cdn.example.com/gh/gao@master/js/drpy.js":            false,
		"https://cdn.example.com/gh/gao@master/js/cilixiong.js":       false,
		"https://cdn.example.com/gh/gao@master/lib/lf_search3_min.js": false,
	}
	for _, candidate := range parsed.DrpyCandidates {
		if _, ok := want[candidate.API]; !ok {
			t.Fatalf("候选地址不在预期内：%q", candidate.API)
		}
		want[candidate.API] = true
	}
	for address, seen := range want {
		if !seen {
			t.Fatalf("缺少候选：%s", address)
		}
	}
	// 相对路径补全出来的候选要打标记：UI 据此提示「聚合订阅常丢原始路径」。
	for _, candidate := range parsed.DrpyCandidates {
		if !candidate.Relative {
			t.Fatalf("由相对路径补全的候选应标记 Relative：%+v", candidate)
		}
	}
	// csp_Douban 不是脚本，应带原因跳过而不是静默丢失。
	if len(parsed.Skipped) == 0 || !strings.Contains(parsed.Skipped[0], "Douban") {
		t.Fatalf("csp_ 源应给出跳过原因：%v", parsed.Skipped)
	}
}

// TestLibretvLooksLikeScriptPath 相对脚本路径判定的边界。
func TestLibretvLooksLikeScriptPath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"./js/drpy.js", true},
		{"../lib/xx.js", true},
		{"js/xx.js", true},
		{"/lib/xx.js", true},
		{"./js/xx.js;md5", true},      // 带 ;md5 校验后缀（TVBox 约定）
		{"csp_Douban", false},         // 内置 Spider
		{"py_xxx", false},             // Python 源
		{"https://a.com/x.js", false}, // 绝对地址由 looksLikeDrpyScriptURL 处理
		{"./js/xx.json", false},       // 不是脚本
		{"has space.js", false},       // 含空格，不像路径
		{"", false},
	}
	for _, c := range cases {
		if got := libretvLooksLikeScriptPath(c.in); got != c.want {
			t.Errorf("libretvLooksLikeScriptPath(%q)=%v，期望 %v", c.in, got, c.want)
		}
	}
}

// TestLibreTVSubscriptionStringTypedFields 字段类型写成字符串（type: "1"）。
//
// 影视仓聚合订阅（518 站）的 parses 里就有 `"type": "1"`。修这个之前，
// 一条解析规则的字段类型不合规会让**整份订阅**解析失败——518 个站点全废。
func TestLibreTVSubscriptionStringTypedFields(t *testing.T) {
	body := subFixture(t, "ysc_string_typed.json")
	parsed, err := parseLibreTVSubscription(body, "https://sub.example.com/tv.json")
	if err != nil {
		t.Fatalf("字段写成字符串的订阅应能解析：%v", err)
	}
	if parsed.ParseBroken {
		t.Fatal("parses 段不应被判为损坏")
	}
	if len(parsed.Sites) != 2 {
		t.Fatalf("应解析出 2 个站点，实际 %d", len(parsed.Sites))
	}
	if len(parsed.Parses) != 3 {
		t.Fatalf("应解析出 3 条解析规则，实际 %d", len(parsed.Parses))
	}
	for _, parse := range parsed.Parses {
		if parse.URL == "" {
			t.Fatalf("解析规则地址丢失：%+v", parse)
		}
	}
	// type 写成 "1" 与 "0" 的都要正确取值。
	if got := int(parsed.Parses[1].Type); got != 1 {
		t.Errorf(`"type":"1" 应解析为 1，实际 %d`, got)
	}
	if got := int(parsed.Parses[2].Type); got != 0 {
		t.Errorf(`"type":"0" 应解析为 0，实际 %d`, got)
	}
}

// TestLibreTVSubscriptionParsesBrokenKeepsSites parses 段写坏时站点仍要导入。
// 两段分开解析，互不拖累。
func TestLibreTVSubscriptionParsesBrokenKeepsSites(t *testing.T) {
	// parses 写成对象而不是数组（真·无法解析）：站点必须照常导入。
	body := `{"sites":[{"key":"s1","name":"采集站一","type":1,"api":"https://api.example.com/provide/vod/"}],"parses":{"name":"x","url":"https://jx.example.com/?url="}}`
	parsed, err := parseLibreTVSubscription(body, "https://sub.example.com/tv.json")
	if err != nil {
		t.Fatalf("parses 写坏不应阻断站点导入：%v", err)
	}
	if len(parsed.Sites) != 1 {
		t.Fatalf("站点应保留，实际 %d", len(parsed.Sites))
	}
	if !parsed.ParseBroken {
		t.Fatal("应标记 parses 段损坏（导入结果里要提示用户）")
	}
	if len(parsed.Parses) != 0 {
		t.Fatalf("parses 段损坏时不应产出解析规则，实际 %d", len(parsed.Parses))
	}
}

// TestLibretvFlexInt 宽松整数字段的取值边界。
func TestLibretvFlexInt(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`1`, 1}, {`"1"`, 1}, {`"3"`, 3}, {`0`, 0}, {`"0"`, 0},
		{`null`, 0}, {`"abc"`, 0}, {`""`, 0}, {` 2 `, 2}, {`" 2 "`, 2},
	}
	for _, c := range cases {
		var v libretvFlexInt
		if err := json.Unmarshal([]byte(c.in), &v); err != nil {
			t.Errorf("Unmarshal(%s) 报错：%v", c.in, err)
			continue
		}
		if int(v) != c.want {
			t.Errorf("Unmarshal(%s)=%d，期望 %d", c.in, int(v), c.want)
		}
	}
}

// TestLibreTVSubscriptionAbsoluteScriptNotFlagged 绝对地址的脚本候选不打
// Relative 标记——只有「由相对路径补全而来」的才需要提示。
func TestLibreTVSubscriptionAbsoluteScriptNotFlagged(t *testing.T) {
	body := `{"sites":[{"key":"a","name":"脚本源","type":3,"api":"https://cdn.example.com/lib/drpy2.min.js","ext":"https://cdn.example.com/js/site.js"}]}`
	parsed, err := parseLibreTVSubscription(body, "https://sub.example.com/tv.json")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(parsed.DrpyCandidates) != 1 {
		t.Fatalf("应有 1 个候选，实际 %d", len(parsed.DrpyCandidates))
	}
	if parsed.DrpyCandidates[0].Relative {
		t.Fatal("绝对地址的候选不应标记 Relative")
	}
	if parsed.DrpyCandidates[0].API != "https://cdn.example.com/js/site.js" {
		t.Fatalf("候选地址不对：%q", parsed.DrpyCandidates[0].API)
	}
}
