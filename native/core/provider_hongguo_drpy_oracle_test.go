package core

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// fixedHgDrpyNow 返回与 oracle 向量（khronos=1758000000）一致的时间基准。
func fixedHgDrpyNow(vectors hgDrpyVectorSet) time.Time {
	khronos := int64(1758000000)
	if len(vectors.HashF13) > 0 {
		khronos = vectors.HashF13[0].Khronos
	}
	return time.Unix(khronos, 0)
}

// md5Sum 是包内 md5 摘要简写。
func md5Sum(value []byte) [16]byte {
	return md5.Sum(value)
}

// drpys 6 神签名移植（路线一）的逐字节对拍测试。
//
// 测试向量由 Node 运行 drpys 原版算法生成（mock 随机源与进程时钟后完全确定），
// 存放于 testdata/hongguo_drpy_vectors.json。任一函数输出不一致即移植失败。

type hgDrpyVectorSet struct {
	SM3 []struct {
		Input  string `json:"input"`
		Output string `json:"output"`
	} `json:"sm3"`
	SumMd5  uint32 `json:"sumMd5"`
	GetIv   uint32 `json:"getIv"`
	Md5SumV string `json:"md5SumV3"`
	HashF13 []struct {
		Query   string `json:"query"`
		Body    string `json:"body"`
		Khronos int64  `json:"khronos"`
		Branch  int    `json:"branch"`
		Output  string `json:"output"`
	} `json:"hashF13"`
	Rc4Gorgon    string   `json:"rc4Gorgon"`
	XGorgon      string   `json:"xGorgon"`
	Helios       string   `json:"helios"`
	HeliosRandom uint32   `json:"heliosRandom"`
	HeliosStable bool     `json:"heliosStable"`
	Proto        string   `json:"proto"`
	DeviceProto  string   `json:"deviceProto"`
	Xmxor        string   `json:"xmxor"`
	KeyHash      []string `json:"keyHash"`
	AesV3        []struct {
		Khronos int64  `json:"khronos"`
		Output  string `json:"output"`
	} `json:"aesV3"`
	BuildMedusa    string `json:"buildMedusa"`
	BuildMedusaLen int    `json:"buildMedusaLen"`
	ProcessNonce   string `json:"processNonce"`
	ProcessStarted int64  `json:"processStarted"`
	MockU32        uint32 `json:"mockU32"`
	MedusaRands    struct {
		MessageRand uint32 `json:"messageRand"`
		EnvLaunch   uint32 `json:"envLaunch"`
		EnvPid      uint32 `json:"envPid"`
		KeyHashRand uint32 `json:"keyHashRand"`
		XmRand      uint32 `json:"xmRand"`
	} `json:"medusaRands"`
	BranchOf []struct {
		Query   string `json:"query"`
		Khronos int64  `json:"khronos"`
		Branch  int    `json:"branch"`
	} `json:"branchOf"`
	DeriveContentKey []struct {
		Input       string  `json:"input"`
		Output      *string `json:"output"`
		Error       *string `json:"error"`
		Constructed bool    `json:"constructed"`
	} `json:"deriveContentKey"`
	DecryptSpadeURL []struct {
		Seed   string `json:"seed"`
		Input  string `json:"input"`
		Output string `json:"output"`
	} `json:"decryptSpadeUrl"`
}

func loadHgDrpyVectors(t *testing.T) hgDrpyVectorSet {
	t.Helper()
	content, err := os.ReadFile("testdata/hongguo_drpy_vectors.json")
	if err != nil {
		t.Fatalf("读取 drpys 对拍向量失败: %v", err)
	}
	var vectors hgDrpyVectorSet
	if err := json.Unmarshal(content, &vectors); err != nil {
		t.Fatalf("解析 drpys 对拍向量失败: %v", err)
	}
	return vectors
}

func TestHgDrpySM3MatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	for _, item := range vectors.SM3 {
		digest := hgDrpySM3([]byte(item.Input))
		if got := hex.EncodeToString(digest[:]); got != item.Output {
			t.Fatalf("SM3(%q) = %s，期望 %s", item.Input, got, item.Output)
		}
	}
}

func TestHgDrpySumMd5AndGetIvMatchOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	data, err := hex.DecodeString("0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	if got := hgDrpySumMd5(data); got != vectors.SumMd5 {
		t.Fatalf("sumMd5 = %d，期望 %d", got, vectors.SumMd5)
	}
	if got := hgDrpyGetIv(0x20230928, []byte("getiv-vector-data")); got != vectors.GetIv {
		t.Fatalf("getIv = %d，期望 %d", got, vectors.GetIv)
	}
}

func TestHgDrpyMd5SumV3MatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	message := make([]byte, 64)
	for index := range message {
		message[index] = 0x5a
	}
	orders := hgDrpyLoadBranch2Orders()
	digest := hgDrpyMd5SumV3(message, 0x1234, orders[:64], 0x56)
	if got := hex.EncodeToString(digest[:]); got != vectors.Md5SumV {
		t.Fatalf("md5SumV3 = %s，期望 %s", got, vectors.Md5SumV)
	}
}

