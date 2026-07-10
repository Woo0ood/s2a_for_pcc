package service

import "testing"

// Real production shapes sampled from lax_s1.payload_audit_logs.

func TestExtractUpstreamModel_ResponsesSSE(t *testing.T) {
	sse := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\"}}\n\n")
	if got := ExtractUpstreamModel("/v1/responses", sse); got != "gpt-5.5" {
		t.Fatalf("got %q, want gpt-5.5", got)
	}
}

func TestExtractUpstreamModel_ChatCompletionJSON(t *testing.T) {
	body := []byte(`{"id":"resp_x","object":"chat.completion","model":"grok-4.20-multi-agent-xhigh","choices":[]}`)
	if got := ExtractUpstreamModel("/v1/chat/completions", body); got != "grok-4.20-multi-agent-xhigh" {
		t.Fatalf("got %q, want grok-4.20-multi-agent-xhigh", got)
	}
}

func TestExtractUpstreamModel_ChatCompletionStreamResponsesFormat(t *testing.T) {
	// This fork routes some chat/completions upstream through the Responses API,
	// so streamed output carries response.model rather than a top-level model.
	sse := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_0\",\"model\":\"gpt-5.4-mini-2026-03-17\"}}\n\n")
	if got := ExtractUpstreamModel("/v1/chat/completions", sse); got != "gpt-5.4-mini-2026-03-17" {
		t.Fatalf("got %q, want gpt-5.4-mini-2026-03-17", got)
	}
}

func TestExtractUpstreamModel_ChatCompletionChunkModel(t *testing.T) {
	sse := []byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o-2024\",\"choices\":[]}\n\ndata: [DONE]\n\n")
	if got := ExtractUpstreamModel("/v1/chat/completions", sse); got != "gpt-4o-2024" {
		t.Fatalf("got %q, want gpt-4o-2024", got)
	}
}

func TestExtractUpstreamModel_GeminiModelVersionJSON(t *testing.T) {
	body := []byte(`{"candidates":[],"modelVersion":"gemini-2.5-pro"}`)
	if got := ExtractUpstreamModel("/v1beta/models/gemini-2.5-pro:generateContent", body); got != "gemini-2.5-pro" {
		t.Fatalf("got %q, want gemini-2.5-pro", got)
	}
}

func TestExtractUpstreamModel_GeminiModelVersionSSE(t *testing.T) {
	sse := []byte("data: {\"candidates\":[],\"modelVersion\":\"gemini-2.5-flash\"}\n\n")
	if got := ExtractUpstreamModel("/v1beta/models/gemini-2.5-flash:streamGenerateContent", sse); got != "gemini-2.5-flash" {
		t.Fatalf("got %q, want gemini-2.5-flash", got)
	}
}

func TestExtractUpstreamModel_Empty(t *testing.T) {
	if got := ExtractUpstreamModel("/v1/responses", nil); got != "" {
		t.Fatalf("nil body: got %q, want empty", got)
	}
	if got := ExtractUpstreamModel("/v1/responses", []byte("data: [DONE]\n\n")); got != "" {
		t.Fatalf("done-only: got %q, want empty", got)
	}
}
