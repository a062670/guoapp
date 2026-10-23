package core

import (
	"crypto/md5"
	"encoding/binary"
	"math/bits"
)

// drpys 6 神签名的基础位运算与 Gorgon 链（路线一）。
//
// Gorgon 真链（drpys 版）：xGorgon = "8404" + rand16 + rc4Gorgon(md5(query)[:4] + md5(body)[:4] + 0 + sdk + be32(ts))
// 其中 rc4Gorgon 输出再经逐字节 nibble 交换 + 位反转混淆。
// 服务端只校验存在性与时间自洽（khronos 与 x-gorgon 内的 ts 一致），
// 实测同一请求重放不同随机数的签名均被接受。

// hgDrpyRor32 是 32 位右旋（drpys ror32：n = count & 31，右旋 n 位）。
// math/bits 只提供左旋，右旋 n 位等价左旋 (32-n)&31 位。
func hgDrpyRor32(value uint32, count uint32) uint32 {
	return bits.RotateLeft32(value, int((32-count&31)&31))
}

// hgDrpyLe32/hgDrpyBe32 ...
func hgDrpyLe32(value uint32) [4]byte {
	var out [4]byte
	binary.LittleEndian.PutUint32(out[:], value)
	return out
}

func hgDrpyBe32(value uint32) [4]byte {
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], value)
	return out
}

// hgDrpyGetIv 是指纹混合函数：逐字节折叠进 32 位状态（drpys getIv）。
func hgDrpyGetIv(iv uint32, data []byte) uint32 {
	value := iv
	for index, current := range data {
		if index%2 == 0 {
			value = (value >> 4) ^ value ^ (value << 6) ^ uint32(current)
		} else {
			value = ^((value >> 7) ^ value ^ (uint32(current) | (value << 12)))
		}
	}
	return value
}

// hgDrpySumMd5 是指纹校验和（drpys sumMd5）：取数据前 12 字节折叠出 32 位值。
// 注意 drpys 对不足 12 字节的输入按 0 读取越界位置（JS Buffer 越界读为 undefined，
// 参与位运算时按 0 处理），此处保持同样语义。
func hgDrpySumMd5(data []byte) uint32 {
	check := uint32(0x20220420)
	for index := 0; index < 12; index++ {
		var current byte
		if index < len(data) {
			current = data[index]
		}
		temp := (check >> 3) ^ check
		if index%2 != 0 {
			temp = (check >> 5) ^ check
		}
		if index%2 == 0 {
			check = uint32(current) ^ (check << 7)
		} else {
			check = uint32(current) | (check << 11)
			check = ^check
		}
		check = check ^ temp
	}
	return (check | 4) ^ 0x1000000
}

// hgDrpyMd5V3Step 是 md5SumV3 的单步（drpys md5V3Step）：
// f 按 kind 选布尔函数，整体为 b + rol32(a + f + m + constant, shift)。
func hgDrpyMd5V3Step(kind int, a, b, c, d, m, shift, constant uint32) uint32 {
	var f uint32
	switch kind {
	case 0:
		f = (b & c) | (^b & d)
	case 1:
		f = (b & d) | (c & ^d)
	case 2:
		f = b ^ c ^ d
	default:
		f = c ^ (b | ^d)
	}
	return b + bits.RotateLeft32(a+f+m+constant, int(shift))
}

