package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ─────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────

func makePARow(t *testing.T, idx int, endpoint, inputBody, outputBody, outputFormat,
	responseID, prevResponseID, convKey string, outputTruncated bool) *service.PayloadAuditEvent {
	t.Helper()
	return &service.PayloadAuditEvent{
		ID:                  int64(idx),
		Endpoint:            endpoint,
		InputBody:           inputBody,
		OutputBody:          outputBody,
		OutputFormat:        outputFormat,
		ResponseID:          responseID,
		PreviousResponseID:  prevResponseID,
		ConversationKey:     convKey,
		OutputTruncated:     outputTruncated,
		Model:               "gpt-4o",
		StatusCode:          200,
		CreatedAt:           time.Now().Add(time.Duration(idx) * time.Minute),
	}
}

// ─────────────────────────────────────────────────────
// /v1/responses multi-turn: item-id dedup
// ─────────────────────────────────────────────────────

// Turn 1: user asks question (item id="msg_001").
// Turn 2: adds function_call_output (id="fc_out_001") + repeats "msg_001" in context.
// Expect: Turn 2 UserItems only contains fc_out_001, not a duplicate of msg_001.
func TestAssembleTranscript_ResponsesEndpoint_DedupsSeenItemIDs(t *testing.T) {
	convKey := "conv-test-001"

	turn1Input := `{"model":"gpt-4o","input":[{"type":"message","role":"user","id":"msg_001","content":[{"type":"input_text","text":"Hello, what is 2+2?"}]}]}`
	turn1Output := `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"2+2 is 4."}]}]}`

	// Turn 2 repeats msg_001 in context and adds a function_call_output
	turn2Input := `{"model":"gpt-4o","input":[` +
		`{"type":"message","role":"user","id":"msg_001","content":[{"type":"input_text","text":"Hello, what is 2+2?"}]},` +
		`{"type":"function_call_output","id":"fc_out_001","call_id":"call_abc","output":"4"}` +
		`]}`
	turn2Output := `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The answer is 4."}]}]}`

	rows := []*service.PayloadAuditEvent{
		makePARow(t, 1, "/v1/responses", turn1Input, turn1Output, "json", "resp_001", "", convKey, false),
		makePARow(t, 2, "/v1/responses", turn2Input, turn2Output, "json", "resp_002", "resp_001", convKey, false),
	}

	tr := service.AssembleTranscript(context.Background(), rows, nil)

	if len(tr.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(tr.Turns))
	}

	turn1 := tr.Turns[0]
	if len(turn1.UserItems) != 1 {
		t.Fatalf("turn 1: expected 1 user item, got %d", len(turn1.UserItems))
	}
	if turn1.UserItems[0].Text != "Hello, what is 2+2?" {
		t.Errorf("turn 1 user text = %q, want 'Hello, what is 2+2?'", turn1.UserItems[0].Text)
	}

	turn2 := tr.Turns[1]
	// msg_001 already seen — only fc_out_001 is new
	if len(turn2.UserItems) != 1 {
		t.Fatalf("turn 2: expected 1 NEW user item (fc_out_001), got %d: %+v", len(turn2.UserItems), turn2.UserItems)
	}
	if turn2.UserItems[0].ToolOutput != "4" {
		t.Errorf("turn 2 tool output = %q, want '4'", turn2.UserItems[0].ToolOutput)
	}

	// manifest
	if tr.Manifest.TurnCount != 2 {
		t.Errorf("manifest TurnCount = %d, want 2", tr.Manifest.TurnCount)
	}
	if tr.Manifest.ConversationKey != convKey {
		t.Errorf("manifest ConversationKey = %q, want %q", tr.Manifest.ConversationKey, convKey)
	}
}

// ─────────────────────────────────────────────────────
// function_call in output
// ─────────────────────────────────────────────────────

