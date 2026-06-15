package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ─────────────────────────────────────────────────────
// Helper: build a rich Transcript for HTML rendering tests.
// ─────────────────────────────────────────────────────

func buildRichTranscript() service.Transcript {
	return service.Transcript{
		Turns: []service.Turn{
			{
				Index:      1,
				CreatedAt:  time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
				Model:      "gpt-4o",
				StatusCode: 200,
				UserItems: []service.Item{
					{Role: "user", Type: "message", Text: "Hello <script>alert('xss')</script>"},
				},
				Assistant: []service.Item{
					{Role: "assistant", Type: "message", Text: "Hi there!"},
					{Type: "function_call", ToolName: "search_web", ToolArgs: `{"query":"weather"}`},
					{Type: "function_call_output", ToolOutput: "Sunny, 25°C"},
					{Type: "reasoning", Text: "The user wants the weather."},
				},
				Attachments: []service.Attachment{
					{SHA256: "abc123", MIME: "image/png", Bytes: 4096, ProxyPath: "blobs/abc123"},
				},
				RawInputBytes:  100,
				RawOutputBytes: 200,
			},
		},
		Manifest: service.Manifest{
			ConversationKey: "conv-render-001",
			TurnCount:       1,
			TimeFrom:        time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
			TimeTo:          time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
			Gaps:            []string{"此前历史不在留存范围 (previous_response_id=resp_old)", "部分输出被截断 (output_truncated)"},
		},
	}
}

// ─────────────────────────────────────────────────────
// XSS escaping: user content with <script> must be escaped
// ─────────────────────────────────────────────────────

func TestRenderTranscriptHTML_EscapesXSS(t *testing.T) {
	tr := buildRichTranscript()
	html, err := service.RenderTranscriptHTML(tr)
	if err != nil {
		t.Fatalf("RenderTranscriptHTML error: %v", err)
	}
	output := string(html)

	// The literal <script> tag must NOT appear unescaped.
	if strings.Contains(output, "<script>alert") {
		t.Error("XSS: found unescaped <script>alert in HTML output")
	}
	// The text should be present but HTML-escaped.
	if !strings.Contains(output, "&lt;script&gt;") && !strings.Contains(output, "&#x3C;script&#x3E;") {
		t.Error("expected HTML-escaped <script> tag in output (e.g. &lt;script&gt;)")
	}
}

// ─────────────────────────────────────────────────────
// Manifest card: conversation key, turn count, gaps
// ─────────────────────────────────────────────────────

func TestRenderTranscriptHTML_ManifestCard(t *testing.T) {
	tr := buildRichTranscript()
	html, err := service.RenderTranscriptHTML(tr)
	if err != nil {
		t.Fatalf("RenderTranscriptHTML error: %v", err)
	}
	output := string(html)

	if !strings.Contains(output, "conv-render-001") {
		t.Error("expected conversation key in HTML output")
	}
	if !strings.Contains(output, "此前历史不在留存范围") {
		t.Error("expected first gap text in HTML output")
	}
	if !strings.Contains(output, "部分输出被截断") {
		t.Error("expected second gap text in HTML output")
	}
}

// ─────────────────────────────────────────────────────
// Tool call: tool name appears in output
// ─────────────────────────────────────────────────────

func TestRenderTranscriptHTML_ToolCallPresent(t *testing.T) {
	tr := buildRichTranscript()
	html, err := service.RenderTranscriptHTML(tr)
	if err != nil {
		t.Fatalf("RenderTranscriptHTML error: %v", err)
	}
	output := string(html)

	if !strings.Contains(output, "search_web") {
		t.Error("expected tool name 'search_web' in HTML output")
	}
}

// ─────────────────────────────────────────────────────
// Attachment proxy path present
// ─────────────────────────────────────────────────────

func TestRenderTranscriptHTML_AttachmentProxyPath(t *testing.T) {
	tr := buildRichTranscript()
	html, err := service.RenderTranscriptHTML(tr)
	if err != nil {
		t.Fatalf("RenderTranscriptHTML error: %v", err)
	}
	output := string(html)

	if !strings.Contains(output, "blobs/abc123") {
		t.Error("expected attachment ProxyPath 'blobs/abc123' in HTML output")
	}
}

// ─────────────────────────────────────────────────────
// No external resource references
// ─────────────────────────────────────────────────────

func TestRenderTranscriptHTML_NoExternalResources(t *testing.T) {
	tr := buildRichTranscript()
	html, err := service.RenderTranscriptHTML(tr)
	if err != nil {
		t.Fatalf("RenderTranscriptHTML error: %v", err)
	}
	output := string(html)

	// No http:// or https:// src/href attributes pointing outside.
	// We check for common patterns that would load external resources.
	for _, pat := range []string{
		`src="http://`, `src="https://`,
		`href="http://`, `href="https://`,
		`<script src`,
		`@import url`,
		`<link rel="stylesheet" href="http`,
	} {
		if strings.Contains(output, pat) {
			t.Errorf("found external resource reference %q in HTML output", pat)
		}
	}
}

// ─────────────────────────────────────────────────────
// Self-contained: inline <style> present, no external CSS/JS
// ─────────────────────────────────────────────────────

func TestRenderTranscriptHTML_SelfContained(t *testing.T) {
	tr := buildRichTranscript()
	html, err := service.RenderTranscriptHTML(tr)
	if err != nil {
		t.Fatalf("RenderTranscriptHTML error: %v", err)
	}
	output := string(html)

	if !strings.Contains(output, "<style>") {
		t.Error("expected inline <style> block in self-contained HTML")
	}
}

// ─────────────────────────────────────────────────────
// Empty transcript: renders without error
// ─────────────────────────────────────────────────────

func TestRenderTranscriptHTML_EmptyTranscript(t *testing.T) {
	tr := service.Transcript{}
	html, err := service.RenderTranscriptHTML(tr)
	if err != nil {
		t.Fatalf("RenderTranscriptHTML error on empty transcript: %v", err)
	}
	if len(html) == 0 {
		t.Error("expected non-empty HTML output even for empty transcript")
	}
}

// ─────────────────────────────────────────────────────
// Reasoning block present
// ─────────────────────────────────────────────────────

func TestRenderTranscriptHTML_ReasoningPresent(t *testing.T) {
	tr := buildRichTranscript()
	html, err := service.RenderTranscriptHTML(tr)
	if err != nil {
		t.Fatalf("RenderTranscriptHTML error: %v", err)
	}
	output := string(html)

	if !strings.Contains(output, "The user wants the weather.") {
		t.Error("expected reasoning text in HTML output")
	}
}