// hgDrpyMd5SumV3 是魔改 MD5（drpys md5SumV3）：
// 初态/终态按 countV2 右旋，消息字按 orders 索引，步进加 sv 常量，
// 输出 16 字节后再附 sumMd5 的 4 字节小端，共 20 字节。
func hgDrpyMd5SumV3(message []byte, countV2 uint32, orders []byte, countV1 uint32) [20]byte {
	var out [20]byte
	var sv [64]uint32
	for index := 0; index < 64; index++ {
		// drpys 用 ror32（右旋），勿误写左旋——方向写反会得到完全不同的常量表。
		sv[index] = hgDrpyRor32(hgDrpyBranch2SV[index], countV1)
	}
	start := [4]uint32{
		hgDrpyRor32(0x79e0f2fb, countV2),
		hgDrpyRor32(0xc8b52570, countV2),
		hgDrpyRor32(0xebc2f8cd, countV2),
		hgDrpyRor32(0x7c104d93, countV2),
	}
	endCount := (countV2 + 6) & 255
	end := [4]uint32{
		hgDrpyRor32(0x19be4866, endCount),
		hgDrpyRor32(0xe85986b4, endCount),
		hgDrpyRor32(0xe19b326e, endCount),
		hgDrpyRor32(0x71d1d7d4, endCount),
	}
	var m [16]uint32
	for index := 0; index < 16; index++ {
		// 真实链路输入恒为 64 字节（branch 0/2 的 data 各 64）；不足时按 0 读取，
		// 避免未来输入变化导致 panic（drpys 在 Node 中越界读会抛错）。
		var word uint32
		if index*4+4 <= len(message) {
			word = binary.LittleEndian.Uint32(message[index*4:])
		}
		m[index] = word
	}
	a, b, c, d := start[0], start[1], start[2], start[3]
	run := func(kind int, shifts [16]int, offset int) {
		for index := 0; index < 16; index++ {
			phase := index & 3
			word := m[orders[offset+index]]
			var next uint32
			switch phase {
			case 0:
				next = hgDrpyMd5V3Step(kind, a, b, c, d, word, uint32(shifts[index]), sv[offset+index])
				a = next
			case 1:
				next = hgDrpyMd5V3Step(kind, d, a, b, c, word, uint32(shifts[index]), sv[offset+index])
				d = next
			case 2:
				next = hgDrpyMd5V3Step(kind, c, d, a, b, word, uint32(shifts[index]), sv[offset+index])
				c = next
			default:
				next = hgDrpyMd5V3Step(kind, b, c, d, a, word, uint32(shifts[index]), sv[offset+index])
				b = next
			}
		}
	}
	run(0, [16]int{7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22}, 0)
	run(1, [16]int{5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20}, 16)
	run(2, [16]int{4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23}, 32)
	run(3, [16]int{6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21}, 48)
	values := [4]uint32{(start[0] + a) ^ end[0], (start[1] + b) ^ end[1], (start[2] + c) ^ end[2], (start[3] + d) ^ end[3]}
	for index := 0; index < 4; index++ {
		binary.LittleEndian.PutUint32(out[index*4:], values[index])
	}
	tail := hgDrpySumMd5(out[:16])
	binary.LittleEndian.PutUint32(out[16:], tail)
	return out
}

// hgDrpyBranch2F13 是 hashF13 的 branch 2 变体（drpys branch2F13）。
func hgDrpyBranch2F13(iv uint32, querySm3, bodyMd5, tsBytes []byte, khronos uint32) [20]byte {
	v := (iv & 13) * 86
	ivV0 := ((v >> 15) & 255) + ((v >> 8) & 255)
	n1 := [5]uint32{0x8980f29b, 0xeb549c7f, 0xb08726db, 0xd40cb5e6, 0xe8f559e4}[ivV0]
	countV1 := (n1 + khronos + 1) & 255
	countV2 := n1 + khronos
	shift := (countV2 + 5) & 7
	// drpys 的 seed 是 8 字节（不是 4）：input 总长 = 32+16+4+8+4 = 64。
	seed := [8]byte{0x84, 0x96, 0x77, 0x9d, 0xd4, 0x15, 0x0b, 0xf8}
	pad := make([]byte, 8)
	for index, value := range seed {
		pad[index] = byte(((uint16(value) | (uint16(value) << 8)) >> shift) & 255)
	}
	input := make([]byte, 0, 64)
	input = append(input, querySm3...)
	input = append(input, bodyMd5...)
	input = append(input, tsBytes...)
	input = append(input, pad...)
	input = append(input, 0xa0, 0x01, 0x00, 0x00)
	orders := hgDrpyLoadBranch2Orders()
	return hgDrpyMd5SumV3(input, countV2, orders[ivV0<<6:(ivV0+1)<<6], countV1)
}