func TestAssembleTranscript_FunctionCallInOutput(t *testing.T) {
	convKey := "conv-fc-002"

	turn1Input := `{"model":"gpt-4o","input":[{"type":"message","role":"user","id":"msg_u1","content":[{"type":"input_text","text":"What's the weather?"}]}]}`
	// output contains a function_call item
	turn1Output := `{"output":[{"type":"function_call","id":"fc_call_1","name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}]}`

	rows := []*service.PayloadAuditEvent{
		makePARow(t, 1, "/v1/responses", turn1Input, turn1Output, "json", "resp_001", "", convKey, false),
	}

	tr := service.AssembleTranscript(context.Background(), rows, nil)

	if len(tr.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(tr.Turns))
	}
	turn := tr.Turns[0]

	// Assistant items should contain the tool call
	if len(turn.Assistant) == 0 {
		t.Fatal("expected assistant items (function_call), got none")
	}
	var foundToolCall bool
	for _, item := range turn.Assistant {
		if item.ToolName == "get_weather" {
			foundToolCall = true
			if !strings.Contains(item.ToolArgs, "Beijing") {
				t.Errorf("tool args = %q, expected to contain 'Beijing'", item.ToolArgs)
			}
		}
	}
	if !foundToolCall {
		t.Errorf("no function_call item found in assistant items: %+v", turn.Assistant)
	}
}

// ─────────────────────────────────────────────────────
// chain-break gap: previous_response_id not in the set
// ─────────────────────────────────────────────────────

func TestAssembleTranscript_ChainBreakGap(t *testing.T) {
	convKey := "conv-chain-003"

	input := `{"model":"gpt-4o","input":[{"type":"message","role":"user","id":"msg_u1","content":[{"type":"input_text","text":"continue"}]}]}`
	output := `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Sure."}]}]}`

	// previous_response_id = "resp_999" but no row has ResponseID = "resp_999"
	rows := []*service.PayloadAuditEvent{
		makePARow(t, 1, "/v1/responses", input, output, "json", "resp_100", "resp_999", convKey, false),
	}

	tr := service.AssembleTranscript(context.Background(), rows, nil)

	var foundChainGap bool
	for _, g := range tr.Manifest.Gaps {
		if strings.Contains(g, "previous_response_id") && strings.Contains(g, "resp_999") {
			foundChainGap = true
		}
	}
	if !foundChainGap {
		t.Errorf("expected chain-break gap in manifest, got: %v", tr.Manifest.Gaps)
	}
}

// ─────────────────────────────────────────────────────
// output_truncated gap
// ─────────────────────────────────────────────────────

func TestAssembleTranscript_OutputTruncatedGap(t *testing.T) {
	convKey := "conv-trunc-004"

	input := `{"model":"gpt-4o","input":[{"type":"message","role":"user","id":"msg_u1","content":[{"type":"input_text","text":"write a lot"}]}]}`
	output := `text output that got cut`

	rows := []*service.PayloadAuditEvent{
		makePARow(t, 1, "/v1/responses", input, output, "text", "resp_200", "", convKey, true),
	}

	tr := service.AssembleTranscript(context.Background(), rows, nil)

	var foundTruncGap bool
	for _, g := range tr.Manifest.Gaps {
		if strings.Contains(g, "截断") || strings.Contains(g, "truncat") {
			foundTruncGap = true
		}
	}
	if !foundTruncGap {
		t.Errorf("expected output-truncated gap in manifest, got: %v", tr.Manifest.Gaps)
	}
}

// ─────────────────────────────────────────────────────
// chat/completions: last-message dedup by role+content hash
// ─────────────────────────────────────────────────────

