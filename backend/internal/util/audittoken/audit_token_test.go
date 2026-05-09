package audittoken

import (
	"strings"
	"testing"
)

func TestAuditToken_GenerateUniqueAndDecodable(t *testing.T) {
	t1, h1 := GenerateAuditToken()
	t2, h2 := GenerateAuditToken()
	if t1 == t2 || h1 == h2 {
		t.Fatal("token/hash collision")
	}
	if !VerifyAuditToken(t1, h1) {
		t.Fatal("verify failed for valid pair")
	}
	if VerifyAuditToken(t2, h1) {
		t.Fatal("cross-verify wrongly succeeded")
	}
}

func TestAuditToken_FormatPrefix(t *testing.T) {
	tok, _ := GenerateAuditToken()
	if !strings.HasPrefix(tok, "sk-pa-") {
		t.Fatalf("missing prefix, got %q", tok)
	}
}

func TestAuditToken_VerifyConstantTimeNoPanic(t *testing.T) {
	_, h := GenerateAuditToken()
	cases := []string{"", "x", strings.Repeat("a", 64), strings.Repeat("a", 1024)}
	for _, c := range cases {
		VerifyAuditToken(c, h) // 仅验证不 panic；时序统计在 CI 上不做
	}
}

func TestAuditToken_HashStability(t *testing.T) {
	if HashAuditToken("hello") != HashAuditToken("hello") {
		t.Fatal("HashAuditToken not deterministic")
	}
}
