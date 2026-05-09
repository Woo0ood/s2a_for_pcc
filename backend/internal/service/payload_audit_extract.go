package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ──────────────────────────────────────────────────────
// Input extraction
// ──────────────────────────────────────────────────────

// InputExtractor extracts human-readable text and format from a request body.
type InputExtractor func(body []byte) (text string, format string)

// ExtractInputText dispatches to the correct extractor based on provider/endpoint.
func ExtractInputText(provider, endpoint string, body []byte) (string, string) {
	switch provider {
	case "openai":
		switch {
		case strings.Contains(endpoint, "/chat/completions"):
			return ExtractOpenAIChatInput(body)
		case strings.Contains(endpoint, "/responses"):
			return ExtractOpenAIResponsesInput(body)
		case strings.Contains(endpoint, "/images"):
			return ExtractOpenAIImagesInput(body)
		}
	case "anthropic":
		return ExtractAnthropicInput(body)
	case "gemini":
		return ExtractGeminiInput(body)
	}
	return fallbackInput(body), "raw"
}

// ExtractOpenAIChatInput extracts [role] content lines from messages[].
func ExtractOpenAIChatInput(body []byte) (string, string) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return fallbackInput(body), "raw"
	}
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString("[")
		b.WriteString(m.Role)
		b.WriteString("] ")
		b.WriteString(extractContentField(m.Content))
		b.WriteString("\n")
	}
	return b.String(), "json"
}

// ExtractOpenAIResponsesInput handles the input field which can be string or array.
func ExtractOpenAIResponsesInput(body []byte) (string, string) {
	var raw struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.Input) == 0 {
		return fallbackInput(body), "raw"
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw.Input, &s); err == nil {
		return s, "json"
	}
	// Try array of {role, content}
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw.Input, &msgs); err == nil && len(msgs) > 0 {
		var b strings.Builder
		for _, m := range msgs {
			b.WriteString("[")
			b.WriteString(m.Role)
			b.WriteString("] ")
			b.WriteString(extractContentField(m.Content))
			b.WriteString("\n")
		}
		return b.String(), "json"
	}
	return fallbackInput(body), "raw"
}

// ExtractAnthropicInput extracts system + messages[].
func ExtractAnthropicInput(body []byte) (string, string) {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fallbackInput(body), "raw"
	}
	if len(req.Messages) == 0 && len(req.System) == 0 {
		return fallbackInput(body), "raw"
	}
	var b strings.Builder
	if len(req.System) > 0 {
		b.WriteString("[system] ")
		b.WriteString(extractContentField(req.System))
		b.WriteString("\n")
	}
	for _, m := range req.Messages {
		b.WriteString("[")
		b.WriteString(m.Role)
		b.WriteString("] ")
		b.WriteString(extractContentField(m.Content))
		b.WriteString("\n")
	}
	return b.String(), "json"
}

// ExtractGeminiInput extracts contents[].parts[].text.
func ExtractGeminiInput(body []byte) (string, string) {
	var req struct {
		Contents []struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Contents) == 0 {
		return fallbackInput(body), "raw"
	}
	var b strings.Builder
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.Text != "" {
				b.WriteString(p.Text)
				b.WriteString("\n")
			}
		}
	}
	if b.Len() == 0 {
		return fallbackInput(body), "raw"
	}
	return b.String(), "json"
}

// ExtractOpenAIImagesInput extracts the prompt field.
func ExtractOpenAIImagesInput(body []byte) (string, string) {
	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Prompt == "" {
		return fallbackInput(body), "raw"
	}
	return req.Prompt, "json"
}

// extractContentField handles content that is either a string or an array of
// content blocks (with "text" field).
func extractContentField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

func fallbackInput(body []byte) string {
	s := string(body)
	if utf8.ValidString(s) {
		return s
	}
	// Truncate to valid UTF-8
	valid := make([]byte, 0, len(body))
	for len(body) > 0 {
		r, size := utf8.DecodeRune(body)
		if r == utf8.RuneError && size <= 1 {
			body = body[1:]
			continue
		}
		valid = append(valid, body[:size]...)
		body = body[size:]
	}
	return string(valid)
}

// ──────────────────────────────────────────────────────
// Stream output extraction (full raw bytes → text)
// ──────────────────────────────────────────────────────

// ExtractOpenAIChatStream processes a complete SSE stream of OpenAI Chat format.
func ExtractOpenAIChatStream(raw []byte) string {
	var b strings.Builder
	forEachSSEData(raw, func(data []byte) {
		b.WriteString(ExtractOpenAIChatDeltaText(data))
	})
	return b.String()
}

