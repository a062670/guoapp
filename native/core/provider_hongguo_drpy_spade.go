package core

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"strings"
)

// drpys 的 Spade 媒体地址解密与密钥派生（路线一）。
//
// 与 playback.go 的 hongguoContentKey（备用接口链路）是同一算法的两个版本：
// 本文件严格按 drpys 移植（偶数链初值 246、无 base36 padding 校验），
// 曾经由 Node 实测端到端验证（contentKey → CENC 解密 → NAL 起始码确认）；
// hongguoContentKey 的偶数链初值为 250 且带 app_v2/web_v2 校验，
// 从未经端到端验证（备用接口上游已断），两链路人各一路，互不影响。

// hgDrpySpadeConstants 是 decryptSpadeUrl 的固定常量（64 字节，drpys constants）。
var hgDrpySpadeConstants = []byte{
	0x4d, 0xd4, 0xc2, 0xe6, 0xb8, 0x31, 0x62, 0x09, 0x0e, 0x52, 0xb3, 0xc7, 0xa6, 0x73, 0x3b, 0xa4,
	0x1c, 0xb2, 0x46, 0x2b, 0x82, 0x9a, 0xb5, 0x8a, 0x19, 0x6b, 0x39, 0xdb, 0x57, 0x17, 0x75, 0x24,
	0xf4, 0x9b, 0xaf, 0x7f, 0x08, 0xe8, 0xd6, 0x8d, 0x26, 0xa7, 0x2e, 0x37, 0xc1, 0xa9, 0x5a, 0x2f,
	0x1f, 0x05, 0xa5, 0x18, 0x92, 0xae, 0xf2, 0x94, 0x97, 0x32, 0xb6, 0x2a, 0x38, 0xaa, 0xdd, 0x58,
}

// hgDrpyContentKey 从 spade_a 派生 AES-128 内容密钥（drpys deriveContentKey）。
//
// 与 hongguoContentKey 的差异：偶数链 previous 初值为 246（drpys 原始值），
// 且不做 base36 padding / app_v2 标签校验——drpys 版经端到端实测验证。
func hgDrpyContentKey(value string) ([]byte, error) {
	if len(value) > 1024 {
		return nil, errors.New("红果媒体密钥数据过长")
	}
	raw, err := decodeHongguoBase64(value)
	if err != nil || len(raw) < 3 {
		return nil, errors.New("红果媒体密钥编码无效")
	}
	workLength := len(raw) - int(raw[0]^raw[1]^raw[2]) + 47
	if workLength > len(raw)-1 {
		workLength = len(raw) - 1
	}
	if workLength < 33 {
		return nil, errors.New("红果媒体密钥结构无效")
	}
	work := make([]byte, workLength)
	copy(work, raw[1:1+workLength])
	// drpys：a=85（奇数链）、b=246（偶数链）；previous 交替取用后更新为旧值。
	odd, even := byte(85), byte(246)
	for index := 0; index < workLength; index++ {
		old := work[index]
		previous := odd
		if index%2 == 0 {
			previous = even
			even = old
		} else {
			odd = old
		}
		popcount := byte(0)
		for position := index; position > 0; position >>= 1 {
			popcount += byte(position & 1)
		}
		work[index] = (previous ^ old) - 21 - popcount
	}
	hexText := string(work[1:33])
	if len(hexText) != 32 {
		return nil, errors.New("红果媒体密钥内容无效")
	}
	for index := 0; index < 32; index++ {
		current := hexText[index]
		if (current >= '0' && current <= '9') || (current >= 'a' && current <= 'f') {
			continue
		}
		return nil, errors.New("红果媒体密钥格式无效")
	}
	key := make([]byte, 16)
	for index := 0; index < 16; index++ {
		high := hexText[index*2]
		low := hexText[index*2+1]
		key[index] = hexDigitValue(high)<<4 | hexDigitValue(low)
	}
	return key, nil
}

func hexDigitValue(value byte) byte {
	switch {
	case value >= '0' && value <= '9':
		return value - '0'
	default:
		return value - 'a' + 10
	}
}

// hgDrpyDecryptSpadeURL 解密 Spade 媒体地址（drpys decryptSpadeUrl）：
// raw = base64(value)，校验 0xa8/0x01/0x00 头后取 16 字节整数倍密文，
// AES-128-CBC（key/iv 由 sha512(seed) 与固定常量二次派生）解密，
// 去 PKCS#7 尾 padding 与尾随 0。
func hgDrpyDecryptSpadeURL(value string, seed []byte) (string, error) {
	encoded := strings.TrimSpace(value)
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return "", errors.New("红果片源密文编码无效")
		}
	}
	if len(raw) < 5 {
		return "", errors.New("红果片源密文过短")
	}
	if raw[0] != 0xa8 || raw[2] != 0x01 || raw[3] != 0x00 {
		return "", errors.New("红果片源密文头格式错误")
	}
	cipherLength := (len(raw) - 4) / 16 * 16
	if cipherLength == 0 {
		return "", errors.New("红果片源密文没有完整块")
	}
	first := sha512.Sum512(seed)
	material := make([]byte, 0, len(first)+len(hgDrpySpadeConstants))
	material = append(material, first[:]...)
	material = append(material, hgDrpySpadeConstants...)
	second := sha512.Sum512(material)
	block, err := aes.NewCipher(second[:16])
	if err != nil {
		return "", err
	}
	plaintext := make([]byte, cipherLength)
	cipher.NewCBCDecrypter(block, second[16:32]).CryptBlocks(plaintext, raw[4:4+cipherLength])
	if len(plaintext) > 0 {
		padding := int(plaintext[len(plaintext)-1])
		if padding >= 1 && padding <= 16 && padding <= len(plaintext) {
			plaintext = plaintext[:len(plaintext)-padding]
		}
	}
	for len(plaintext) > 0 && plaintext[len(plaintext)-1] == 0 {
		plaintext = plaintext[:len(plaintext)-1]
	}
	return string(plaintext), nil
}
