package core

import (
	"crypto/md5"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"strconv"
	"strings"
	"time"
)

// drpys 6 神签名的请求组装——红果 App 播放取址专用。
//
// 与既有的 signHongguoRequest 并存：本文件只服务
// multi_video_model/v1/ 播放取址链路（provider_hongguo_drpy_media.go），
// 不改动 catalog/detail 等既有链路使用的 signHongguoRequest。

// hgDrpyVideoAPI 是 drpys 的两段式取址第一段端点。
const hgDrpyVideoAPI = "https://api5-normal-sinfonlineb.fqnovel.com/novel/player/multi_video_model/v1/"

// hgDrpyVideoUA 必须与 drpys 的 VIDEO_UA 完全一致（服务端按 UA 族校验）。
const hgDrpyVideoUA = "com.phoenix.read/71332 (Linux; U; Android 16; zh_CN; 25053RT47C; Build/BP2A.250605.031.A3; Cronet/TTNetVersion:04657795 2026-01-23 QuicVersion:c67e9834 2025-09-08)"

// hgDrpyDeviceParams 是 drpys DEVICE 常量的有序键值（顺序即 URL 参数顺序，
// 签名对 query 字节级敏感，顺序必须固定）。
var hgDrpyDeviceParams = [][2]string{
	{"ac", "wifi"},
	{"channel", "update_64"},
	{"aid", "8662"},
	{"app_name", "novelread"},
	{"version_code", "71332"},
	{"version_name", "7.1.3.32"},
	{"device_platform", "android"},
	{"os", "android"},
	{"ssmix", "a"},
	{"device_type", "25053RT47C"},
	{"device_brand", "Redmi"},
	{"language", "zh"},
	{"os_api", "36"},
	{"os_version", "16"},
	{"manifest_version_code", "71332"},
	{"resolution", "1280*2772"},
	{"dpi", "520"},
	{"update_version_code", "71332"},
	{"host_abi", "arm64-v8a"},
	{"dragon_device_type", "phone"},
	{"pv_player", "71332"},
	{"compliance_status", "0"},
	{"need_personal_recommend", "1"},
	{"player_so_load", "1"},
	{"is_android_pad_screen", "0"},
}

