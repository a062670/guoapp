package core

import "math/bits"

// 红果「6 神」签名体系——drpys 开源实现移植（路线一）。
//
// 来源：woshishiq1/drpys 的 spider/js/红果果[短].js（commit 22261adf），
// 经 N3urda/hongguoTV 移植为 Node 实现，本文件群将其逐函数移植到 Go。
// 与另一套本地实现（RC4 Gorgon / heliosWith / BuildMedusaFor）
// 是同一套字节算法的两个独立载体：常量表已逐值比对一致（hashF13 的
// T64A/TBL5/TBL6/K8 与 drpys 的 tt02/ivV1List/init/n1 表完全相同），
// 仅 Gorgon/Helios 的封装形式不同（drpys 用 md5SumV3+hashF13，
// 另一套实现用 RC4 变体 + heliosWith，服务端两者均接受）。
//
// 命名约定：本文件群所有标识符带 hgDrpy 前缀，与既有的 hongguo* 系列
// 并存不冲突，两者可直接共存。

// hgDrpySM3 计算 SM3（GB/T 32905）摘要。
//
// 移植自 drpys 的 sm3.js（纯 JS 实现，不依赖 Node crypto 的 SM3）。
// 逐块 64 字节、68 个消息字、64 轮压缩函数，输出 32 字节大端。
func hgDrpySM3(input []byte) [32]byte {
	var digest [32]byte
	// 填充：数据 + 0x80 + 0x00... + 8 字节大端位长度，总长为 64 的倍数。
	total := ((len(input) + 9) + 63) / 64 * 64
	data := make([]byte, total)
	copy(data, input)
	data[len(input)] = 0x80
	for index := len(input) + 1; index < total-8; index++ {
		data[index] = 0
	}
	bitLength := uint64(len(input)) * 8
	for index := 0; index < 8; index++ {
		data[total-8+index] = byte(bitLength >> (56 - 8*index))
	}
	state := [8]uint32{0x7380166f, 0x4914b2b9, 0x172442d7, 0xda8a0600, 0xa96f30bc, 0x163138aa, 0xe38dee4d, 0xb0fb0e4e}
	var words [68]uint32
	for offset := 0; offset < total; offset += 64 {
		for index := 0; index < 16; index++ {
			words[index] = uint32(data[offset+index*4])<<24 | uint32(data[offset+index*4+1])<<16 |
				uint32(data[offset+index*4+2])<<8 | uint32(data[offset+index*4+3])
		}
		for index := 16; index < 68; index++ {
			x := words[index-16] ^ words[index-9] ^ bits.RotateLeft32(words[index-3], 15)
			words[index] = x ^ bits.RotateLeft32(x, 15) ^ bits.RotateLeft32(x, 23) ^ bits.RotateLeft32(words[index-13], 7) ^ words[index-6]
		}
		a, b, c, d, e, f, g, h := state[0], state[1], state[2], state[3], state[4], state[5], state[6], state[7]
		for index := 0; index < 64; index++ {
			a12 := bits.RotateLeft32(a, 12)
			tj := uint32(0x79cc4519)
			if index >= 16 {
				tj = 0x7a879d8a
			}
			s1 := bits.RotateLeft32(a12+e+bits.RotateLeft32(tj, index), 7)
			s2 := s1 ^ a12
			var ff, gg uint32
			if index < 16 {
				ff = a ^ b ^ c
				gg = e ^ f ^ g
			} else {
				ff = (a & b) | (a & c) | (b & c)
				gg = (e & f) | (^e & g)
			}
			t1 := ff + d + s2 + (words[index] ^ words[index+4])
			t2 := gg + h + s1 + words[index]
			d = c
			c = bits.RotateLeft32(b, 9)
			b = a
			a = t1
			h = g
			g = bits.RotateLeft32(f, 19)
			f = e
			e = t2 ^ bits.RotateLeft32(t2, 9) ^ bits.RotateLeft32(t2, 17)
		}
		state[0] ^= a
		state[1] ^= b
		state[2] ^= c
		state[3] ^= d
		state[4] ^= e
		state[5] ^= f
		state[6] ^= g
		state[7] ^= h
	}
	for index := 0; index < 8; index++ {
		digest[index*4] = byte(state[index] >> 24)
		digest[index*4+1] = byte(state[index] >> 16)
		digest[index*4+2] = byte(state[index] >> 8)
		digest[index*4+3] = byte(state[index])
	}
	return digest
}
