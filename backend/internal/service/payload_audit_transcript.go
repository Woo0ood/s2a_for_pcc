package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// ─────────────────────────────────────────────────────────────────────────────
// Public models
// ─────────────────────────────────────────────────────────────────────────────

// Item represents a single atomic message or tool action within a turn.
type Item struct {
	Role       string // "user", "assistant", "system", "tool", …
	Type       string // "message", "function_call", "function_call_output", "reasoning", "raw"
	Text       string // message text (for message/raw types)
	ToolName   string // populated for function_call
	ToolArgs   string // populated for function_call (JSON string)
	ToolOutput string // populated for function_call_output
}

// Turn holds the parsed content of a single gateway request+response row.
type Turn struct {
	Index     int
	CreatedAt time.Time
	Model     string
	StatusCode int

	// UserItems: only input items that are NEW in this turn (not seen in earlier turns).
	UserItems   []Item
	// Assistant: items parsed from the structured output.
	Assistant   []Item
	// Attachments: blob-pointer attachments resolved from input/output bodies.
	Attachments []Attachment

	RawInputBytes  int
	RawOutputBytes int
}

// Manifest describes the completeness of a Transcript.
type Manifest struct {
	ConversationKey string
	TurnCount       int
	TimeFrom        time.Time
	TimeTo          time.Time
	// Gaps is an explicit list of missing or unrecoverable portions.
	Gaps []string
}

// Transcript is the assembled, turn-by-turn view of a conversation.
type Transcript struct {
	Turns    []Turn
	Manifest Manifest
}

// ─────────────────────────────────────────────────────────────────────────────
// AssembleTranscript
// ─────────────────────────────────────────────────────────────────────────────

