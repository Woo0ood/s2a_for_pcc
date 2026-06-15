package service

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func toInt64Ptr(v int64) *int64 { return &v }

func newScopedSnap(maxIn, maxOut, excerptBytes int) *ConfigSnapshot {
	return &ConfigSnapshot{
		Enabled:        true,
		AllGroups:      true,
		InputMaxBytes:  maxIn,
		OutputMaxBytes: maxOut,
		ExcerptBytes:   excerptBytes,
		Generation:     1,
	}
}

func TestCollector_DisabledIsNoop(t *testing.T) {
	c := NewPayloadAuditCollector(nil)
	c.SetInput([]byte("x"), "json")
	c.AppendOutput("hello")
	if evt := c.Finalize(200, time.Second, ""); evt != nil {
		t.Fatal("expected nil for disabled collector")
	}
}

func TestCollector_TruncatesInputAtCap(t *testing.T) {
	c := NewPayloadAuditCollector(newScopedSnap(100, 100, 64))
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/chat/completions"})
	big := bytes.Repeat([]byte("a"), 500)
	c.SetInput(big, "json")
	evt := c.Finalize(200, time.Second, "")
	if evt == nil {
		t.Fatal("expected event")
	}
	if !evt.InputTruncated {
		t.Fatal("InputTruncated should be true")
	}
	if evt.InputBytes != 500 {
		t.Fatalf("InputBytes should reflect original, got %d", evt.InputBytes)
	}
	if len(evt.InputBody) != 100 {
		t.Fatalf("InputBody should be capped to 100, got %d", len(evt.InputBody))
	}
}

func TestCollector_AppendOutputStopsAtCap(t *testing.T) {
	c := NewPayloadAuditCollector(newScopedSnap(1000, 100, 64))
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/chat/completions"})
	chunk := bytes.Repeat([]byte("b"), 30)
	for i := 0; i < 5; i++ {
		c.AppendOutput(string(chunk))
	}
	evt := c.Finalize(200, time.Second, "")
	if !evt.OutputTruncated {
		t.Fatal("should be truncated after 100 byte cap")
	}
	if len(evt.OutputBody) > 100 {
		t.Fatalf("body should be <= 100, got %d", len(evt.OutputBody))
	}
	if evt.OutputBytes != 150 {
		t.Fatalf("OutputBytes should reflect total appended (150), got %d", evt.OutputBytes)
	}
}

func TestCollector_GroupNotInScope(t *testing.T) {
	snap := &ConfigSnapshot{
		Enabled:        true,
		AllGroups:      false,
		GroupIDs:       map[int64]struct{}{1: {}, 2: {}},
		InputMaxBytes:  1000,
		OutputMaxBytes: 1000,
		ExcerptBytes:   64,
	}
	c := NewPayloadAuditCollector(snap)
	c.SetMetadata(PayloadAuditMetadata{GroupID: toInt64Ptr(99)})
	c.SetInput([]byte("foo"), "json")
	if evt := c.Finalize(200, 0, ""); evt != nil {
		t.Fatal("out-of-scope group should not emit")
	}
}

func TestCollector_FinalizeIdempotent(t *testing.T) {
	c := NewPayloadAuditCollector(newScopedSnap(1000, 1000, 64))
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/chat/completions"})
	c.SetInput([]byte("hello"), "json")
	e1 := c.Finalize(200, time.Second, "")
	e2 := c.Finalize(200, time.Second, "")
	if e1 == nil || e2 != nil {
		t.Fatalf("expected first non-nil, second nil; got %v / %v", e1, e2)
	}
}

