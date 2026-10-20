package core

// LibreTV / TVBox 社区配置工具的 Base58 订阅支持。
//
// 背景：LunaTV-config 之类的配置工具会把整份 TVBox 配置预处理好再分发，
// 除原始 JSON 外还提供 Base58 编码的订阅链接（format=2 为原始 JSON、
// format=3 为带代理前缀的 JSON）。客户端拿到的是一个 Base58 单行文本
// （text/plain），解码后就是标准 {"sites":[...]} 配置。
//
// 好处是导入时不必实时解析 type≥3 的脚本配置：工具已把可用源筛好、
// 代理前缀也替换完毕，客户端只要解码 + 解析 JSON 即可，无自建后端也能用。
//
// 编码规则沿用 bs58 / Bitcoin 字母表，无校验和；订阅文件可能带换行，
// 解码前统一去掉空白字符。

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// libretvBase58Alphabet bs58（Bitcoin）字母表，去掉了易混淆的 0/O/I/l。
const libretvBase58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var libretvBase58Radix = big.NewInt(58)

// libretvStripSpace 去掉全部空白：Base58 本体不含空白，订阅文件常带换行。
func libretvStripSpace(text string) string {
	return strings.Map(func(char rune) rune {
		switch char {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return char
	}, text)
}

// libretvBase58Decode 解码 Base58 文本为原始字节。
// 前导 '1' 按 bs58 约定还原成前导 0x00 字节。
func libretvBase58Decode(text string) ([]byte, error) {
	raw := libretvStripSpace(text)
	if raw == "" {
		return nil, errors.New("Base58 内容为空")
	}
	value := new(big.Int)
	for index, char := range raw {
		position := strings.IndexRune(libretvBase58Alphabet, char)
		if position < 0 {
			return nil, fmt.Errorf("第 %d 个字符不是合法 Base58：%q", index+1, char)
		}
		value.Mul(value, libretvBase58Radix)
		value.Add(value, big.NewInt(int64(position)))
	}
	decoded := value.Bytes()
	leading := 0
	for leading < len(raw) && raw[leading] == '1' {
		leading++
	}
	out := make([]byte, leading+len(decoded))
	copy(out[leading:], decoded)
	return out, nil
}

// looksLikeLibreTVBase58 粗判一段文本是否像 Base58 订阅内容：
// 足够长、字符全部落在字母表内、且不含 JSON / URL / XML 的特征字符。
// 仅用于避免对普通文本做无意义的大数解码，最终仍以解码后能否解析成配置为准。
func looksLikeLibreTVBase58(text string) bool {
	raw := libretvStripSpace(text)
	if len(raw) < 32 {
		return false
	}
	if strings.Contains(raw, "://") || strings.ContainsAny(raw, "{}<>\"'") {
		return false
	}
	for _, char := range raw {
		if strings.IndexRune(libretvBase58Alphabet, char) < 0 {
			return false
		}
	}
	return true
}
