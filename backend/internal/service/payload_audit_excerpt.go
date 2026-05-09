package service

import (
	"fmt"
	"unicode/utf8"
)

const minExcerpt = 32

// excerpt 把 text 截取成至多 total 字节的摘要，超长时保留头部 + 中间分隔标记 + 尾部。
// total <= 0 或 text == "" 时返回空串；len(text) <= total 时原样返。
// 若 total 太小 (sep 长度都装不下)，退化为单端截断 + 静态短标记。
// 始终在 UTF-8 rune 边界截断，结果保证是合法 UTF-8。
func excerpt(text string, total int) string {
	if total <= 0 || text == "" {
		return ""
	}
	if len(text) <= total {
		return text
	}
	truncatedBytes := len(text) - total
	sep := fmt.Sprintf("\n…[truncated %d bytes]…\n", truncatedBytes)

	// 边界保护：sep 可能比 total 还长（极小 total 或极大 truncatedBytes）
	if len(sep) >= total-minExcerpt {
		marker := "…[truncated]"
		budget := total - len(marker)
		if budget < 0 {
			budget = 0
		}
		return safeTruncateUTF8(text, budget) + marker
	}

	half := (total - len(sep)) / 2
	if half <= 0 {
		return safeTruncateUTF8(text, total)
	}
	return safeTruncateUTF8(text, half) + sep + safeTruncateUTF8Tail(text, half)
}

// safeTruncateUTF8 返回 s 的前 n 字节子串，但若末端切到了多字节 rune 中间，
// 向左收缩直到落在 rune 边界。结果总是合法 UTF-8。
func safeTruncateUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	out := s[:n]
	for len(out) > 0 && !utf8.ValidString(out) {
		out = out[:len(out)-1]
	}
	return out
}

// safeTruncateUTF8Tail 返回 s 的后 n 字节子串，但若开头切到了多字节 rune 中间，
// 向右推进直到落在 rune 边界。结果总是合法 UTF-8。
func safeTruncateUTF8Tail(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	out := s[len(s)-n:]
	for len(out) > 0 && !utf8.ValidString(out) {
		out = out[1:]
	}
	return out
}
