package core

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// loadHgDrpyVectors 与对拍测试共用同一个向量集（解密/派生密钥的端到端用例复用其中的确定性样本）。

func TestHgDrpyVideoModelPayloadShape(t *testing.T) {
	payload, err := hgDrpyVideoModelPayload("123456")
	if err != nil {
		t.Fatalf("payload 构造失败: %v", err)
	}
	var decoded map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v", err)
	}
	bizParam, _ := decoded["biz_param"].(map[string]any)
	if bizParam == nil {
		t.Fatal("payload 缺少 biz_param")
	}
	if value, ok := bizParam["need_all_video_definition"].(bool); !ok || !value {
		t.Error("biz_param.need_all_video_definition 应为 true")
	}
	if value, ok := bizParam["video_platform"].(json.Number); !ok || value.String() != "1024" {
		t.Errorf("biz_param.video_platform = %v，期望 1024", bizParam["video_platform"])
	}
	mixed, _ := decoded["mixed_video_id_map"].(map[string]any)
	if mixed == nil {
		t.Fatal("payload 缺少 mixed_video_id_map")
	}
	list, _ := mixed["1004"].([]any)
	if len(list) != 1 || list[0] != "123456" {
		t.Errorf("mixed_video_id_map.1004 = %v，期望 [123456]", mixed["1004"])
	}
}

func TestHgDrpyParseFallback(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"字符串直传", "https://api.fqnovel.com/x", "https://api.fqnovel.com/x"},
		{"对象 fallback_api", map[string]any{"fallback_api": "https://api.fqnovel.com/y"}, "https://api.fqnovel.com/y"},
		{"空字符串", "", ""},
		{"nil", nil, ""},
		{"无 fallback_api 的对象", map[string]any{"other": 1}, ""},
		{"对象 fallback_api 为空", map[string]any{"fallback_api": ""}, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := hgDrpyParseFallback(testCase.value); got != testCase.want {
				t.Errorf("hgDrpyParseFallback = %q，期望 %q", got, testCase.want)
			}
		})
	}
}

