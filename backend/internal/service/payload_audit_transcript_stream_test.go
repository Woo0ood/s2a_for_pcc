package service_test

// Tests for TranscriptStreamer and StreamingAssembler.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// makeStreamRow builds a minimal PayloadAuditEvent for streaming tests.
func makeStreamRow(t *testing.T, idx int, endpoint, inputBody, outputBody, outputFormat,
	responseID, prevResponseID, convKey string) *service.PayloadAuditEvent {
	t.Helper()
	return &service.PayloadAuditEvent{
		ID:                 int64(idx),
		Endpoint:           endpoint,
		InputBody:          inputBody,
		OutputBody:         outputBody,
		OutputFormat:       outputFormat,
		ResponseID:         responseID,
		PreviousResponseID: prevResponseID,
		ConversationKey:    convKey,
		Model:              "gpt-4o",
		StatusCode:         200,
		CreatedAt:          time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC).Add(time.Duration(idx) * time.Minute),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestTranscriptStreamer_ThreeTurns: head + 3 articles + bottom manifest
// ─────────────────────────────────────────────────────────────────────────────

func TestTranscriptStreamer_ThreeTurns(t *testing.T) {
	convKey := "stream-conv-001"

	rows := []*service.PayloadAuditEvent{
		makeStreamRow(t, 1, "/v1/responses",
			`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello from turn 1"}]}]}`,
			`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Reply 1"}]}]}`,
			"json", "resp_s001", "", convKey),
		makeStreamRow(t, 2, "/v1/responses",
			`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello from turn 2"}]}]}`,
			`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Reply 2"}]}]}`,
			"json", "resp_s002", "resp_s001", convKey),
		makeStreamRow(t, 3, "/v1/responses",
			`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello from turn 3"}]}]}`,
			`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Reply 3"}]}]}`,
			"json", "resp_s003", "resp_s002", convKey),
	}

	// Build responseIDSet (all response_ids in this export).
	respIDs := map[string]bool{
		"resp_s001": true, "resp_s002": true, "resp_s003": true,
	}

	var buf bytes.Buffer
	streamer, err := service.NewTranscriptStreamer(&buf, convKey)
	if err != nil {
		t.Fatalf("NewTranscriptStreamer: %v", err)
	}

	assembler := service.NewStreamingAssembler(nil, respIDs, false)
	for _, row := range rows {
		turn, gaps := assembler.Next(context.Background(), row)
		if err := streamer.WriteTurn(turn); err != nil {
			t.Fatalf("WriteTurn: %v", err)
		}
		streamer.AddGaps(gaps...)
	}
	if err := streamer.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	html := buf.String()

	// Head: DOCTYPE + style present.
	if !strings.Contains(html, "<!DOCTYPE html>") {
		t.Error("expected <!DOCTYPE html> in streaming output")
	}
	if !strings.Contains(html, "<style>") {
		t.Error("expected <style> block in streaming output")
	}

	// All 3 turns' content present.
	for i := 1; i <= 3; i++ {
		want := "Hello from turn " + string(rune('0'+i))
		if !strings.Contains(html, want) {
			t.Errorf("turn %d: expected %q in streaming HTML", i, want)
		}
		want2 := "Reply " + string(rune('0'+i))
		if !strings.Contains(html, want2) {
			t.Errorf("turn %d: expected %q in streaming HTML", i, want2)
		}
	}

	// Bottom manifest: TurnCount=3 and conversation key.
	if !strings.Contains(html, convKey) {
		t.Error("expected conversation key in bottom manifest")
	}
	// The manifest must appear AFTER the last turn article.
	lastTurnPos := strings.LastIndex(html, "<article class=\"turn\">")
	manifestPos := strings.LastIndex(html, "<section class=\"manifest\">")
	if manifestPos == -1 {
		t.Fatal("no manifest section found in streaming output")
	}
	if manifestPos < lastTurnPos {
		t.Error("manifest must appear AFTER the last turn article in streaming output")
	}

	// Time range is in the manifest.
	if !strings.Contains(html, "2026-06-15") {
		t.Error("expected date 2026-06-15 in manifest time fields")
	}

	// Footer present.
	if !strings.Contains(html, "</body></html>") {
		t.Error("expected </body></html> in streaming output")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestTranscriptStreamer_GapAddedViaAddGaps
// ─────────────────────────────────────────────────────────────────────────────

func TestTranscriptStreamer_GapAddedViaAddGaps(t *testing.T) {
	var buf bytes.Buffer
	streamer, err := service.NewTranscriptStreamer(&buf, "gap-test-conv")
	if err != nil {
		t.Fatalf("NewTranscriptStreamer: %v", err)
	}

	assembler := service.NewStreamingAssembler(nil, nil, false)
	row := makeStreamRow(t, 1, "/v1/responses",
		`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}]}`,
		`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}]}`,
		"json", "resp_g001", "resp_MISSING", "gap-test-conv")

	turn, gaps := assembler.Next(context.Background(), row)
	_ = streamer.WriteTurn(turn)
	streamer.AddGaps(gaps...)
	// Add a manual extra gap.
	streamer.AddGaps("manual test gap for streaming")
	// Deduplicate: adding the same gap twice should appear only once.
	streamer.AddGaps("manual test gap for streaming")

	if err := streamer.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	html := buf.String()

	// Chain-break gap (previous_response_id not in respIDs).
	if !strings.Contains(html, "previous_response_id") {
		t.Error("expected chain-break gap in streaming HTML")
	}

	// Manual gap present.
	if !strings.Contains(html, "manual test gap for streaming") {
		t.Error("expected manual gap in streaming HTML")
	}

	// Dedup: the manual gap should appear only once.
	count := strings.Count(html, "manual test gap for streaming")
	if count != 1 {
		t.Errorf("expected manual gap to appear exactly once, got %d", count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestTranscriptStreamer_EmptyNoTurns
// ─────────────────────────────────────────────────────────────────────────────

func TestTranscriptStreamer_EmptyNoTurns(t *testing.T) {
	var buf bytes.Buffer
	streamer, err := service.NewTranscriptStreamer(&buf, "empty-stream")
	if err != nil {
		t.Fatalf("NewTranscriptStreamer: %v", err)
	}
	if err := streamer.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "No turns in this conversation.") {
		t.Error("expected empty-state paragraph when no turns written")
	}
	if !strings.Contains(html, "</body></html>") {
		t.Error("expected footer in empty-turn streaming output")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestTranscriptStreamer_CrossTurnDedup
// Mirror of TestAssembleTranscript_ResponsesContentHashDedup but via streamer.
// Turn 1: instructions + user msgA.
// Turn 2: same instructions + msgA (repeats) + new msgB.
// Expected: turn 2 shows ONLY msgB (instructions and msgA already hashed).
// ─────────────────────────────────────────────────────────────────────────────

func TestTranscriptStreamer_CrossTurnDedup(t *testing.T) {
	convKey := "stream-dedup-002"
	instructions := "You are Codex, based on GPT-5."
	msgA := "What is the capital of France?"
	msgB := "Now tell me about Go generics."

	makeBody := func(extra string) string {
		base := `{"model":"codex-mini-latest","instructions":"` + instructions + `","input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + msgA + `"}]}`
		if extra != "" {
			base += `,` + extra
		}
		base += `]}`
		return base
	}

	turn1Body := makeBody("")
	turn2Body := makeBody(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + msgB + `"}]}`)
	outputBody := `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`

	rows := []*service.PayloadAuditEvent{
		makeStreamRow(t, 1, "/v1/responses", turn1Body, outputBody, "json", "resp_dd1", "", convKey),
		makeStreamRow(t, 2, "/v1/responses", turn2Body, outputBody, "json", "resp_dd2", "resp_dd1", convKey),
	}

	respIDs := map[string]bool{"resp_dd1": true, "resp_dd2": true}

	var buf bytes.Buffer
	streamer, err := service.NewTranscriptStreamer(&buf, convKey)
	if err != nil {
		t.Fatalf("NewTranscriptStreamer: %v", err)
	}

	assembler := service.NewStreamingAssembler(nil, respIDs, false)

	// Collect per-turn items for verification.
	var turnItems [][]service.Item
	for _, row := range rows {
		turn, gaps := assembler.Next(context.Background(), row)
		turnItems = append(turnItems, turn.UserItems)
		_ = streamer.WriteTurn(turn)
		streamer.AddGaps(gaps...)
	}
	_ = streamer.Finish()

	// Turn 1: system (instructions) + user (msgA) = 2 items.
	if len(turnItems[0]) != 2 {
		t.Fatalf("turn 1: expected 2 UserItems, got %d: %+v", len(turnItems[0]), turnItems[0])
	}
	if turnItems[0][0].Role != "system" {
		t.Errorf("turn 1 item[0].Role = %q, want 'system'", turnItems[0][0].Role)
	}
	if turnItems[0][1].Text != msgA {
		t.Errorf("turn 1 item[1].Text = %q, want %q", turnItems[0][1].Text, msgA)
	}

	// Turn 2: only msgB is new.
	if len(turnItems[1]) != 1 {
		t.Fatalf("turn 2: expected 1 new UserItem (msgB only), got %d: %+v", len(turnItems[1]), turnItems[1])
	}
	if turnItems[1][0].Text != msgB {
		t.Errorf("turn 2 item[0].Text = %q, want %q", turnItems[1][0].Text, msgB)
	}

	// The HTML output must contain msgA once and msgB once (msgA not repeated in turn 2).
	html := buf.String()
	if strings.Count(html, msgA) != 1 {
		t.Errorf("expected msgA to appear exactly once in HTML (dedup), got %d", strings.Count(html, msgA))
	}
	if !strings.Contains(html, msgB) {
		t.Error("expected msgB in HTML output")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestTranscriptStreamer_ManifestTurnCount
// ─────────────────────────────────────────────────────────────────────────────

func TestTranscriptStreamer_ManifestTurnCount(t *testing.T) {
	var buf bytes.Buffer
	streamer, err := service.NewTranscriptStreamer(&buf, "count-test")
	if err != nil {
		t.Fatalf("NewTranscriptStreamer: %v", err)
	}

	assembler := service.NewStreamingAssembler(nil, nil, false)
	for i := 1; i <= 3; i++ {
		row := makeStreamRow(t, i, "/v1/responses",
			`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
			"json", "", "", "count-test")
		turn, _ := assembler.Next(context.Background(), row)
		_ = streamer.WriteTurn(turn)
	}
	_ = streamer.Finish()

	html := buf.String()
	// Manifest must show TurnCount = 3.
	// The template renders it as <dd>3</dd> within the Turn Count section.
	if !strings.Contains(html, "<dd>3</dd>") {
		t.Errorf("expected <dd>3</dd> for TurnCount in manifest, html snippet:\n%s",
			extractManifest(html))
	}
}

// extractManifest returns just the manifest section from an HTML string (for error messages).
func extractManifest(html string) string {
	start := strings.Index(html, `<section class="manifest">`)
	end := strings.Index(html, "</section>")
	if start == -1 || end == -1 || end < start {
		return "(manifest not found)"
	}
	return html[start : end+len("</section>")]
}