func TestCollector_OutputOmitted(t *testing.T) {
	c := NewPayloadAuditCollector(newScopedSnap(1000, 1000, 64))
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/embeddings"})
	c.SetInput([]byte(`{"input":"foo"}`), "json")
	c.MarkOutputOmitted()
	evt := c.Finalize(200, time.Second, "")
	if !evt.OutputOmitted {
		t.Fatal("OutputOmitted should be true")
	}
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
func containsPointer(s string) bool {
	return strings.Contains(s, "s2a-blob://") || strings.Contains(s, "s2a-body://")
}

func TestCollector_SetInput_OffloadExtractsPending(t *testing.T) {
	snap := &ConfigSnapshot{Enabled: true, AllGroups: true, OffloadEnabled: true, BlobOffloadMinBytes: 512, InputMaxBytes: 0}
	c := NewPayloadAuditCollector(snap)
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/responses"})
	raw := make([]byte, 1024)
	body := []byte(`{"image_url":"data:image/png;base64,` + base64Std(raw) + `"}`)
	c.SetInput(body, "json")
	if len(c.PendingBlobs()) != 1 {
		t.Fatalf("want 1 pending blob, got %d", len(c.PendingBlobs()))
	}
	if !containsPointer(c.InputBodyForTest()) {
		t.Fatal("input body should contain pointer after rewrite")
	}
}

func TestCollector_SetInput_OversizedBodyOffloaded(t *testing.T) {
	snap := &ConfigSnapshot{Enabled: true, AllGroups: true, OffloadEnabled: true, BlobOffloadMinBytes: 1 << 20, InputMaxBytes: 16}
	c := NewPayloadAuditCollector(snap)
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/responses"})
	c.SetInput([]byte(`{"prompt":"this body is way over sixteen bytes"}`), "json")
	if c.PendingBody() == nil {
		t.Fatal("expected pending body offload")
	}
	if !containsPointer(c.InputBodyForTest()) {
		t.Fatal("input body should be a body pointer")
	}
}

func TestCollector_SetInput_ExtractsConversationIDs(t *testing.T) {
	snap := newScopedSnap(1000, 1000, 64)
	c := NewPayloadAuditCollector(snap)
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/responses"})
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"sess-1","previous_response_id":"resp_0","store":false}`)
	c.SetInput(body, "json")
	evt := c.Finalize(200, 0, "")
	if evt == nil {
		t.Fatal("expected event")
	}
	if evt.ConversationKey != "sess-1" {
		t.Fatalf("ConversationKey: want %q got %q", "sess-1", evt.ConversationKey)
	}
	if evt.PreviousResponseID != "resp_0" {
		t.Fatalf("PreviousResponseID: want %q got %q", "resp_0", evt.PreviousResponseID)
	}
}

func TestCollector_SetResponseID(t *testing.T) {
	snap := newScopedSnap(1000, 1000, 64)
	c := NewPayloadAuditCollector(snap)
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/responses"})
	c.SetInput([]byte(`{"model":"gpt-5.5","prompt_cache_key":"sess-1","previous_response_id":"resp_0"}`), "json")
	c.SetResponseID("resp_9")
	evt := c.Finalize(200, 0, "")
	if evt == nil {
		t.Fatal("expected event")
	}
	if evt.ResponseID != "resp_9" {
		t.Fatalf("ResponseID: want %q got %q", "resp_9", evt.ResponseID)
	}
}

// A panic mid-collection must NEVER escape SetInput into the LLM request path:
// it recovers, discards staged offload state, and leaves a fallback marker so a
// row still lands.
func TestCollector_SetInputPanicRecovered(t *testing.T) {
	setInputPanicHook = func() { panic("boom in collection") }
	defer func() { setInputPanicHook = nil }()

	c := NewPayloadAuditCollector(newScopedSnap(1000, 1000, 64))
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/responses"})

	// Must not propagate the panic.
	c.SetInput([]byte(`{"model":"gpt-5.4","input":"hi"}`), "json")

	if len(c.PendingBlobs()) != 0 || c.PendingBody() != nil {
		t.Fatalf("staged offload must be cleared after panic, got blobs=%d body=%v",
			len(c.PendingBlobs()), c.PendingBody())
	}
	if got := c.InputBodyForTest(); !strings.Contains(got, "capture failed") {
		t.Fatalf("expected fallback marker, got %q", got)
	}
	if evt := c.Finalize(200, time.Second, ""); evt == nil {
		t.Fatal("collector should still finalize a row after a recovered panic")
	}
}

// recoverCollect (deferred by AppendOutput/AppendRawEvent) must swallow panics so
// a collection bug cannot break the streaming response path.
func TestCollector_RecoverCollectSwallows(t *testing.T) {
	c := NewPayloadAuditCollector(newScopedSnap(1000, 1000, 64))
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("recoverCollect should swallow, but panic escaped: %v", r)
		}
	}()
	func() {
		defer c.recoverCollect("test")
		panic("boom in append")
	}()
}