// hgDrpyLocalID 生成 drpys localId 形态的合成 ID：
// 1000000000000000 + 48 位随机数（十进制字符串）。
func hgDrpyLocalID() string {
	var random [6]byte
	if _, err := crand.Read(random[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	value := uint64(1_000_000_000_000_000)
	for _, current := range random {
		value = value<<8 | uint64(current)
	}
	value %= 9_000_000_000_000_000
	return strconv.FormatUint(1_000_000_000_000_000+value, 10)
}

// hgDrpyPythonURLEncode 复刻 drpys 的 pythonUrlEncode：
// 保留 A-Za-z0-9-_.~ 与 *，其余按 %XX（大写）编码——与 Python
// urllib.parse.quote(safe=”) 的字节输出一致（!'( ) 亦编码）。
func hgDrpyPythonURLEncode(values [][2]string) string {
	var builder strings.Builder
	for index, pair := range values {
		if index > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(hgDrpyQuote(pair[0]))
		builder.WriteByte('=')
		builder.WriteString(hgDrpyQuote(pair[1]))
	}
	return builder.String()
}

func hgDrpyQuote(value string) string {
	const upperDigits = "0123456789ABCDEF"
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		current := value[index]
		switch {
		case current >= 'A' && current <= 'Z',
			current >= 'a' && current <= 'z',
			current >= '0' && current <= '9',
			current == '-' || current == '_' || current == '.' || current == '~' || current == '*':
			builder.WriteByte(current)
		default:
			builder.WriteByte('%')
			builder.WriteByte(upperDigits[current>>4])
			builder.WriteByte(upperDigits[current&0x0f])
		}
	}
	return builder.String()
}

// hgDrpyBranchOf 预测本次请求的 hashF13 分支（drpys branchOf）。
// 返回 1 的签名变体服务端不认，调用方须抖动 _rticket 避开。
func hgDrpyBranchOf(fullURL string, body []byte, khronos uint32) int {
	query := ""
	if index := strings.IndexByte(fullURL, '?'); index >= 0 && index < len(fullURL)-1 {
		query = fullURL[index+1:]
	}
	var bodyMd5 [16]byte
	if len(body) > 0 {
		bodyMd5 = md5.Sum(body)
	}
	querySm3 := hgDrpySM3([]byte(query))
	stamp := hgDrpyLe32(khronos)
	iv := hgDrpyGetIv(hgDrpyGetIv(hgDrpyGetIv(0x20230928, querySm3[:]), bodyMd5[:]), stamp[:])
	low := iv & 15
	return int(low - ((low*171)>>9)*3)
}

// hgDrpyMd5Hex 计算大写 MD5 hex（drpys md5()，用于 x-ss-stub）。
func hgDrpyMd5Hex(value []byte) string {
	digest := md5.Sum(value)
	const upperDigits = "0123456789ABCDEF"
	encoded := make([]byte, len(digest)*2)
	for index, current := range digest {
		encoded[index*2] = upperDigits[current>>4]
		encoded[index*2+1] = upperDigits[current&0x0f]
	}
	return string(encoded)
}

// hgDrpySignedVideoRequest 组装带 6 神签名的播放取址请求（drpys signedVideoRequest）。
//
// 返回完整 URL（含 query）与 10 个请求头。query 内 iid/device_id 每次请求
// 重新合成（与 URL、签名保持一致）；branch 1 变体会通过抖动 _rticket
// （最多 32 秒偏移）避开。
// 等价于 hgDrpySignedVideoRequestAt(payload, now, hgDrpyVideoAPI)。
func hgDrpySignedVideoRequest(payload []byte, now time.Time) (string, map[string]string) {
	return hgDrpySignedVideoRequestAt(payload, now, hgDrpyVideoAPI)
}

// hgDrpySignedVideoRequestAt 与 hgDrpySignedVideoRequest 相同，但端点由 api 指定。
// x-medusa 与 hgDrpyBranchOf 都以 fullURL（含 host）为输入，换端点会自动重算
// 签名、_rticket 抖动循环自动适配分支，无需额外处理。
func hgDrpySignedVideoRequestAt(payload []byte, now time.Time, api string) (string, map[string]string) {
	iid := hgDrpyLocalID()
	deviceID := hgDrpyLocalID()
	khronos := uint32(now.Unix())
	ticketBase := now.UnixMilli()
	var query, fullURL string
	requestTicket := ticketBase
	for offset := int64(0); offset < 32; offset++ {
		requestTicket = ticketBase + offset
		values := make([][2]string, 0, len(hgDrpyDeviceParams)+4)
		values = append(values, hgDrpyDeviceParams...)
		values = append(values,
			[2]string{"iid", iid},
			[2]string{"device_id", deviceID},
			[2]string{"ts", strconv.FormatUint(uint64(khronos), 10)},
			[2]string{"_rticket", strconv.FormatInt(requestTicket, 10)},
		)
		query = hgDrpyPythonURLEncode(values)
		fullURL = api + "?" + query
		if hgDrpyBranchOf(fullURL, payload, khronos) != 1 {
			break
		}
	}
	random := uint32(0)
	var randomBuffer [2]byte
	if _, err := crand.Read(randomBuffer[:]); err == nil {
		random = uint32(binary.LittleEndian.Uint16(randomBuffer[:]))
	}
	bodyDigest := hgDrpyMd5Hex(payload)
	ladonStamp := hgDrpyBe32(khronos)
	argusStamp := hgDrpyLe32(khronos)
	nonce, started := hgDrpyLoadProcess()
	headers := map[string]string{
		"User-Agent":                hgDrpyVideoUA,
		"Accept":                    "application/json; charset=utf-8,application/x-protobuf",
		"Content-Type":              "application/json; charset=UTF-8",
		"x-xs-from-web":             "0",
		"x-ss-req-ticket":           strconv.FormatInt(requestTicket, 10),
		"x-tt-request-tag":          "t=0;n=0",
		"sdk-version":               "2",
		"passport-sdk-version":      "50561",
		"x-vc-bdturing-sdk-version": "3.7.2.cn",
		"x-ss-stub":                 bodyDigest,
		"x-gorgon":                  hgDrpyXGorgon(query, payload, khronos, random),
		"x-khronos":                 strconv.FormatUint(uint64(khronos), 10),
		"x-ladon":                   base64.StdEncoding.EncodeToString(ladonStamp[:]),
		"x-argus":                   base64.StdEncoding.EncodeToString(argusStamp[:]),
		"x-helios":                  hgDrpyHelios(khronos, hgDrpyRandomU32()),
		"x-medusa":                  base64.StdEncoding.EncodeToString(hgDrpyBuildMedusa(fullURL, payload, khronos, deviceID, "7.1.3.32", nonce, started/1000, started, nil)),
		"x-tt-dt":                   "",
	}
	return fullURL, headers
}