// ExtractOpenAIResponsesStream processes SSE with event:/data: pairs.
func ExtractOpenAIResponsesStream(raw []byte) string {
	var b strings.Builder
	forEachSSEEvent(raw, func(eventType string, data []byte) {
		b.WriteString(ExtractOpenAIResponsesEventText(eventType, data))
	})
	return b.String()
}

// ExtractAnthropicStream processes Anthropic SSE stream.
func ExtractAnthropicStream(raw []byte) string {
	var b strings.Builder
	forEachSSEEvent(raw, func(eventType string, data []byte) {
		b.WriteString(ExtractAnthropicEventText(eventType, data))
	})
	return b.String()
}

// ExtractGeminiStream processes Gemini stream (SSE or JSON array).
func ExtractGeminiStream(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return extractGeminiJSONArray(trimmed)
	}
	var b strings.Builder
	forEachSSEData(raw, func(data []byte) {
		b.WriteString(ExtractGeminiStreamFrame(data))
	})
	return b.String()
}

// ──────────────────────────────────────────────────────
// Per-chunk extractors
// ──────────────────────────────────────────────────────

// ExtractOpenAIChatDeltaText extracts text from a single data: line payload.
func ExtractOpenAIChatDeltaText(payload []byte) string {
	var obj struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Refusal          string `json:"refusal"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int `json:"index"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return ""
	}

	var b strings.Builder

	if obj.Error != nil {
		msg := obj.Error.Message
		if msg == "" {
			msg = obj.Error.Type
		}
		b.WriteString(fmt.Sprintf("[error: %s]", msg))
		return b.String()
	}

	for _, c := range obj.Choices {
		if c.Delta.Content != "" {
			b.WriteString(c.Delta.Content)
		}
		if c.Delta.Refusal != "" {
			b.WriteString(fmt.Sprintf("[refusal: %s]", c.Delta.Refusal))
		}
		if c.Delta.Reasoning != "" {
			b.WriteString(fmt.Sprintf("[reasoning: %s]", c.Delta.Reasoning))
		}
		if c.Delta.ReasoningContent != "" {
			b.WriteString(fmt.Sprintf("[reasoning: %s]", c.Delta.ReasoningContent))
		}
		for _, tc := range c.Delta.ToolCalls {
			if tc.Function.Name != "" {
				b.WriteString(fmt.Sprintf("[tool_call name=%s]", tc.Function.Name))
			}
			if tc.Function.Arguments != "" {
				b.WriteString(fmt.Sprintf("[tool_args: %s]", tc.Function.Arguments))
			}
		}
		if c.FinishReason != nil && *c.FinishReason != "" && *c.FinishReason != "stop" {
			b.WriteString(fmt.Sprintf("[finish: %s]", *c.FinishReason))
		}
	}

	return b.String()
}

// ExtractOpenAIResponsesEventText extracts text from OpenAI Responses SSE events.
func ExtractOpenAIResponsesEventText(eventType string, payload []byte) string {
	switch eventType {
	case "response.output_text.delta":
		var d struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(payload, &d) == nil && d.Delta != "" {
			return d.Delta
		}
	case "response.refusal.delta":
		var d struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(payload, &d) == nil && d.Delta != "" {
			return fmt.Sprintf("[refusal: %s]", d.Delta)
		}
	case "response.reasoning_summary.delta", "response.reasoning.delta":
		var d struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(payload, &d) == nil && d.Delta != "" {
			return fmt.Sprintf("[reasoning: %s]", d.Delta)
		}
	case "response.function_call_arguments.delta":
		var d struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(payload, &d) == nil && d.Delta != "" {
			return fmt.Sprintf("[tool_args delta=%s]", d.Delta)
		}
	case "response.failed":
		var d struct {
			Response struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
				Status string `json:"status"`
			} `json:"response"`
		}
		if json.Unmarshal(payload, &d) == nil {
			reason := d.Response.Error.Message
			if reason == "" {
				reason = d.Response.Status
			}
			if reason == "" {
				reason = "unknown"
			}
			return fmt.Sprintf("[failed: %s]", reason)
		}
	case "response.incomplete":
		var d struct {
			Response struct {
				IncompleteDetails struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
		}
		if json.Unmarshal(payload, &d) == nil {
			reason := d.Response.IncompleteDetails.Reason
			if reason == "" {
				reason = "unknown"
			}
			return fmt.Sprintf("[incomplete: %s]", reason)
		}
	}
	return ""
}

