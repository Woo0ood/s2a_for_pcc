package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
)

const defaultBlobMinBytes = 256 * 1024 // 256 KiB（解码后）

// ExtractedBlob 是从 body 中抽出的一个内联对象。
type ExtractedBlob struct {
	SHA256 string
	MIME   string
	Bytes  int    // 解码后字节数
	Data   []byte // 解码后原始字节（供上传；上传后应尽快释放）
}

// dataURIRe 匹配 data:<mime>;base64,<run>。
// base64 标准字母表 [A-Za-z0-9+/] + 末尾 '='，不含 '"' 或 '\\'，
// 故匹配不会越过 JSON 字符串边界，替换后 body 仍是合法 JSON。
var dataURIRe = regexp.MustCompile(`data:([a-zA-Z0-9.+/_-]+);base64,([A-Za-z0-9+/]+={0,2})`)

// RewriteInlineBlobs 把 body 中解码后 >= minBytes 的内联 base64 替换为指针，
// 返回改写后的 body 与抽出的对象列表。非法/过小/解码失败的保持原样。
func RewriteInlineBlobs(body []byte, minBytes int) ([]byte, []ExtractedBlob) {
	if minBytes <= 0 {
		minBytes = defaultBlobMinBytes
	}
	locs := dataURIRe.FindAllSubmatchIndex(body, -1)
	if locs == nil {
		return body, nil
	}
	var blobs []ExtractedBlob
	out := make([]byte, 0, len(body))
	last := 0
	for _, m := range locs {
		full0, full1 := m[0], m[1]
		mime := string(body[m[2]:m[3]])
		b64 := body[m[4]:m[5]]
		if base64.StdEncoding.DecodedLen(len(b64)) < minBytes {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(string(b64))
		if err != nil || len(data) < minBytes {
			continue
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		blobs = append(blobs, ExtractedBlob{SHA256: sha, MIME: mime, Bytes: len(data), Data: data})
		out = append(out, body[last:full0]...)
		out = append(out, EncodeBlobPointer(sha, mime, len(data))...)
		last = full1
	}
	if last == 0 {
		return body, nil
	}
	out = append(out, body[last:]...)
	return out, blobs
}