// hgDrpyHashF13 是 Gorgon 的 query/body 指纹（drpys hashF13）：
// 由 getIv 链选分支：branch 0 走 8 态自定义压缩（20 字节），
// branch 1 直接拼接常量（36 字节，服务端不认该变体，调用方需抖动 _rticket 避开），
// branch 2 走 md5SumV3（20 字节）。
func hgDrpyHashF13(querySm3, bodyMd5, tsBytes []byte, khronos uint32) []byte {
	iv := hgDrpyGetIv(hgDrpyGetIv(hgDrpyGetIv(0x20230928, querySm3), bodyMd5), tsBytes)
	low := iv & 15
	ivV0 := (low * 171) >> 9
	branch := low - ivV0*3
	switch branch {
	case 2:
		digest := hgDrpyBranch2F13(iv, querySm3, bodyMd5, tsBytes, khronos)
		return digest[:]
	case 0:
		var out [20]byte
		ivV1List := [6]uint32{0xc4a78580, 0xb3c0fd39, 0xc58c5686, 0xc9aa3ba7, 0xf5a7adf2, 0x963c2ed1}
		ivV1 := ivV1List[ivV0]
		countV1 := (ivV1 + khronos + 1) & 255
		countV2 := ivV1 + khronos
		tt02 := [64]uint32{
			0xebb64faf, 0x07aadcc2, 0xcf3187bf, 0xe01138ff, 0x6d0bfcff, 0x5a30a3be, 0xb41ad638, 0x34180eb8,
			0xf233eb6f, 0xb1a584cc, 0xccc30dc7, 0x47d1db51, 0xd55653de, 0x70a84fa1, 0x57473c12, 0xf76f0288,
			0x2c077f0a, 0xda0dcad0, 0xfbb86f6c, 0xfdc4cf00, 0x688a020d, 0xe676c6a6, 0x8cd6338b, 0x1a3c8d0e,
			0xcce8b06b, 0x6ad0ed0b, 0xa0522717, 0xdc71ac83, 0x2285db71, 0xd5b4dda6, 0x736f8650, 0x6560306c,
			0x617ce2a6, 0xe423417e, 0x0a40e143, 0x544e4032, 0x88dffb2a, 0x716c1ae0, 0x4c467a88, 0x05b23bb3,
			0xe1d0b866, 0xbaa3dcb8, 0xae3374d3, 0xc3381a50, 0x1702f75b, 0xfe6da368, 0xf0b4cf48, 0x4e0ffbb8,
			0x72aad10d, 0x26c53a3d, 0xf2bce0f6, 0xb4557581, 0x4a257fdd, 0x8c3182a2, 0xab0b3b86, 0x3d5dfb14,
			0x4f103634, 0xd37b52d7, 0x444eff16, 0xeb0a33d1, 0x6ca86f6e, 0x00284ba7, 0x08387cfa, 0x5fb37586,
		}
		var tt [64]uint32
		for index, value := range tt02 {
			// drpys 用 ror32（右旋）。
			tt[index] = hgDrpyRor32(value, countV1)
		}
		n0 := (countV2 + 2) & 7
		seed := [4]byte{0xfa, 0x45, 0x61, 0xd7}
		pad := make([]byte, 4)
		for index, value := range seed {
			pad[index] = byte(((uint16(value) | (uint16(value) << 8)) >> n0) & 255)
		}
		data := make([]byte, 0, 40+len(pad)+8)
		data = append(data, querySm3...)
		data = append(data, bodyMd5...)
		data = append(data, tsBytes...)
		data = append(data, pad...)
		data = append(data, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0xa0)
		// 消息扩展：di 从 data 的 4 字节大端字起步，滚动扩展 112 个新字。
		di := make([]uint32, 0, 128+112)
		for index := 0; index+4 <= len(data); index += 4 {
			di = append(di, binary.BigEndian.Uint32(data[index:]))
		}
		di0 := di[0]
		for index := 0; index < 112; index++ {
			di1 := di[index+1]
			di14 := di[index+14]
			r1 := bits.RotateLeft32(di1, 14) ^ bits.RotateLeft32(di1, 25) ^ (di1 >> 3)
			r2 := bits.RotateLeft32(di14, 13) ^ bits.RotateLeft32(di14, 15) ^ (di14 >> 10)
			next := di0 + di[index+9] + r1 + r2
			di = append(di, next)
			di0 = di1
		}
		init := [8]uint32{
			hgDrpyRor32(0x7aba4fc8, countV2&31),
			hgDrpyRor32(0x67166507, countV2&31),
			hgDrpyRor32(0x6403fa00, countV2&31),
			hgDrpyRor32(0x340f512f, countV2&31),
			hgDrpyRor32(984304912, countV2&31),
			hgDrpyRor32(3005047866, countV2&31),
			hgDrpyRor32(2874125293, countV2&31),
			hgDrpyRor32(2152413264, countV2&31),
		}
		chosen := [6][11]int{
			{101, 5, 7, 6, 3, 2, 1, 0, 5, 4, 3},
			{96, 0, 6, 7, 5, 3, 2, 1, 5, 4, 4},
			{96, 7, 6, 2, 1, 4, 0, 5, 4, 3, 5},
			{99, 3, 6, 2, 4, 5, 1, 0, 0, 7, 6},
			{96, 0, 5, 6, 7, 3, 1, 2, 5, 4, 4},
			{100, 2, 0, 3, 5, 4, 6, 7, 2, 1, 5},
		}[ivV0]
		d := init
		for index := 0; index < chosen[0]; index++ {
			base := ivV1 + uint32(index)
			n0v := di[base&127]
			n1 := ((d[chosen[3]] ^ d[chosen[4]]) & d[chosen[1]]) ^ d[chosen[3]]
			n2 := bits.RotateLeft32(d[chosen[1]], 26) ^ bits.RotateLeft32(d[chosen[1]], 21) ^ bits.RotateLeft32(d[chosen[1]], 7)
			n4 := n0v + n1 + n2 + tt[base&63] + d[chosen[5]]
			n5 := bits.RotateLeft32(d[chosen[2]], 30) ^ bits.RotateLeft32(d[chosen[2]], 19) ^ bits.RotateLeft32(d[chosen[2]], 10)
			n6 := (d[chosen[2]] & d[chosen[6]]) | ((d[chosen[2]] | d[chosen[6]]) & d[chosen[7]])
			n7 := n5 + n6
			old := d[chosen[9]]
			// d.unshift(d.pop())：末尾元素移到首位。
			last := d[7]
			for position := 7; position > 0; position-- {
				d[position] = d[position-1]
			}
			d[0] = last
			d[chosen[10]] = n7 + n4
			d[chosen[8]] = old + n4
		}
		var ret [32]byte
		for index := 0; index < 8; index++ {
			binary.BigEndian.PutUint32(ret[index*4:], d[index]+init[index])
		}
		for index := 0; index < 16; index++ {
			ret[index] ^= ret[index+16]
		}
		copy(out[:16], ret[:16])
		tail := hgDrpySumMd5(ret[:16])
		binary.LittleEndian.PutUint32(out[16:], tail)
		return out[:]
	default:
		// branch 1：直接拼接（服务端不认该变体，调用方需抖动 _rticket 避开）。
		// 36 字节 = querySm3 前 16 + bodyMd5 全 16 + sumMd5(querySm3+bodyMd5) 的 le32。
		material := make([]byte, 0, 36)
		material = append(material, querySm3[:16]...)
		material = append(material, bodyMd5...)
		tail := hgDrpySumMd5(material)
		stamp := hgDrpyLe32(tail)
		material = append(material, stamp[:]...)
		return material
	}
}

