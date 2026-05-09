package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ──────────────────────────────────────────────────────
// OpenAI Chat stream tests
// ──────────────────────────────────────────────────────

func TestExtractOpenAIChatStream(t *testing.T) {
	cases := map[string][]string{
		"plain.sse":     {"hello world"},
		"tool.sse":      {"[tool_call name=get_weather", "[tool_args:", "[finish: tool_calls]"},
		"refusal.sse":   {"[refusal: I cannot help"},
		"reasoning.sse": {"[reasoning: Let me think", "The answer is 42"},
		"error.sse":     {"[error: Rate limit exceeded]"},
	}
	for name, wants := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata/payload_audit_extract/openai_chat", name))
			if err != nil {
				t.Fatal(err)
			}
			got := ExtractOpenAIChatStream(data)
			for _, w := range wants {
				if !strings.Contains(got, w) {
					t.Fatalf("missing %q in:\n%s", w, got)
				}
			}
		})
	}
}

// ──────────────────────────────────────────────────────
// OpenAI Responses stream tests
// ──────────────────────────────────────────────────────

func TestExtractOpenAIResponsesStream(t *testing.T) {
	cases := map[string][]string{
		"plain.sse":     {"Hello", " world"},
		"refusal.sse":   {"[refusal: I'm sorry"},
		"reasoning.sse": {"[reasoning: Thinking step 1", "Final answer"},
		"tool.sse":      {"[tool_args delta={\"location\":"},
		"failed.sse":    {"partial", "[failed: server overloaded]"},
	}
	for name, wants := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata/payload_audit_extract/openai_responses", name))
			if err != nil {
				t.Fatal(err)
			}
			got := ExtractOpenAIResponsesStream(data)
			for _, w := range wants {
				if !strings.Contains(got, w) {
					t.Fatalf("missing %q in:\n%s", w, got)
				}
			}
		})
	}
}

// ──────────────────────────────────────────────────────
// Anthropic stream tests
// ──────────────────────────────────────────────────────

func TestExtractAnthropicStream(t *testing.T) {
	cases := map[string][]string{
		"plain.sse":    {"Hello world", "[stop: end_turn]"},
		"tool.sse":     {"[tool_use name=get_weather id=toolu_01]", "[tool_use args=", "[stop: tool_use]"},
		"thinking.sse": {"[thinking: Let me analyze", "Here is my answer"},
		"error.sse":    {"partial", "[error: Overloaded]"},
		"stop.sse":     {"Done", "[stop: max_tokens]"},
	}
	for name, wants := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata/payload_audit_extract/anthropic", name))
			if err != nil {
				t.Fatal(err)
			}
			got := ExtractAnthropicStream(data)
			for _, w := range wants {
				if !strings.Contains(got, w) {
					t.Fatalf("missing %q in:\n%s", w, got)
				}
			}
		})
	}
}

// ──────────────────────────────────────────────────────
// Gemini stream tests
// ──────────────────────────────────────────────────────

func TestExtractGeminiStream(t *testing.T) {
	cases := map[string][]string{
		"plain.sse":     {"Hello world!"},
		"function.sse":  {"[function_call name=get_weather args="},
		"safety.sse":    {"partial response", "[finish: SAFETY]"},
		"blocked.sse":   {"[blocked: SAFETY]"},
		"multipart.sse": {"Part one", " and part two", " final", "[finish: MAX_TOKENS]"},
	}
	for name, wants := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata/payload_audit_extract/gemini", name))
			if err != nil {
				t.Fatal(err)
			}
			got := ExtractGeminiStream(data)
			for _, w := range wants {
				if !strings.Contains(got, w) {
					t.Fatalf("missing %q in:\n%s", w, got)
				}
			}
		})
	}
}

// ──────────────────────────────────────────────────────
// Gemini JSON array fallback
// ──────────────────────────────────────────────────────

func TestExtractGeminiStream_JSONArray(t *testing.T) {
	raw := []byte(`[{"candidates":[{"content":{"parts":[{"text":"from array"}]},"finishReason":"STOP"}]},{"candidates":[{"content":{"parts":[{"text":" more"}]},"finishReason":"MAX_TOKENS"}]}]`)
	got := ExtractGeminiStream(raw)
	if !strings.Contains(got, "from array") {
		t.Fatalf("missing 'from array' in: %s", got)
	}
	if !strings.Contains(got, "[finish: MAX_TOKENS]") {
		t.Fatalf("missing '[finish: MAX_TOKENS]' in: %s", got)
	}
}

// ──────────────────────────────────────────────────────
// Input extraction tests
// ──────────────────────────────────────────────────────