func TestAssembleTranscript_ChatCompletions_DedupsRepeatMessages(t *testing.T) {
	convKey := "conv-chat-005"

	// Turn 1: messages=[user:hi]
	turn1Input := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	turn1Output := `{"choices":[{"message":{"role":"assistant","content":"Hello!"}}]}`

	// Turn 2: messages=[user:hi, assistant:Hello!, user:tell me more]
	// The first two messages are repeats; only the last is new.
	turn2Input := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"Hello!"},{"role":"user","content":"tell me more"}]}`
	turn2Output := `{"choices":[{"message":{"role":"assistant","content":"Sure, here's more."}}]}`

	rows := []*service.PayloadAuditEvent{
		makePARow(t, 1, "/v1/chat/completions", turn1Input, turn1Output, "json", "", "", convKey, false),
		makePARow(t, 2, "/v1/chat/completions", turn2Input, turn2Output, "json", "", "", convKey, false),
	}

	tr := service.AssembleTranscript(context.Background(), rows, nil)

	if len(tr.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(tr.Turns))
	}

	turn2 := tr.Turns[1]
	// Only new messages not seen in turn 1 should be in UserItems for turn 2.
	// Turn 1 emitted "hi" (user) → seenHash["user\x00hi"] = true.
	// Turn 2 messages: "hi" (dup), "Hello!" (assistant — new hash), "tell me more" (user — new hash).
	// So we expect 2 new items: assistant "Hello!" and user "tell me more".
	if len(turn2.UserItems) < 1 {
		t.Fatalf("turn 2: expected new user items, got %d: %+v", len(turn2.UserItems), turn2.UserItems)
	}
	var foundNew bool
	for _, item := range turn2.UserItems {
		if item.Text == "tell me more" {
			foundNew = true
		}
	}
	if !foundNew {
		t.Errorf("turn 2: expected 'tell me more' among new items, got: %+v", turn2.UserItems)
	}
}

// ─────────────────────────────────────────────────────
// SSE output format: extract assistant text
// ─────────────────────────────────────────────────────

func TestAssembleTranscript_SSEOutput_ExtractsText(t *testing.T) {
	convKey := "conv-sse-006"

	input := `{"model":"gpt-4o","input":[{"type":"message","role":"user","id":"msg_u1","content":[{"type":"input_text","text":"ping"}]}]}`
	sseOutput := "event: response.output_text.delta\ndata: {\"delta\":\"pong\"}\n\nevent: response.output_text.done\ndata: {\"text\":\"pong\"}\n\n"

	rows := []*service.PayloadAuditEvent{
		makePARow(t, 1, "/v1/responses", input, sseOutput, "sse", "resp_300", "", convKey, false),
	}

	tr := service.AssembleTranscript(context.Background(), rows, nil)

	if len(tr.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(tr.Turns))
	}
	if len(tr.Turns[0].Assistant) == 0 {
		t.Fatal("expected assistant items from SSE output, got none")
	}
	var text string
	for _, item := range tr.Turns[0].Assistant {
		text += item.Text
	}
	if !strings.Contains(text, "pong") {
		t.Errorf("expected 'pong' in assistant text, got %q", text)
	}
}

// ─────────────────────────────────────────────────────
// RawInputBytes / RawOutputBytes are set
// ─────────────────────────────────────────────────────

func TestAssembleTranscript_RawBytesSet(t *testing.T) {
	convKey := "conv-bytes-007"
	input := `{"model":"gpt-4o","input":[{"type":"message","role":"user","id":"msg_u1","content":[{"type":"input_text","text":"hi"}]}]}`
	output := `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`

	rows := []*service.PayloadAuditEvent{
		makePARow(t, 1, "/v1/responses", input, output, "json", "resp_400", "", convKey, false),
	}

	tr := service.AssembleTranscript(context.Background(), rows, nil)

	if tr.Turns[0].RawInputBytes != len(input) {
		t.Errorf("RawInputBytes = %d, want %d", tr.Turns[0].RawInputBytes, len(input))
	}
	if tr.Turns[0].RawOutputBytes != len(output) {
		t.Errorf("RawOutputBytes = %d, want %d", tr.Turns[0].RawOutputBytes, len(output))
	}
}

// ─────────────────────────────────────────────────────
// Nil rows / empty input
// ─────────────────────────────────────────────────────

func TestAssembleTranscript_EmptyRows(t *testing.T) {
	tr := service.AssembleTranscript(context.Background(), nil, nil)
	if len(tr.Turns) != 0 {
		t.Errorf("expected 0 turns, got %d", len(tr.Turns))
	}
	if tr.Manifest.TurnCount != 0 {
		t.Errorf("expected TurnCount=0, got %d", tr.Manifest.TurnCount)
	}
}