// hgDrpyReverseBits 反转一个字节的位序（drpys reverseBits）。
func hgDrpyReverseBits(value byte) byte {
	result := byte(0)
	for index := 0; index < 8; index++ {
		result = (result << 1) | ((value >> index) & 1)
	}
	return result
}

// hgDrpyRc4Gorgon 是 RC4 变体（drpys rc4Gorgon）：
// 初始化只赋值 s[i]=s[j]（不交换回写，非标准 RC4），
// 输出字节取自 s[(y+y)&255]，且 i 从 1 起（先自增）。
func hgDrpyRc4Gorgon(data []byte, key []byte) []byte {
	var s [256]byte
	for index := range s {
		s[index] = byte(index)
	}
	j := 0
	for index := 0; index < 256; index++ {
		j = (j + int(s[index]) + int(key[index%len(key)])) & 255
		s[index] = s[j]
	}
	i := 0
	j = 0 // drpys 在输出循环前显式重置 j（let i = 0; j = 0;），漏掉会整体跑偏
	out := make([]byte, len(data))
	for index, value := range data {
		i++
		x := s[i]
		j += int(x)
		y := s[j&255]
		s[i] = y
		out[index] = value ^ s[(int(y)*2)&255]
	}
	return out
}

// hgDrpyXGorgon 生成 X-Gorgon 头的 hex 值（drpys xGorgon）。
//
// 入参 random 为 16 位随机数（同时进入 RC4 key 与头前缀）。
func hgDrpyXGorgon(query string, body []byte, timestamp, random uint32) string {
	queryDigest := md5.Sum([]byte(query))
	var bodyDigest [16]byte
	if len(body) > 0 {
		bodyDigest = md5.Sum(body)
	}
	sdk := hgDrpyLe32(67503104)
	stamp := hgDrpyBe32(timestamp)
	input := make([]byte, 0, 4+4+4+4+4)
	input = append(input, queryDigest[:4]...)
	input = append(input, bodyDigest[:4]...)
	input = append(input, 0, 0, 0, 0)
	input = append(input, sdk[:]...)
	input = append(input, stamp[:]...)
	key := []byte{0x4a, 0x40, 0x16, byte((random >> 8) & 255), 0x47, 0x6c, 0x01, byte(random & 255)}
	out := hgDrpyRc4Gorgon(input, key)
	for index := range out {
		value := bits.RotateLeft8(out[index], 4)
		next := out[(index+1)%len(out)]
		out[index] = ^(hgDrpyReverseBits(next^value) ^ 20)
	}
	signature := make([]byte, 0, 2+4+len(out))
	signature = append(signature, 0x84, 0x04)
	signature = append(signature, byte(random&255), byte((random>>8)&255), 0x40, 0x01)
	signature = append(signature, out...)
	const hexDigits = "0123456789abcdef"
	encoded := make([]byte, len(signature)*2)
	for index, value := range signature {
		encoded[index*2] = hexDigits[value>>4]
		encoded[index*2+1] = hexDigits[value&0x0f]
	}
	return string(encoded)
}
