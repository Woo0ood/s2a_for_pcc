package service

import (
	"bytes"
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