// ExtractAnthropicEventText extracts text from Anthropic SSE events.
func ExtractAnthropicEventText(eventType string, payload []byte) string {
	switch eventType {
	case "content_block_delta":
		var d struct {
			Delta struct {
				Type           string `json:"type"`
				Text           string `json:"text"`
				PartialJSON    string `json:"partial_json"`
				Thinking       string `json:"thinking"`
			} `json:"delta"`
		}
		if json.Unmarshal(payload, &d) == nil {
			switch d.Delta.Type {
			case "text_delta":
				return d.Delta.Text
			case "input_json_delta":
				return fmt.Sprintf("[tool_use args=%s]", d.Delta.PartialJSON)
			case "thinking_delta":
				return fmt.Sprintf("[thinking: %s]", d.Delta.Thinking)
			}
		}
	case "content_block_start":
		var d struct {
			ContentBlock struct {
				Type string `json:"type"`
				Name string `json:"name"`
				ID   string `json:"id"`
			} `json:"content_block"`
		}
		if json.Unmarshal(payload, &d) == nil && d.ContentBlock.Type == "tool_use" {
			return fmt.Sprintf("[tool_use name=%s id=%s]", d.ContentBlock.Name, d.ContentBlock.ID)
		}
	case "message_delta":
		var d struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal(payload, &d) == nil && d.Delta.StopReason != "" {
			return fmt.Sprintf("[stop: %s]", d.Delta.StopReason)
		}
	case "error":
		var d struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(payload, &d) == nil {
			msg := d.Error.Message
			if msg == "" {
				msg = d.Error.Type
			}
			return fmt.Sprintf("[error: %s]", msg)
		}
	}
	return ""
}

// ExtractGeminiStreamFrame extracts text from a single Gemini SSE data frame.
func ExtractGeminiStreamFrame(frame []byte) string {
	return extractGeminiObject(frame)
}

func extractGeminiObject(data []byte) string {
	var obj struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string          `json:"text"`
					FunctionCall json.RawMessage `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return ""
	}

	var b strings.Builder

	if obj.PromptFeedback.BlockReason != "" {
		b.WriteString(fmt.Sprintf("[blocked: %s]", obj.PromptFeedback.BlockReason))
	}

	for _, c := range obj.Candidates {
		for _, p := range c.Content.Parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
			if len(p.FunctionCall) > 0 {
				var fc struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				}
				if json.Unmarshal(p.FunctionCall, &fc) == nil && fc.Name != "" {
					argsStr := string(fc.Args)
					if argsStr == "" {
						argsStr = "{}"
					}
					b.WriteString(fmt.Sprintf("[function_call name=%s args=%s]", fc.Name, argsStr))
				}
			}
		}
		switch c.FinishReason {
		case "SAFETY", "RECITATION", "MAX_TOKENS", "OTHER":
			b.WriteString(fmt.Sprintf("[finish: %s]", c.FinishReason))
		}
	}

	return b.String()
}

func extractGeminiJSONArray(raw []byte) string {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return ""
	}
	var b strings.Builder
	for _, item := range arr {
		b.WriteString(extractGeminiObject(item))
	}
	return b.String()
}

// ──────────────────────────────────────────────────────
// SSE parsing helpers
// ──────────────────────────────────────────────────────

// forEachSSEData iterates data-only SSE (no event: lines), like OpenAI Chat / Gemini.
func forEachSSEData(raw []byte, fn func(data []byte)) {
	scanner := bytes.NewBuffer(raw)
	for {
		line, err := scanner.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			break
		}
		line = bytes.TrimRight(line, "\r\n")
		if bytes.HasPrefix(line, []byte("data: ")) {
			data := line[6:]
			if bytes.Equal(data, []byte("[DONE]")) {
				continue
			}
			fn(data)
		}
		if err != nil {
			break
		}
	}
}

// forEachSSEEvent iterates SSE with event:/data: pairs (Anthropic / OpenAI Responses).
// If a data: line has no preceding event: line, eventType is inferred from the data.
func forEachSSEEvent(raw []byte, fn func(eventType string, data []byte)) {
	scanner := bytes.NewBuffer(raw)
	var currentEvent string
	for {
		line, err := scanner.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			break
		}
		line = bytes.TrimRight(line, "\r\n")
		if bytes.HasPrefix(line, []byte("event: ")) {
			currentEvent = string(bytes.TrimSpace(line[7:]))
		} else if bytes.HasPrefix(line, []byte("data: ")) {
			data := line[6:]
			if bytes.Equal(data, []byte("[DONE]")) {
				currentEvent = ""
				continue
			}
			evtType := currentEvent
			if evtType == "" {
				evtType = inferEventType(data)
			}
			fn(evtType, data)
			currentEvent = ""
		} else if len(line) == 0 {
			// empty line = event boundary in SSE, reset
			// but only if we haven't already consumed the data
		}
		if err != nil {
			break
		}
	}
}

// inferEventType attempts to determine Anthropic event type from data payload
// when no event: line is present.
func inferEventType(data []byte) string {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &probe) == nil && probe.Type != "" {
		return probe.Type
	}
	return ""
}
