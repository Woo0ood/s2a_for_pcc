package handler

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func newAuditGinCtx(method, path, ip string) *gin.Context {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = ip + ":12345"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return c
}

func TestAttachCollector_NilServiceReturnsDisabled(t *testing.T) {
	c := newAuditGinCtx("POST", "/v1/chat/completions", "1.2.3.4")
	coll := AttachPayloadAuditCollector(c, nil, "openai", "/v1/chat/completions")
	if coll == nil {
		t.Fatal("should return non-nil even when nil svc")
	}
	if coll.Enabled() {
		t.Fatal("should be disabled")
	}
}

func TestAttachCollector_DisabledSnapshotReturnsDisabled(t *testing.T) {
	// Zero-value PayloadAuditService has no stored snapshot, so
	// Snapshot() returns nil → NewPayloadAuditCollector(nil) → disabled.
	svc := &service.PayloadAuditService{}
	c := newAuditGinCtx("POST", "/x", "1.2.3.4")
	coll := AttachPayloadAuditCollector(c, svc, "openai", "/x")
	if coll.Enabled() {
		t.Fatal("should be disabled when svc has no snapshot")
	}
}

func TestFinalize_NilCollectorIsNoop(t *testing.T) {
	FinalizePayloadAudit(nil, nil, 200, time.Second, "")
}

func TestFinalize_DisabledCollectorIsNoop(t *testing.T) {
	coll := service.NewPayloadAuditCollector(nil)
	FinalizePayloadAudit(coll, nil, 200, time.Second, "")
	// must not panic
}
