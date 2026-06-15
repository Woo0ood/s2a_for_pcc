package service

import (
	"strings"
	"testing"
)

func TestCollector_AppendRawEvent_StructuredOutput(t *testing.T) {
	snap := &ConfigSnapshot{Enabled: true, AllGroups: true, OutputMaxBytes: 0, ExcerptBytes: 256}
	c := NewPayloadAuditCollector(snap)
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/responses", Stream: true})
	c.AppendOutput("hello world") // text → drives the excerpt
	c.AppendRawEvent([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_xyz\"}}\n\n"))
	c.AppendRawEvent([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
	evt := c.Finalize(200, 0, "")
	if evt == nil {
		t.Fatal("nil event")
	}
	if evt.OutputFormat != "sse" {
		t.Fatalf("want sse, got %q", evt.OutputFormat)
	}
	if !strings.Contains(evt.OutputBody, "response.created") {
		t.Fatalf("raw events not in body: %q", evt.OutputBody)
	}
	if evt.ResponseID != "resp_xyz" {
		t.Fatalf("response id not captured from events, got %q", evt.ResponseID)
	}
	if !strings.Contains(evt.OutputExcerpt, "hello") {
		t.Fatalf("excerpt should come from text, got %q", evt.OutputExcerpt)
	}
}

func TestCollector_NoRawEvent_FallsBackToText(t *testing.T) {
	snap := &ConfigSnapshot{Enabled: true, AllGroups: true, OutputMaxBytes: 0, ExcerptBytes: 256}
	c := NewPayloadAuditCollector(snap)
	c.SetMetadata(PayloadAuditMetadata{Provider: "openai", Endpoint: "/v1/chat/completions"})
	c.AppendOutput("plain text out")
	evt := c.Finalize(200, 0, "")
	if evt.OutputFormat != "text" || evt.OutputBody != "plain text out" {
		t.Fatalf("expected text fallback, got fmt=%q body=%q", evt.OutputFormat, evt.OutputBody)
	}
}
