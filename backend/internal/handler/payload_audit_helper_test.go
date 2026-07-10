package handler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
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

// Regression for the production bug: behind the nginx reverse proxy the direct
// peer (RemoteAddr) is the docker bridge gateway (172.18.0.1), so the audited
// client_ip was always that gateway. AttachPayloadAuditCollector must resolve the
// real client from the forwarded-header chain, and stamp the server request id.
func TestAttachCollector_CapturesForwardedClientIPAndRequestID(t *testing.T) {
	svc := &service.PayloadAuditService{}
	svc.InstallSnapshotForTest(&service.ConfigSnapshot{
		Enabled: true, AllGroups: true,
		InputMaxBytes: 1000, OutputMaxBytes: 1000, ExcerptBytes: 64, Generation: 1,
	})
	c := newAuditGinCtx("POST", "/v1/responses", "172.18.0.1")
	c.Request.Header.Set("X-Real-IP", "203.0.113.42")
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestID, "req-777"))

	coll := AttachPayloadAuditCollector(c, svc, "openai", "/v1/responses")
	if !coll.Enabled() {
		t.Fatal("expected enabled collector")
	}
	coll.SetInput([]byte(`{"model":"gpt-5.5"}`), "json")
	evt := coll.Finalize(200, time.Second, "")
	if evt == nil {
		t.Fatal("expected event")
	}
	if evt.ClientIP != "203.0.113.42" {
		t.Fatalf("ClientIP = %q, want 203.0.113.42 (X-Real-IP, not RemoteAddr 172.18.0.1)", evt.ClientIP)
	}
	if evt.RequestID != "req-777" {
		t.Fatalf("RequestID = %q, want req-777", evt.RequestID)
	}
}

func TestFinalize_NilCollectorIsNoop(t *testing.T) {
	FinalizePayloadAudit(nil, nil, nil, 200, time.Second, "")
}

func TestFinalize_DisabledCollectorIsNoop(t *testing.T) {
	coll := service.NewPayloadAuditCollector(nil)
	FinalizePayloadAudit(coll, nil, nil, 200, time.Second, "")
	// must not panic
}