func TestHgDrpyHashF13MatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	seen := map[int]int{}
	for _, item := range vectors.HashF13 {
		querySm3 := hgDrpySM3([]byte(item.Query))
		var body []byte
		if item.Body != "" {
			body, _ = hex.DecodeString(item.Body)
		}
		bodyMd5 := make([]byte, 16)
		if len(body) > 0 {
			sum := md5Sum(body)
			copy(bodyMd5, sum[:])
		}
		stamp := hgDrpyLe32(uint32(item.Khronos))
		digest := hgDrpyHashF13(querySm3[:], bodyMd5, stamp[:], uint32(item.Khronos))
		got := hex.EncodeToString(digest)
		if got != item.Output {
			t.Fatalf("hashF13(query=%q, khronos=%d) = %s，期望 %s", item.Query, item.Khronos, got, item.Output)
		}
		seen[item.Branch]++
	}
	// 三个分支都必须有覆盖，否则该测试不完整。
	for branch := 0; branch <= 2; branch++ {
		if seen[branch] == 0 {
			t.Fatalf("hashF13 对拍缺少 branch %d 的用例", branch)
		}
	}
}

func TestHgDrpyBranchOfMatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	for _, item := range vectors.BranchOf {
		url := hgDrpyVideoAPI + "?" + item.Query
		// oracle 的 body 规则：khronos 为奇数时带 "x"，否则为空。
		var body []byte
		if item.Khronos%2 == 1 {
			body = []byte("x")
		}
		if got := hgDrpyBranchOf(url, body, uint32(item.Khronos)); got != item.Branch {
			t.Fatalf("branchOf(%q) = %d，期望 %d", item.Query, got, item.Branch)
		}
	}
}

func TestHgDrpyXGorgonMatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	got := hgDrpyXGorgon("device_id=1000000000000001&ts=1758000000", []byte(`{"biz_param":{}}`), 1758000000, 0x9d2c)
	if got != vectors.XGorgon {
		t.Fatalf("xGorgon = %s，期望 %s", got, vectors.XGorgon)
	}
}

func TestHgDrpyRc4GorgonMatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	got := hgDrpyRc4Gorgon([]byte("rc4-vector-0123456789"), []byte{0x4a, 0x40, 0x16, 0x9d, 0x47, 0x6c, 0x01, 0x2c})
	if want := vectors.Rc4Gorgon; hex.EncodeToString(got) != want {
		t.Fatalf("rc4Gorgon = %s，期望 %s", hex.EncodeToString(got), want)
	}
}

func TestHgDrpyHeliosMatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	got := hgDrpyHelios(1758000000, vectors.HeliosRandom)
	if got != vectors.Helios {
		t.Fatalf("helios = %s，期望 %s", got, vectors.Helios)
	}
	if !vectors.HeliosStable {
		t.Fatal("oracle 自检失败：drpys helios 对固定输入不稳定")
	}
}

func TestHgDrpyProtoMatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	nested := hgDrpyProto([]hgDrpyProtoField{
		{Tag: 1, Value: int64(42), Type: "sint"},
		{Tag: 2, Value: "nested", Type: "string"},
	})
	got := hgDrpyProto([]hgDrpyProtoField{
		{Tag: 1, Value: "text", Type: "string"},
		{Tag: 2, Value: []byte{0x00, 0xff, 0x10}, Type: "bytes"},
		{Tag: 3, Value: int64(-12345), Type: "sint"},
		{Tag: 4, Value: int64(67890), Type: "sint"},
		{Tag: 5, Value: float64(1.5), Type: "float"},
		{Tag: 6, Value: nested, Type: "message"},
		{Tag: 7, Value: "", Type: "string"}, // 空字符串应跳过
	})
	if want := vectors.Proto; hex.EncodeToString(got) != want {
		t.Fatalf("proto = %s，期望 %s", hex.EncodeToString(got), want)
	}
}

func TestHgDrpyDeviceProtoMatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	got := hgDrpyDeviceProto("1000000000000001", vectors.ProcessNonce, vectors.ProcessStarted)
	if want := vectors.DeviceProto; hex.EncodeToString(got) != want {
		t.Fatalf("deviceProto = %s，期望 %s", hex.EncodeToString(got), want)
	}
}

func TestHgDrpyXmxorAndKeyHashMatchOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	key := make([]byte, 32)
	for index := range key {
		key[index] = 0x3c
	}
	got := hgDrpyXmxor([]byte("xmxor-vector-payload-0123456789abcdef"), key)
	if want := vectors.Xmxor; hex.EncodeToString(got) != want {
		t.Fatalf("xmxor = %s，期望 %s", hex.EncodeToString(got), want)
	}
	hash, seed := hgDrpyKeyHash(0x13579bdf)
	if got := hex.EncodeToString(hash); got != vectors.KeyHash[0] {
		t.Fatalf("keyHash hash = %s，期望 %s", got, vectors.KeyHash[0])
	}
	if got := hex.EncodeToString(seed); got != vectors.KeyHash[1] {
		t.Fatalf("keyHash seed = %s，期望 %s", got, vectors.KeyHash[1])
	}
}

