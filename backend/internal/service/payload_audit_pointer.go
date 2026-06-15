package service

import (
	"net/url"
	"strconv"
	"strings"
)

const (
	PointerKindBlob = "blob"
	PointerKindBody = "body"

	blobScheme = "s2a-blob://sha256-"
	bodyScheme = "s2a-body://sha256-"
)

// Pointer 是旁路对象在库内的可解析指针。
type Pointer struct {
	Kind   string // PointerKindBlob | PointerKindBody
	SHA256 string
	MIME   string
	Bytes  int
}

// EncodeBlobPointer 生成大对象指针。mime 做 URL 转义以保证整体仍是合法 JSON 字符串值。
func EncodeBlobPointer(sha, mime string, bytes int) string {
	return blobScheme + sha + "?mime=" + url.QueryEscape(mime) + "&bytes=" + strconv.Itoa(bytes)
}

// EncodeBodyPointer 生成超大正文指针。
func EncodeBodyPointer(sha string, bytes int) string {
	return bodyScheme + sha + "?bytes=" + strconv.Itoa(bytes)
}

// isLowerHex reports whether s is a non-empty string of lowercase hex digits.
// Pointer SHAs feed object-store key construction, so they must not contain
// path-traversal characters ('/', '.', etc.) or anything outside [0-9a-f].
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// IsHexSHA256 reports whether s is exactly 64 lowercase hex digits (a real
// sha256). Used to validate untrusted sha path params at HTTP boundaries.
func IsHexSHA256(s string) bool { return len(s) == 64 && isLowerHex(s) }

// ParsePointer 解析指针；非指针返回 ok=false。
func ParsePointer(s string) (Pointer, bool) {
	var kind, rest string
	switch {
	case strings.HasPrefix(s, blobScheme):
		kind, rest = PointerKindBlob, s[len(blobScheme):]
	case strings.HasPrefix(s, bodyScheme):
		kind, rest = PointerKindBody, s[len(bodyScheme):]
	default:
		return Pointer{}, false
	}
	sha, query, _ := strings.Cut(rest, "?")
	if !isLowerHex(sha) {
		return Pointer{}, false
	}
	p := Pointer{Kind: kind, SHA256: sha}
	vals, err := url.ParseQuery(query)
	if err != nil {
		return Pointer{}, false
	}
	p.MIME = vals.Get("mime")
	p.Bytes, _ = strconv.Atoi(vals.Get("bytes"))
	return p, true
}

// IsPointer 报告字符串是否为旁路指针。
func IsPointer(s string) bool { _, ok := ParsePointer(s); return ok }
