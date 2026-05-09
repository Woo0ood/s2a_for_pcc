package audittoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
)

const auditTokenPrefix = "sk-pa-" // payload audit

// GenerateAuditToken 生成 32 字节 CSPRNG 随机令牌（base32 编码无填充），
// 返回明文 token（带 sk-pa- 前缀）和它的 SHA256 hex hash。
// 明文只在创建时返回一次，存储只保留 hash。
func GenerateAuditToken() (token, hashedHex string) {
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	token = auditTokenPrefix + encoded
	sum := sha256.Sum256([]byte(token))
	hashedHex = hex.EncodeToString(sum[:])
	return
}

// HashAuditToken 计算给定 token 的 SHA256 hex hash。
func HashAuditToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyAuditToken 用恒定时间比较 provided token 的 hash 与已存的 hash 是否匹配。
func VerifyAuditToken(provided, expectedHashedHex string) bool {
	h := HashAuditToken(provided)
	return subtle.ConstantTimeCompare([]byte(h), []byte(expectedHashedHex)) == 1
}