// AssembleTranscript converts an ordered slice of PayloadAuditEvents into a
// structured Transcript. Rows MUST be sorted ascending by (created_at, id).
//
// resolver may be nil — if nil, pointer bodies are left as-is.
func AssembleTranscript(ctx context.Context, rows []*PayloadAuditEvent, resolver *BlobResolver) Transcript {
	if len(rows) == 0 {
		return Transcript{}
	}

	// Build a set of all responseIDs present in this slice for chain-gap detection.
	responseIDSet := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.ResponseID != "" {
			responseIDSet[r.ResponseID] = true
		}
	}

	// seenHashes deduplicates input items across turns for both /v1/responses and
	// chat/completions. Items are keyed by itemHash(). The responses path previously
	// used item IDs, but Codex /v1/responses items have NO id field, so content-hash
	// dedup is the only reliable approach for both paths.
	seenHashes := make(map[string]bool)

	var turns []Turn
	var allGaps []string
	outputTruncated := false

	for idx, row := range rows {
		// Resolve pointers in bodies.
		resolvedInput, inputAtts, inputGaps := resolver.ResolveBody(ctx, row.InputBody)
		resolvedOutput, outputAtts, outputGaps := resolver.ResolveBody(ctx, row.OutputBody)

		gaps := append(inputGaps, outputGaps...)

		atts := append(inputAtts, outputAtts...)

		// Parse user items from the resolved input.
		userItems := parseUserItems(row.Endpoint, resolvedInput, seenHashes)

		// Parse assistant items from the resolved output.
		assistantItems := parseAssistantItems(row.OutputFormat, resolvedOutput)

		// Check gap conditions.
		if row.PreviousResponseID != "" && !responseIDSet[row.PreviousResponseID] {
			gaps = append(gaps, fmt.Sprintf(
				"此前历史不在留存范围 (previous_response_id=%s)", row.PreviousResponseID,
			))
		}
		if row.OutputTruncated {
			outputTruncated = true
		}

		turns = append(turns, Turn{
			Index:          idx + 1,
			CreatedAt:      row.CreatedAt,
			Model:          row.Model,
			StatusCode:     row.StatusCode,
			UserItems:      userItems,
			Assistant:      assistantItems,
			Attachments:    atts,
			RawInputBytes:  len(row.InputBody),
			RawOutputBytes: len(row.OutputBody),
		})

		allGaps = append(allGaps, gaps...)
	}

	if outputTruncated {
		allGaps = append(allGaps, "部分输出被截断 (output_truncated)")
	}

	// Dedup gaps preserving order.
	allGaps = dedupStrings(allGaps)

	first := rows[0]
	last := rows[len(rows)-1]

	return Transcript{
		Turns: turns,
		Manifest: Manifest{
			ConversationKey: first.ConversationKey,
			TurnCount:       len(turns),
			TimeFrom:        first.CreatedAt,
			TimeTo:          last.CreatedAt,
			Gaps:            allGaps,
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Input parsing
// ─────────────────────────────────────────────────────────────────────────────

// parseUserItems extracts new input items from a resolved body.
// For /v1/responses: deduplicates by content-hash (item.id is absent on Codex inputs).
// For chat/completions: deduplicates by content-hash.
// Other endpoints: emit a single raw Item.
func parseUserItems(endpoint, body string, seenHashes map[string]bool) []Item {
	switch {
	case strings.Contains(endpoint, "/responses"):
		return parseResponsesInputItems(body, seenHashes)
	case strings.Contains(endpoint, "/chat/completions"):
		return parseChatInputItems(body, seenHashes)
	default:
		return parseRawInputItem(body)
	}
}

// itemHash returns a stable content-based key for deduplication across turns.
// Uses \x00 as a field separator (never appears in UTF-8 text fields).
func itemHash(it Item) string {
	return it.Role + "\x00" + it.Type + "\x00" + it.Text + "\x00" + it.ToolName + "\x00" + it.ToolArgs + "\x00" + it.ToolOutput
}

// parseResponsesInputItems parses the OpenAI Responses API request body.
//
// It handles three cases for the "input" field:
//   - absent / unknown structure → raw fallback
//   - string → single user message
//   - array → iterate items via responseItemToItem
//
// It also reads the top-level "instructions" string, which carries the
// system/developer prompt, and emits it as a system role item.
//
// All items are deduplicated across turns via seenHashes (content-hash).
func parseResponsesInputItems(body string, seenHashes map[string]bool) []Item {
	var items []Item

	emit := func(it Item) {
		h := itemHash(it)
		if seenHashes[h] {
			return
		}
		seenHashes[h] = true
		items = append(items, it)
	}

	// 1. Top-level "instructions" → system prompt.
	if instructions := strings.TrimSpace(gjson.Get(body, "instructions").String()); instructions != "" {
		emit(Item{Role: "system", Type: "message", Text: instructions})
	}

	// 2. "input" field.
	input := gjson.Get(body, "input")
	switch {
	case !input.Exists():
		// No input field at all; return what we have (may just be instructions).
		if len(items) == 0 {
			return parseRawInputItem(body)
		}
	case input.Type == gjson.String:
		// Plain string input (e.g. simple "say hi in one word").
		if s := input.String(); s != "" {
			emit(Item{Role: "user", Type: "message", Text: s})
		}
	case input.IsArray():
		input.ForEach(func(_, elem gjson.Result) bool {
			emit(responseItemToItem(elem))
			return true
		})
	default:
		// Unexpected input shape; raw fallback for that field.
		emit(Item{Type: "raw", Text: input.Raw})
	}

	return items
}

// responseItemToItem maps a single JSON object from `input[]` to an Item.
func responseItemToItem(elem gjson.Result) Item {
	itemType := elem.Get("type").String()
	switch itemType {
	case "message":
		role := elem.Get("role").String()
		if role == "" {
			role = "user"
		}
		text := extractResponsesContentText(elem.Get("content"))
		return Item{Role: role, Type: "message", Text: text}

	case "function_call":
		return Item{
			Type:     "function_call",
			ToolName: elem.Get("name").String(),
			ToolArgs: elem.Get("arguments").String(),
		}

	case "function_call_output":
		return Item{
			Type:       "function_call_output",
			ToolOutput: elem.Get("output").String(),
		}

	case "reasoning":
		summaryArr := elem.Get("summary")
		text := ""
		if summaryArr.IsArray() {
			var parts []string
			summaryArr.ForEach(func(_, s gjson.Result) bool {
				if t := s.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
				return true
			})
			text = strings.Join(parts, " ")
		}
		// If summary was empty but encrypted_content is present, emit a placeholder
		// so the reasoning block is at least visible and honest about the situation.
		if text == "" && elem.Get("encrypted_content").String() != "" {
			text = "(reasoning: encrypted, not retained)"
		}
		return Item{Type: "reasoning", Text: text}

	case "tool_search_call":
		// Best-effort: surface name + any query text.
		name := elem.Get("name").String()
		query := elem.Get("query").String()
		return Item{
			Type:     "tool_search_call",
			ToolName: name,
			Text:     query,
		}

	case "tool_search_output":
		// Best-effort: surface name + output text.
		name := elem.Get("name").String()
		output := elem.Get("output").String()
		return Item{
			Type:       "tool_search_output",
			ToolName:   name,
			ToolOutput: output,
		}

	default:
		// Emit a best-effort raw item.
		return Item{Type: "raw", Text: elem.Raw}
	}
}

// extractResponsesContentText extracts text from `content` which may be:
//   - a string
//   - an array of content blocks {type, text}
func extractResponsesContentText(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		var parts []string
		content.ForEach(func(_, block gjson.Result) bool {
			if t := block.Get("text").String(); t != "" {
				parts = append(parts, t)
			}
			return true
		})
		return strings.Join(parts, "")
	}
	return content.Raw
}

// parseChatInputItems parses `messages[]` from a chat/completions body.
// Chat requests re-send full history each turn; to avoid repeating already-shown
// messages, we use itemHash (same hash function as the responses path) keyed
// against the shared seenHashes map.
func parseChatInputItems(body string, seenHashes map[string]bool) []Item {
	messagesArr := gjson.Get(body, "messages")
	if !messagesArr.Exists() || !messagesArr.IsArray() {
		return parseRawInputItem(body)
	}

	var items []Item
	messagesArr.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := extractChatContent(msg.Get("content"))
		it := Item{Role: role, Type: "message", Text: content}
		h := itemHash(it)
		if seenHashes[h] {
			return true // already shown in a previous turn
		}
		seenHashes[h] = true
		items = append(items, it)
		return true
	})
	return items
}

