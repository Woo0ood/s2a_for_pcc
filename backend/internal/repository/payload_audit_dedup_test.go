package repository

import "testing"

func TestBatchTokenStable(t *testing.T) {
	a := []*PayloadAuditEvent{{ID: 3}, {ID: 1}, {ID: 2}}
	b := []*PayloadAuditEvent{{ID: 1}, {ID: 2}, {ID: 3}}
	if BatchToken(a) != BatchToken(b) {
		t.Fatal("token must be order-invariant")
	}
}

func TestBatchTokenDiffers(t *testing.T) {
	a := []*PayloadAuditEvent{{ID: 1}, {ID: 2}}
	b := []*PayloadAuditEvent{{ID: 1}, {ID: 3}}
	if BatchToken(a) == BatchToken(b) {
		t.Fatal("token must differ when ids differ")
	}
}

func TestBatchTokenLength(t *testing.T) {
	a := []*PayloadAuditEvent{{ID: 1}}
	if got := len(BatchToken(a)); got != 16 {
		t.Fatalf("len = %d, want 16", got)
	}
}

func TestBatchTokenEmpty(t *testing.T) {
	if BatchToken(nil) == "" {
		t.Fatal("empty batch should still return a token")
	}
}
