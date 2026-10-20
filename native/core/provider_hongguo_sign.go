package core

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"net/http"
	"strconv"
	"time"
)

func newHongguoDeviceID() string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return strconv.FormatUint(1_000_000_000_000_000_000+binary.BigEndian.Uint64(random[:])%8_000_000_000_000_000_000, 10)
}

// signHongguoRequest 红果 App 请求头签名。
//
// 【服务端规则与实测依据】2026-10-16 用参考实现（sunnysky123/hongguo-web 的
// PureSign.java，六头纯代码实现）编译成预言机后逐组对照实测（同一 URL / 同一 body）：
//
//	完全不发签名头                              → 放行（正常返回）
//	gorgon + khronos（+ ss-stub / ss-req-ticket）→ HTTP 200 **空 body**（比不签名更糟）
//	gorgon + khronos + argus + ladon            → 正常返回（2.3 万字节完整数据）
//	完整六头（再加 helios / medusa）             → 同样正常，但 **helios/medusa 并非必需**
//
// ⇒ 一旦携带签名，**必须带齐 gorgon / khronos / argus / ladon 这一最小集**；
// 不完整的签名会被服务端当作伪造成客户端处理（返空）。本端点不校验 helios/medusa。
//
// 因此 2026-10-15 15:30 前后本项目取流全挂的真因是：Phase 1/2 只发了
// gorgon + khronos（+ ss-stub / ss-req-ticket），**缺 argus / ladon**，
// 服务端上线该校验后即被拒。**Gorgon 算法本身没有错**——实测本函数的 Gorgon
// 与参考实现的 RC4 变体虽算法不同，两者都被服务端接受。
//
// 【2026-10-16 07:50 复验快照】同一规则再次逐组实测（各 3 次），结论稳定：
//
//	旧四头 gorgon+khronos+ss-stub+ss-req-ticket → 0/3（200 空 body）
//	补齐 argus+ladon                            → 3/3
//	**有 argus+ladon 但不带 gorgon**             → 3/3（argus/ladon 才是校验关键，
//	                                              gorgon 孤立出现反而有害）
//	仅 gorgon+khronos                           → 0/3
//	完全不带签名                                 → 3/3
//
// 【设备标识维度已排除】hongguo-drama 的 issue #19 提到「平台风控按设备限制取址」，
// 其 v1.7 用「每次请求更换合成设备标识符」绕过（时间线自 10/13 起，早于 10/15 的
// 签名校验，属**另一类**故障）。2026-10-16 用 2×2 矩阵实测（固定/轮换 device_id ×
// 带/不带 iid，各 5 次）**四组全部 5/5 通过** ⇒ 本端点在「匿名 + 完整签名 + 免费集」
// 下不按设备限流，本项目沿用 state 中持久化的 device_id / iid 即可，无需改为逐请求轮换。
// （另：hongguo-drama 的 app-source.js 同时带 iid 与 device_id 且逐请求更换；
// 低星仓库 sgccdev 的 README 称「不要加 iid，会被风控拦」，该说法实测未复现。）
//
// X-Argus / X-Ladon 构造极简，就是同一时间戳的 base64 两种字节序：
//
//	x-argus = base64(le32(khronos))
//	x-ladon = base64(be32(khronos))
func signHongguoRequest(request *http.Request, body []byte, now time.Time) {
	timestamp := uint32(now.Unix())
	queryHash := md5.Sum([]byte(request.URL.RawQuery))
	var payload [20]byte
	copy(payload[:4], queryHash[:4])
	if body != nil {
		bodyHash := md5.Sum(body)
		copy(payload[4:8], bodyHash[:4])
		request.Header.Set("X-SS-STUB", fmt.Sprintf("%X", bodyHash))
	}
	copy(payload[12:16], []byte{0, 6, 11, 28})
	binary.BigEndian.PutUint32(payload[16:], timestamp)
	key := [...]byte{0x44, 0xb9, 0xb9, 0xd9, 0xa4, 0xae, 0xf9, 0xfc, 0xa4, 0x93, 0xaa, 0x75, 0x7c, 0xa3, 0xc2, 0xc4, 0xa4, 0x96, 0x93, 0x8f}
	for index := range payload {
		payload[index] ^= key[index]
	}
	for index := range payload {
		mixed := bits.RotateLeft8(payload[index], 4) ^ payload[(index+1)%len(payload)]
		payload[index] = bits.Reverse8(mixed) ^ 0xff ^ byte(len(payload))
	}
	signature := append([]byte{0x84, 0x04, 0x40, 0x1c, 0, 0}, payload[:]...)
	request.Header.Set("X-Gorgon", hex.EncodeToString(signature))
	request.Header.Set("X-Khronos", strconv.FormatUint(uint64(timestamp), 10))

	// 最小签名集另两头：同一时间戳的 base64 两种字节序（小端 / 大端各一份）。
	//
	// 服务端校验升级后，携带签名的请求必须四头齐送（Gorgon / Khronos /
	// Argus / Ladon），缺任一头都会拿到 200 空 body——接口看似成功、
	// 实际取不到播放地址，表现为无法播放。四头齐送即可，服务端不校验
	// 其他签名头。
	var stamp [4]byte
	binary.LittleEndian.PutUint32(stamp[:], timestamp)
	request.Header.Set("X-Argus", base64.StdEncoding.EncodeToString(stamp[:]))
	binary.BigEndian.PutUint32(stamp[:], timestamp)
	request.Header.Set("X-Ladon", base64.StdEncoding.EncodeToString(stamp[:]))
	request.Header.Set("X-SS-Req-Ticket", strconv.FormatInt(now.UnixMilli(), 10))
}