// extractChatContent handles content that is a string or array of blocks.
func extractChatContent(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		var parts []string
		content.ForEach(func(_, block gjson.Result) bool {
			if t := block.Get("text").String(); t != "" {
				parts = append(parts, t)
			}
			return true
		})
		return strings.Join(parts, "")
	}
	return content.Raw
}

// parseRawInputItem returns a single raw fallback Item.
func parseRawInputItem(body string) []Item {
	truncated := body
	const maxRaw = 4096
	if len(truncated) > maxRaw {
		truncated = truncated[:maxRaw] + "…[truncated]"
	}
	return []Item{{Type: "raw", Text: truncated}}
}

// ─────────────────────────────────────────────────────────────────────────────
// Output parsing
// ─────────────────────────────────────────────────────────────────────────────

// parseAssistantItems converts a stored output body to structured assistant Items.
// outputFormat is "sse", "json", or "text" (from repository.PayloadAuditEvent.OutputFormat).
func parseAssistantItems(outputFormat, body string) []Item {
	switch outputFormat {
	case "sse":
		return parseSSEOutput(body)
	case "json":
		return parseJSONOutput(body)
	default:
		// "text" or unknown: emit as a single assistant message.
		if strings.TrimSpace(body) == "" {
			return nil
		}
		return []Item{{Role: "assistant", Type: "message", Text: body}}
	}
}

// parseJSONOutput handles both chat completions and responses API JSON outputs.
func parseJSONOutput(body string) []Item {
	// Try OpenAI Responses format: {"output":[...]}
	outputArr := gjson.Get(body, "output")
	if outputArr.Exists() && outputArr.IsArray() {
		return parseResponsesOutputItems(outputArr)
	}

	// Try chat completions format: {"choices":[{"message":{...}}]}
	choicesArr := gjson.Get(body, "choices")
	if choicesArr.Exists() && choicesArr.IsArray() {
		return parseChatChoices(choicesArr)
	}

	// Fallback: raw.
	return []Item{{Role: "assistant", Type: "raw", Text: body}}
}

// parseResponsesOutputItems parses the `output[]` array from Responses API JSON.
func parseResponsesOutputItems(arr gjson.Result) []Item {
	var items []Item
	arr.ForEach(func(_, elem gjson.Result) bool {
		t := elem.Get("type").String()
		switch t {
		case "message":
			text := extractResponsesContentText(elem.Get("content"))
			items = append(items, Item{Role: "assistant", Type: "message", Text: text})
		case "function_call":
			items = append(items, Item{
				Type:     "function_call",
				ToolName: elem.Get("name").String(),
				ToolArgs: elem.Get("arguments").String(),
			})
		case "reasoning":
			text := ""
			summaryArr := elem.Get("summary")
			if summaryArr.IsArray() {
				var parts []string
				summaryArr.ForEach(func(_, s gjson.Result) bool {
					if tx := s.Get("text").String(); tx != "" {
						parts = append(parts, tx)
					}
					return true
				})
				text = strings.Join(parts, " ")
			}
			items = append(items, Item{Type: "reasoning", Text: text})
		default:
			items = append(items, Item{Role: "assistant", Type: "raw", Text: elem.Raw})
		}
		return true
	})
	return items
}

