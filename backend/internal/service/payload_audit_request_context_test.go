package service

import (
	"testing"
	"time"
)

func TestCollector_CapturesRequestContextAndUpstreamModel(t *testing.T) {
	c := NewPayloadAuditCollector(newScopedSnap(1000, 4000, 64))
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/responses"})
	c.SetRequestContext("203.0.113.7", "req-abc-123")
	c.AppendRawEvent([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_9\",\"model\":\"gpt-5.5\"}}\n\n"))
	evt := c.Finalize(200, time.Second, "")
	if evt == nil {
		t.Fatal("expected event")
	}
	if evt.ClientIP != "203.0.113.7" {
		t.Fatalf("ClientIP = %q, want 203.0.113.7", evt.ClientIP)
	}
	if evt.RequestID != "req-abc-123" {
		t.Fatalf("RequestID = %q, want req-abc-123", evt.RequestID)
	}
	if evt.UpstreamModel != "gpt-5.5" {
		t.Fatalf("UpstreamModel = %q, want gpt-5.5", evt.UpstreamModel)
	}
	if evt.ResponseID != "resp_9" {
		t.Fatalf("ResponseID = %q, want resp_9", evt.ResponseID)
	}
}

// SetRequestContext is called at handler entry; a later SetMetadata (carrying
// auth/model identity) must not wipe the captured client IP / request id.
func TestCollector_SetMetadataDoesNotClobberRequestContext(t *testing.T) {
	c := NewPayloadAuditCollector(newScopedSnap(1000, 1000, 64))
	c.SetRequestContext("198.51.100.9", "req-xyz")
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/responses", Model: "gpt-5.5"})
	evt := c.Finalize(200, time.Second, "")
	if evt == nil {
		t.Fatal("expected event")
	}
	if evt.ClientIP != "198.51.100.9" || evt.RequestID != "req-xyz" {
		t.Fatalf("clobbered by SetMetadata: ip=%q reqid=%q", evt.ClientIP, evt.RequestID)
	}
}

// A disabled collector must ignore SetRequestContext without panicking.
func TestCollector_SetRequestContextOnDisabledIsNoop(t *testing.T) {
	c := NewPayloadAuditCollector(nil)
	c.SetRequestContext("1.2.3.4", "req-1")
	if evt := c.Finalize(200, time.Second, ""); evt != nil {
		t.Fatal("disabled collector should not emit")
	}
}
