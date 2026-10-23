package core

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"math/bits"
	"strconv"
)

// drpys 6 神签名的 X-Helios（路线一）。
//
// 结构（drpys helios）：le32(random) + 34 轮 Feistel 加密的
// "${khronos}-1588093228-8662"（16 字节分组、每块 34 轮），整体 base64。
// 密钥表来自 md5(le32(random) + "8662") 的 hex  ASCII 串（4 个 u64 小端读入）。
// 服务端不校验内容（每次请求随机），只要求格式合法。

// hgDrpyHelios 生成 X-Helios 头的 base64 值（drpys helios）。
func hgDrpyHelios(khronos uint32, random uint32) string {
	data := make([]byte, 0, 8)
	stamp := hgDrpyLe32(random)
	data = append(data, stamp[:]...)
	data = append(data, '8', '6', '6', '2')
	digest := md5.Sum(data)
	ascii := make([]byte, 32)
	const hexDigits = "0123456789abcdef"
	for index, value := range digest {
		ascii[index*2] = hexDigits[value>>4]
		ascii[index*2+1] = hexDigits[value&0x0f]
	}
	// words：hex ASCII 串按 8 字节小端读成 4 个 u64。
	words := make([]uint64, 4)
	for index := 0; index < 4; index++ {
		words[index] = binary.LittleEndian.Uint64(ascii[index*8:])
	}
	table := []uint64{words[0]}
	b0 := words[0]
	b8 := words[1]
	queue := append([]uint64(nil), words[2:]...)
	for index := int64(0); index < 34; index++ {
		x8 := bits.RotateLeft64(b8, 64-8) + b0
		x8 ^= uint64(index)
		queue = append(queue, x8)
		x8 ^= bits.RotateLeft64(b0, 64-61)
		table = append(table, x8)
		b0 = x8
		b8 = queue[0]
		queue = queue[1:]
	}
	text := []byte(strconv.FormatUint(uint64(khronos), 10) + "-1588093228-8662")
	padLength := (len(text)/16 + 1) * 16
	if padLength < len(text)+1 {
		padLength = len(text) + 1
	}
	pad := make([]byte, padLength)
	for index := range pad {
		pad[index] = byte(16 - len(text)%16)
	}
	copy(pad, text)
	output := make([]byte, 0, padLength)
	for at := 0; at < padLength; at += 16 {
		a := binary.LittleEndian.Uint64(pad[at:])
		b := binary.LittleEndian.Uint64(pad[at+8:])
		for index := 0; index < 34; index++ {
			b = table[index] ^ (a + bits.RotateLeft64(b, 64-8))
			a = b ^ bits.RotateLeft64(a, 64-61)
		}
		block := make([]byte, 16)
		binary.LittleEndian.PutUint64(block, a)
		binary.LittleEndian.PutUint64(block[8:], b)
		output = append(output, block...)
	}
	result := make([]byte, 0, 4+len(output))
	result = append(result, stamp[:]...)
	result = append(result, output...)
	return base64.StdEncoding.EncodeToString(result)
}