func TestHgDrpyFallbackFromModel(t *testing.T) {
	t.Run("video_model 为对象", func(t *testing.T) {
		response := map[string]any{
			"data": map[string]any{
				"42": map[string]any{
					"video_model": map[string]any{"fallback_api": "https://api.fqnovel.com/a"},
				},
			},
		}
		got, err := hgDrpyFallbackFromModel(response, "42")
		if err != nil || got != "https://api.fqnovel.com/a" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("video_model 为 JSON 字符串", func(t *testing.T) {
		response := map[string]any{
			"data": map[string]any{
				"42": map[string]any{
					"video_model": `{"fallback_api":"https://api.fqnovel.com/b"}`,
				},
			},
		}
		got, err := hgDrpyFallbackFromModel(response, "42")
		if err != nil || got != "https://api.fqnovel.com/b" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("业务码非零", func(t *testing.T) {
		response := map[string]any{"Code": json.Number("1001")}
		if _, err := hgDrpyFallbackFromModel(response, "42"); err == nil {
			t.Fatal("业务码非零应报错")
		}
	})
	t.Run("缺少分集", func(t *testing.T) {
		response := map[string]any{"data": map[string]any{}}
		if _, err := hgDrpyFallbackFromModel(response, "42"); err == nil {
			t.Fatal("缺少分集应报错")
		}
	})
	t.Run("缺少 fallback_api", func(t *testing.T) {
		response := map[string]any{
			"data": map[string]any{"42": map[string]any{"video_model": map[string]any{}}},
		}
		if _, err := hgDrpyFallbackFromModel(response, "42"); err == nil {
			t.Fatal("缺少 fallback_api 应报错")
		}
	})
}

func TestHgDrpyVideoInfo(t *testing.T) {
	t.Run("正常", func(t *testing.T) {
		response := map[string]any{
			"video_info": map[string]any{"data": map[string]any{"video_list": map[string]any{}}},
		}
		info, err := hgDrpyVideoInfo(response)
		if err != nil || info == nil {
			t.Fatalf("info %v err %v", info, err)
		}
	})
	t.Run("外层业务码非零", func(t *testing.T) {
		response := map[string]any{"code": json.Number("7")}
		if _, err := hgDrpyVideoInfo(response); err == nil {
			t.Fatal("外层 code 非零应报错")
		}
	})
	t.Run("video_info 业务码非零", func(t *testing.T) {
		response := map[string]any{"video_info": map[string]any{"Code": json.Number("8")}}
		if _, err := hgDrpyVideoInfo(response); err == nil {
			t.Fatal("video_info Code 非零应报错")
		}
	})
	t.Run("缺少 data", func(t *testing.T) {
		response := map[string]any{"video_info": map[string]any{}}
		if _, err := hgDrpyVideoInfo(response); err == nil {
			t.Fatal("缺少 data 应报错")
		}
	})
}

func TestHgDrpyCleanURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{`https:\/\/x.fqnovelvod.com\/a\/b.m3u8?k=1`, "https://x.fqnovelvod.com/a/b.m3u8?k=1"},
		{`https://x.fqnovelvod.com/a?x=1\u0026y=2`, "https://x.fqnovelvod.com/a?x=1&y=2"},
		{"https://x.fqnovelvod.com/a?x=1&amp;y=2", "https://x.fqnovelvod.com/a?x=1&y=2"},
		{"  https://x.fqnovelvod.com/a  ", "https://x.fqnovelvod.com/a"},
	}
	for _, testCase := range cases {
		if got := hgDrpyCleanURL(testCase.in); got != testCase.want {
			t.Errorf("hgDrpyCleanURL(%q) = %q，期望 %q", testCase.in, got, testCase.want)
		}
	}
}

func TestHgDrpyCheckedURL(t *testing.T) {
	domains := []string{"qznovelvod.com", "fqnovelvod.com", "douyinvod.com"}
	allowed := []string{
		"https://v.qznovelvod.com/a.m3u8",
		"https://fqnovelvod.com/a.m3u8",
		"https://x.douyinvod.com/a.m3u8",
		"https://x.douyinvod.com:443/a.m3u8",
	}
	for _, value := range allowed {
		if _, err := hgDrpyCheckedURL(value, domains); err != nil {
			t.Errorf("hgDrpyCheckedURL(%q) 应通过: %v", value, err)
		}
	}
	rejected := []string{
		"http://v.qznovelvod.com/a.m3u8",            // 非 https
		"https://evil.com/a.m3u8",                   // 域名不在白名单
		"https://notqznovelvod.com.evil.com/a.m3u8", // 后缀伪装
		"https://v.qznovelvod.com:8443/a.m3u8",      // 端口非 443
		"https://user:pass@v.qznovelvod.com/a.m3u8", // 带凭据
		"not a url", // 格式无效
	}
	for _, value := range rejected {
		if _, err := hgDrpyCheckedURL(value, domains); err == nil {
			t.Errorf("hgDrpyCheckedURL(%q) 应被拒绝", value)
		}
	}
}

func TestHgDrpyDownloadQuality(t *testing.T) {
	for _, quality := range []string{"", "auto", "1080p", "720p"} {
		if _, err := hgDrpyNormalizeDownloadQuality(quality); err != nil {
			t.Errorf("hgDrpyNormalizeDownloadQuality(%q) 应通过: %v", quality, err)
		}
	}
	if _, err := hgDrpyNormalizeDownloadQuality("480p"); err == nil {
		t.Error("480p 应被拒绝")
	}
	cases := []struct {
		width, height int
		quality       string
		want          bool
	}{
		{1920, 1080, "auto", true},
		{720, 1280, "auto", true},
		{0, 0, "1080p", false},
		{1280, 720, "1080p", false},
		{1920, 1080, "1080p", true},
		{720, 1280, "720p", true},
		{640, 480, "720p", false},
	}
	for _, testCase := range cases {
		if got := hgDrpyMeetsDownloadQuality(testCase.width, testCase.height, testCase.quality); got != testCase.want {
			t.Errorf("hgDrpyMeetsDownloadQuality(%d,%d,%q) = %v，期望 %v",
				testCase.width, testCase.height, testCase.quality, got, testCase.want)
		}
	}
}

// hgDrpyMediaListItem 构造 video_list 的一行。
func hgDrpyMediaListItem(codec string, width, height int, quality, mainURL, spadeA string) map[string]any {
	item := map[string]any{
		"codec_type": codec,
		"vwidth":     json.Number(itoa64(int64(width))),
		"vheight":    json.Number(itoa64(int64(height))),
		"main_url":   mainURL,
	}
	if quality != "" {
		item["quality_desc"] = quality
	}
	if spadeA != "" {
		item["spade_a"] = spadeA
	}
	return item
}

func TestHgDrpySelectMediaAutoPrefersH264ThenPixels(t *testing.T) {
	info := map[string]any{
		"video_list": map[string]any{
			"hevc720":  hgDrpyMediaListItem("hevc", 1280, 720, "720p", "https://a.fqnovelvod.com/hevc720.m3u8", ""),
			"h264720":  hgDrpyMediaListItem("h264", 1280, 720, "720p", "https://a.fqnovelvod.com/h264720.m3u8", ""),
			"h2641080": hgDrpyMediaListItem("h264", 1920, 1080, "1080p", "https://a.fqnovelvod.com/h2641080.m3u8", ""),
			"unknown":  hgDrpyMediaListItem("bytevc2", 1920, 1080, "1080p", "https://a.fqnovelvod.com/unknown.m3u8", ""),
		},
	}
	media, err := hgDrpySelectMedia(info, "42", "auto")
	if err != nil {
		t.Fatalf("hgDrpySelectMedia 失败: %v", err)
	}
	// auto：codecRank 0 的 h264 优先，同编码像素高者优先 → h2641080 首选。
	if media.URL != "https://a.fqnovelvod.com/h2641080.m3u8" {
		t.Errorf("首选 = %s，期望 h2641080", media.URL)
	}
	if media.Quality != 1080 {
		t.Errorf("Quality = %d，期望 1080", media.Quality)
	}
	if media.Referer != hgDrpyMediaReferer {
		t.Errorf("Referer = %s，期望 %s", media.Referer, hgDrpyMediaReferer)
	}
	if len(media.Variants) != 3 {
		t.Fatalf("variants = %d，期望 3（bytevc2 被过滤）", len(media.Variants))
	}
	if media.Variants[0].URL != "https://a.fqnovelvod.com/h2641080.m3u8" ||
		media.Variants[1].URL != "https://a.fqnovelvod.com/h264720.m3u8" ||
		media.Variants[2].URL != "https://a.fqnovelvod.com/hevc720.m3u8" {
		t.Errorf("variants 顺序不符: %s / %s / %s",
			media.Variants[0].URL, media.Variants[1].URL, media.Variants[2].URL)
	}
}

func TestHgDrpySelectMediaRequestedQualityFiltersByShortSide(t *testing.T) {
	info := map[string]any{
		"video_list": map[string]any{
			"1080": hgDrpyMediaListItem("h264", 1920, 1080, "1080p", "https://a.fqnovelvod.com/1080.m3u8", ""),
			"720":  hgDrpyMediaListItem("h264", 1280, 720, "720p", "https://a.fqnovelvod.com/720.m3u8", ""),
			"480":  hgDrpyMediaListItem("h264", 854, 480, "480p", "https://a.fqnovelvod.com/480.m3u8", ""),
			"noDim": map[string]any{
				"codec_type": "h264",
				"main_url":   "https://a.fqnovelvod.com/nodim.m3u8",
			},
		},
	}
	media, err := hgDrpySelectMedia(info, "42", "720p")
	if err != nil {
		t.Fatalf("hgDrpySelectMedia 失败: %v", err)
	}
	// 720p 档：短边 >=720 的只有 1080/720 两行；无尺寸行不过。
	if media.URL != "https://a.fqnovelvod.com/720.m3u8" {
		t.Errorf("首选 = %s，期望 720（短边升序）", media.URL)
	}
	if media.Quality != 720 {
		t.Errorf("Quality = %d，期望 720", media.Quality)
	}
	if len(media.Variants) != 2 {
		t.Fatalf("variants = %d，期望 2", len(media.Variants))
	}
	if _, err := hgDrpySelectMedia(info, "42", "1080p"); err != nil {
		t.Fatalf("1080p 应可用: %v", err)
	}
}

func TestHgDrpySelectMediaRejectsNonWhitelistedDomain(t *testing.T) {
	info := map[string]any{
		"video_list": map[string]any{
			"bad": hgDrpyMediaListItem("h264", 1920, 1080, "1080p", "https://evil.example.com/a.m3u8", ""),
		},
	}
	if _, err := hgDrpySelectMedia(info, "42", "auto"); err == nil {
		t.Fatal("非白名单域名应报错")
	}
}

func TestHgDrpySelectMediaDeduplicatesIdentity(t *testing.T) {
	info := map[string]any{
		"video_list": map[string]any{
			"a": hgDrpyMediaListItem("h264", 1920, 1080, "1080p", "https://a.fqnovelvod.com/same.m3u8", ""),
			"b": hgDrpyMediaListItem("avc", 1920, 1080, "1080p", "https://a.fqnovelvod.com/same.m3u8", ""),
		},
	}
	media, err := hgDrpySelectMedia(info, "42", "auto")
	if err != nil {
		t.Fatalf("hgDrpySelectMedia 失败: %v", err)
	}
	if len(media.Variants) != 1 {
		t.Fatalf("variants = %d，期望 1（同 URL 去重）", len(media.Variants))
	}
}

// TestHgDrpySelectMediaSpadeEndToEnd 端到端验证 key_seed 解密 + spade_a 派生 CENC key：
// 直接复用对拍向量里 decryptSpadeUrl / deriveContentKey 的确定性样本。
func TestHgDrpySelectMediaSpadeEndToEnd(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	if len(vectors.DecryptSpadeURL) == 0 || len(vectors.DeriveContentKey) == 0 {
		t.Fatal("对拍向量缺少 decryptSpadeUrl/deriveContentKey 样本")
	}
	spadeCase := vectors.DecryptSpadeURL[0]
	seed, err := hex.DecodeString(spadeCase.Seed)
	if err != nil {
		t.Fatalf("seed hex 无效: %v", err)
	}
	contentKeyCase := vectors.DeriveContentKey[len(vectors.DeriveContentKey)-1]
	if contentKeyCase.Output == nil || *contentKeyCase.Output == "" {
		t.Fatal("deriveContentKey 样本缺少输出")
	}
	wantContentKey := *contentKeyCase.Output
	info := map[string]any{
		"key_seed": base64.StdEncoding.EncodeToString(seed),
		"video_list": map[string]any{
			"1080p": hgDrpyMediaListItem("h264", 1920, 1080, "1080p", spadeCase.Input, contentKeyCase.Input),
		},
	}
	media, err := hgDrpySelectMedia(info, "42", "auto")
	if err != nil {
		t.Fatalf("hgDrpySelectMedia 失败: %v", err)
	}
	if media.URL != spadeCase.Output {
		t.Errorf("URL = %s，期望 %s", media.URL, spadeCase.Output)
	}
	if got := hex.EncodeToString(media.CENCKey); got != wantContentKey {
		t.Errorf("CENCKey = %s，期望 %s", got, wantContentKey)
	}
	if len(media.Variants) != 1 {
		t.Fatalf("variants = %d，期望 1", len(media.Variants))
	}
}

func TestHgDrpySelectMediaEmptyVideoList(t *testing.T) {
	if _, err := hgDrpySelectMedia(map[string]any{"video_list": map[string]any{}}, "42", "auto"); err == nil {
		t.Fatal("空 video_list 应报错")
	}
	if _, err := hgDrpySelectMedia(map[string]any{}, "42", "auto"); err == nil {
		t.Fatal("缺 video_list 应报错")
	}
}