func TestExtractInputText_OpenAIChat(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`)
	text, format := ExtractInputText("openai", "/v1/chat/completions", body)
	if !strings.Contains(text, "[system] sys") {
		t.Fatalf("missing system message in: %s", text)
	}
	if !strings.Contains(text, "[user] hello") {
		t.Fatalf("missing user message in: %s", text)
	}
	if format != "json" {
		t.Fatalf("expected format=json, got %s", format)
	}
}

func TestExtractInputText_OpenAIResponses_String(t *testing.T) {
	body := []byte(`{"input":"What is Go?"}`)
	text, format := ExtractInputText("openai", "/v1/responses", body)
	if !strings.Contains(text, "What is Go?") {
		t.Fatalf("missing input text in: %s", text)
	}
	if format != "json" {
		t.Fatalf("expected format=json, got %s", format)
	}
}

func TestExtractInputText_OpenAIResponses_Array(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"hello from array"}]}`)
	text, format := ExtractInputText("openai", "/v1/responses", body)
	if !strings.Contains(text, "hello from array") {
		t.Fatalf("missing input text in: %s", text)
	}
	if format != "json" {
		t.Fatalf("expected format=json, got %s", format)
	}
}

func TestExtractInputText_Anthropic(t *testing.T) {
	body := []byte(`{"system":"be helpful","messages":[{"role":"user","content":"hi"}]}`)
	text, format := ExtractInputText("anthropic", "/v1/messages", body)
	if !strings.Contains(text, "[system] be helpful") {
		t.Fatalf("missing system in: %s", text)
	}
	if !strings.Contains(text, "[user] hi") {
		t.Fatalf("missing user message in: %s", text)
	}
	if format != "json" {
		t.Fatalf("expected format=json, got %s", format)
	}
}

func TestExtractInputText_Gemini(t *testing.T) {
	body := []byte(`{"contents":[{"parts":[{"text":"explain Go"}]}]}`)
	text, format := ExtractInputText("gemini", "/v1beta/models/gemini-pro:generateContent", body)
	if !strings.Contains(text, "explain Go") {
		t.Fatalf("missing text in: %s", text)
	}
	if format != "json" {
		t.Fatalf("expected format=json, got %s", format)
	}
}

func TestExtractInputText_OpenAIImages(t *testing.T) {
	body := []byte(`{"prompt":"a cat on a rocket","size":"1024x1024"}`)
	text, format := ExtractInputText("openai", "/v1/images/generations", body)
	if !strings.Contains(text, "a cat on a rocket") {
		t.Fatalf("missing prompt in: %s", text)
	}
	if format != "json" {
		t.Fatalf("expected format=json, got %s", format)
	}
}

func TestExtractInputText_FallbackOnUnknown(t *testing.T) {
	text, format := ExtractInputText("unknown", "/x", []byte("just bytes"))
	if !strings.Contains(text, "just bytes") {
		t.Fatalf("missing raw fallback in: %s", text)
	}
	if format != "raw" {
		t.Fatalf("expected format=raw, got %s", format)
	}
}

func TestExtractInputText_InvalidJSON(t *testing.T) {
	text, format := ExtractInputText("openai", "/v1/chat/completions", []byte("not json at all"))
	if text != "not json at all" {
		t.Fatalf("expected raw fallback, got: %s", text)
	}
	if format != "raw" {
		t.Fatalf("expected format=raw, got %s", format)
	}
}

func TestExtractInputText_ContentArray(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"from array content"}]}]}`)
	text, format := ExtractInputText("openai", "/v1/chat/completions", body)
	if !strings.Contains(text, "from array content") {
		t.Fatalf("missing array content text in: %s", text)
	}
	if format != "json" {
		t.Fatalf("expected format=json, got %s", format)
	}
}

// ──────────────────────────────────────────────────────
// Per-chunk extractor unit tests
// ──────────────────────────────────────────────────────

func TestExtractOpenAIChatDeltaText_Content(t *testing.T) {
	got := ExtractOpenAIChatDeltaText([]byte(`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`))
	if got != "hi" {
		t.Fatalf("expected 'hi', got %q", got)
	}
}

func TestExtractOpenAIChatDeltaText_InvalidJSON(t *testing.T) {
	got := ExtractOpenAIChatDeltaText([]byte("not json"))
	if got != "" {
		t.Fatalf("expected empty for invalid json, got %q", got)
	}
}

func TestExtractAnthropicEventText_NoEventType(t *testing.T) {
	got := ExtractAnthropicEventText("", []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`))
	if got != "" {
		t.Fatalf("expected empty for unknown event type, got %q", got)
	}
}

func TestExtractGeminiStreamFrame_Empty(t *testing.T) {
	got := ExtractGeminiStreamFrame([]byte(`{}`))
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