// parseChatChoices extracts assistant message + tool_calls from chat choices.
func parseChatChoices(arr gjson.Result) []Item {
	var items []Item
	arr.ForEach(func(_, choice gjson.Result) bool {
		msg := choice.Get("message")
		if !msg.Exists() {
			// streaming delta
			msg = choice.Get("delta")
		}
		if !msg.Exists() {
			return true
		}
		content := msg.Get("content").String()
		if content != "" {
			items = append(items, Item{Role: "assistant", Type: "message", Text: content})
		}
		// tool_calls
		toolCalls := msg.Get("tool_calls")
		if toolCalls.IsArray() {
			toolCalls.ForEach(func(_, tc gjson.Result) bool {
				name := tc.Get("function.name").String()
				args := tc.Get("function.arguments").String()
				items = append(items, Item{
					Type:     "function_call",
					ToolName: name,
					ToolArgs: args,
				})
				return true
			})
		}
		return true
	})
	return items
}

// parseSSEOutput processes an SSE event stream and extracts structured Items.
// It accumulates deltas and also collects function_call output items.
func parseSSEOutput(body string) []Item {
	// Accumulate text and tool calls from SSE events.
	var textBuilder strings.Builder
	var toolCalls []Item    // function_call items accumulated
	var reasonBuilder strings.Builder

	forEachSSEEvent([]byte(body), func(eventType string, data []byte) {
		switch eventType {
		case "response.output_text.delta", "response.output_text.done":
			var d struct {
				Delta string `json:"delta"`
				Text  string `json:"text"`
			}
			if json.Unmarshal(data, &d) == nil {
				if d.Delta != "" {
					textBuilder.WriteString(d.Delta)
				}
			}

		case "response.output_item.added", "response.output_item.done":
			// Captures function_call items added as output items.
			var d struct {
				Item struct {
					Type      string `json:"type"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
					ID        string `json:"id"`
				} `json:"item"`
			}
			if json.Unmarshal(data, &d) == nil && d.Item.Type == "function_call" {
				toolCalls = append(toolCalls, Item{
					Type:     "function_call",
					ToolName: d.Item.Name,
					ToolArgs: d.Item.Arguments,
				})
			}

		case "response.function_call_arguments.done":
			// Captures complete function call arguments.
			var d struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				CallID    string `json:"call_id"`
			}
			if json.Unmarshal(data, &d) == nil && d.Name != "" {
				toolCalls = append(toolCalls, Item{
					Type:     "function_call",
					ToolName: d.Name,
					ToolArgs: d.Arguments,
				})
			}

		case "response.reasoning_summary.delta", "response.reasoning.delta":
			var d struct{ Delta string `json:"delta"` }
			if json.Unmarshal(data, &d) == nil {
				reasonBuilder.WriteString(d.Delta)
			}

		// Chat completions SSE deltas.
		default:
			// Try chat-style delta.
			if len(data) > 0 {
				var chatChunk struct {
					Choices []struct {
						Delta struct {
							Content   string `json:"content"`
							ToolCalls []struct {
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal(data, &chatChunk) == nil {
					for _, c := range chatChunk.Choices {
						textBuilder.WriteString(c.Delta.Content)
						for _, tc := range c.Delta.ToolCalls {
							if tc.Function.Name != "" {
								toolCalls = append(toolCalls, Item{
									Type:     "function_call",
									ToolName: tc.Function.Name,
									ToolArgs: tc.Function.Arguments,
								})
							}
						}
					}
				}
			}
		}
	})

	// Also scan data-only SSE lines (for chat completions without event: lines).
	// We do a second pass treating it as data-only if textBuilder is still empty.
	if textBuilder.Len() == 0 && len(toolCalls) == 0 {
		forEachSSEData([]byte(body), func(data []byte) {
			var chatChunk struct {
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						ToolCalls []struct {
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal(data, &chatChunk) == nil {
				for _, c := range chatChunk.Choices {
					textBuilder.WriteString(c.Delta.Content)
				}
			}
		})
	}

	var items []Item
	if text := textBuilder.String(); text != "" {
		items = append(items, Item{Role: "assistant", Type: "message", Text: text})
	}
	items = append(items, dedupToolCalls(toolCalls)...)
	if reason := reasonBuilder.String(); reason != "" {
		items = append(items, Item{Type: "reasoning", Text: reason})
	}
	return items
}

// dedupToolCalls removes duplicate function_call items (same name+args).
func dedupToolCalls(calls []Item) []Item {
	seen := make(map[string]bool, len(calls))
	var out []Item
	for _, c := range calls {
		key := c.ToolName + "\x00" + c.ToolArgs
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Utilities
// ─────────────────────────────────────────────────────────────────────────────

// dedupStrings preserves order and removes exact duplicates.
func dedupStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