func TestHgDrpyAesV3MatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	for _, item := range vectors.AesV3 {
		data := []byte("aesv3-vector-" + itoa64(item.Khronos) + "-0123456789abcdefghij")
		got := hgDrpyNewAesV3(hgDrpyMedusaAesKey, uint32(item.Khronos)).encrypt(data, hgDrpyMedusaAesIV)
		if want := item.Output; hex.EncodeToString(got) != want {
			t.Fatalf("AesV3(khronos=%d) = %s，期望 %s", item.Khronos, hex.EncodeToString(got), want)
		}
	}
}

func itoa64(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

func TestHgDrpyBuildMedusaMatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	url := hgDrpyVideoAPI + "?device_id=1000000000000001&ts=1758000000&_rticket=1758000000000"
	rands := &hgDrpyMedusaRand{
		MessageRand: vectors.MedusaRands.MessageRand,
		EnvLaunch:   vectors.MedusaRands.EnvLaunch,
		EnvPid:      vectors.MedusaRands.EnvPid,
		KeyHashRand: vectors.MedusaRands.KeyHashRand,
		XmRand:      vectors.MedusaRands.XmRand,
	}
	// drpys 的 buildMedusa 返回 base64 字符串（x-medusa 头值即该文本），
	// Go 侧 hgDrpyBuildMedusa 产出原始字节、base64 由调用方加（见 signedVideoRequest）。
	// 对拍时按 JS 语义比对：raw → base64 → ASCII 字节 hex。
	raw := hgDrpyBuildMedusa(url, []byte(`{"biz_param":{}}`), 1758000000, "1000000000000001", "7.1.3.32", vectors.ProcessNonce, 1758000000, vectors.ProcessStarted, rands)
	got := base64.StdEncoding.EncodeToString(raw)
	wantBytes, err := hex.DecodeString(vectors.BuildMedusa)
	if err != nil {
		t.Fatalf("vectors.buildMedusa 不是合法 hex: %v", err)
	}
	if got != string(wantBytes) {
		t.Fatalf("buildMedusa = %s，期望 %s", got, string(wantBytes))
	}
	if len(got) != vectors.BuildMedusaLen {
		t.Fatalf("buildMedusa(base64) 长度 = %d，期望 %d", len(got), vectors.BuildMedusaLen)
	}
}

func TestHgDrpyContentKeyMatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	for _, item := range vectors.DeriveContentKey {
		key, err := hgDrpyContentKey(item.Input)
		if item.Output != nil {
			if err != nil {
				t.Fatalf("deriveContentKey(%s) 失败: %v（期望 %s）", item.Input, err, *item.Output)
			}
			if got := hex.EncodeToString(key); got != *item.Output {
				t.Fatalf("deriveContentKey(%s) = %s，期望 %s", item.Input, got, *item.Output)
			}
			continue
		}
		if err == nil {
			t.Fatalf("deriveContentKey(%s) 意外成功（drpys 在该输入下报错）", item.Input)
		}
	}
}

func TestHgDrpyDecryptSpadeURLMatchesOracle(t *testing.T) {
	vectors := loadHgDrpyVectors(t)
	for _, item := range vectors.DecryptSpadeURL {
		seed, err := hex.DecodeString(item.Seed)
		if err != nil {
			t.Fatal(err)
		}
		got, err := hgDrpyDecryptSpadeURL(item.Input, seed)
		if err != nil {
			t.Fatalf("decryptSpadeUrl 失败: %v", err)
		}
		if got != item.Output {
			t.Fatalf("decryptSpadeUrl = %q，期望 %q", got, item.Output)
		}
	}
}

func TestHgDrpySignedVideoRequestShape(t *testing.T) {
	// 不带 oracle 对拍（含大量随机量），只验证结构自洽：
	// 10 个头齐全、branch 避开 1、query 编码不含未编码的 !'( )、khronos 自洽。
	vectors := loadHgDrpyVectors(t)
	payload := []byte(`{"biz_param":{}}`)
	url, headers := hgDrpySignedVideoRequest(payload, fixedHgDrpyNow(vectors))
	for _, key := range []string{"User-Agent", "x-gorgon", "x-khronos", "x-ladon", "x-argus", "x-helios", "x-medusa", "x-ss-stub", "x-ss-req-ticket", "Content-Type"} {
		if headers[key] == "" {
			t.Fatalf("缺少请求头 %s", key)
		}
	}
	if !strings.HasPrefix(url, hgDrpyVideoAPI+"?") {
		t.Fatalf("URL 形态异常: %s", url)
	}
	if strings.ContainsAny(url, "!'()") {
		t.Fatalf("query 含未编码字符: %s", url)
	}
	khronos := headers["x-khronos"]
	if !strings.Contains(url, "ts="+khronos) {
		t.Fatalf("query 的 ts 与 x-khronos 不一致: %s vs %s", url, khronos)
	}
}
