package service

import "testing"

func TestExtractRequestConvIDs_Responses(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"sess-abc","previous_response_id":"resp_prev","store":false}`)
	ck, prev := ExtractRequestConvIDs("/v1/responses", body)
	if ck != "sess-abc" || prev != "resp_prev" {
		t.Fatalf("got ck=%q prev=%q", ck, prev)
	}
}

func TestExtractRequestConvIDs_ChatHasNone(t *testing.T) {
	ck, prev := ExtractRequestConvIDs("/v1/chat/completions", []byte(`{"model":"x","messages":[]}`))
	if ck != "" || prev != "" {
		t.Fatalf("chat should have no conv ids, got %q/%q", ck, prev)
	}
}

func TestExtractResponseID(t *testing.T) {
	// responses 流式：response.created 事件里带 response.id
	sse := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\"}}\n\n")
	if got := ExtractResponseID("/v1/responses", sse); got != "resp_123" {
		t.Fatalf("got %q", got)
	}
	// chat：顶层 id
	if got := ExtractResponseID("/v1/chat/completions", []byte(`{"id":"chatcmpl-9","object":"chat.completion"}`)); got != "chatcmpl-9" {
		t.Fatalf("got %q", got)
	}
}
