package service

// Tests for raw-event tee on the /v1/responses path.
// Each test verifies that, alongside the AppendOutput text excerpt,
// AppendRawEvent is also called so that OutputBody carries the full
// structured SSE/JSON payload rather than just flattened text.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// attachTestAuditCollector attaches a PayloadAuditCollector to a gin context
// and returns it so tests can inspect Finalize().
func attachTestAuditCollector(c *gin.Context) *PayloadAuditCollector {
	snap := &ConfigSnapshot{Enabled: true, AllGroups: true, OutputMaxBytes: 0, ExcerptBytes: 512}
	coll := NewPayloadAuditCollector(snap)
	coll.SetMetadata(PayloadAuditMetadata{
		Provider: "openai",
		Endpoint: "/v1/responses",
		Stream:   true,
	})
	c.Set(PayloadAuditCollectorCtxKey, coll)
	return coll
}

// ---------------------------------------------------------------------------
// Site 1: handleStreamingResponsePassthrough (OpenAI passthrough streaming)
// ---------------------------------------------------------------------------

func TestHandleStreamingResponsePassthrough_TeesRawSSEEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	coll := attachTestAuditCollector(c)

	sseBodies := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hello ","item_id":"item_1","output_index":0,"content_index":0}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"world","item_id":"item_1","output_index":0,"content_index":0}`,
		``,
		`data: {"type":"response.done","response":{"id":"resp_abc","usage":{"input_tokens":5,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}}}`,
		``,
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(sseBodies)),
		Header:     http.Header{"x-request-id": []string{"rid-raw-tee"}},
	}

	_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "", "")
	require.NoError(t, err)

	evt := coll.Finalize(200, 0, "")
	require.NotNil(t, evt)

	// The OutputBody must contain the raw SSE lines (not just flattened text).
	require.Equal(t, "sse", evt.OutputFormat, "OutputFormat should be sse when AppendRawEvent was called")
	require.Contains(t, evt.OutputBody, "response.output_text.delta", "raw SSE event type must be present in OutputBody")
	require.Contains(t, evt.OutputBody, "hello ", "raw delta text must be present in OutputBody")

	// The excerpt should still come from AppendOutput (flattened text).
	require.Contains(t, evt.OutputExcerpt, "hello", "AppendOutput excerpt must still work")
}

// ---------------------------------------------------------------------------
// Site 2: handleNonStreamingResponsePassthrough (non-stream OpenAI passthrough)
// ---------------------------------------------------------------------------

func TestHandleNonStreamingResponsePassthrough_TeesRawJSONBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	// Non-stream request: Stream=false so OutputFormat is "json" when raw body present.
	snap := &ConfigSnapshot{Enabled: true, AllGroups: true, OutputMaxBytes: 0, ExcerptBytes: 512}
	coll := NewPayloadAuditCollector(snap)
	coll.SetMetadata(PayloadAuditMetadata{
		Provider: "openai",
		Endpoint: "/v1/responses",
		Stream:   false,
	})
	c.Set(PayloadAuditCollectorCtxKey, coll)

	respJSON := `{"id":"resp_ns_1","output":[{"type":"message","content":[{"type":"output_text","text":"hi there"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(respJSON)),
	}

	_, err := svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, "", "")
	require.NoError(t, err)

	evt := coll.Finalize(200, 0, "")
	require.NotNil(t, evt)

	// Raw body should be the full JSON, not just text.
	require.Equal(t, "json", evt.OutputFormat, "OutputFormat should be json for non-stream body tee")
	require.Contains(t, evt.OutputBody, `"id":"resp_ns_1"`, "raw JSON must be in OutputBody")
	require.Contains(t, evt.OutputExcerpt, "hi there", "excerpt must still come from AppendOutput")
}

// ---------------------------------------------------------------------------
// Site 3: handleResponsesStreamingResponse (Anthropic→Responses conversion)
// ---------------------------------------------------------------------------

func TestHandleResponsesStreamingResponse_TeesConvertedSSEEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	coll := attachTestAuditCollector(c)

	// Simulate Anthropic upstream SSE that will be converted to Responses SSE.
	upstreamSSE := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid-converted"}},
		Body:   io.NopCloser(strings.NewReader(upstreamSSE)),
	}

	svc := &GatewayService{}
	result, err := svc.handleResponsesStreamingResponse(resp, c, "claude-sonnet-4.5", "claude-sonnet-4.5", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)

	evt := coll.Finalize(200, 0, "")
	require.NotNil(t, evt)

	// The raw event output must contain converted Responses-format SSE.
	require.Equal(t, "sse", evt.OutputFormat, "OutputFormat should be sse for converted streaming response")
	// The converted SSE should contain Responses API event types.
	require.True(t,
		strings.Contains(evt.OutputBody, "response.") || strings.Contains(evt.OutputBody, "event:"),
		"OutputBody should contain converted Responses SSE events, got: %q", evt.OutputBody,
	)
}

// ---------------------------------------------------------------------------
// Site 4: handleResponsesBufferedStreamingResponse (non-stream Anthropic→Responses)
// ---------------------------------------------------------------------------

func TestHandleResponsesBufferedStreamingResponse_TeesRawJSONBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	snap := &ConfigSnapshot{Enabled: true, AllGroups: true, OutputMaxBytes: 0, ExcerptBytes: 512}
	coll := NewPayloadAuditCollector(snap)
	coll.SetMetadata(PayloadAuditMetadata{
		Provider: "openai",
		Endpoint: "/v1/responses",
		Stream:   false,
	})
	c.Set(PayloadAuditCollectorCtxKey, coll)

	upstreamSSE := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_buf","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":8,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"buffered"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
	}, "\n")

	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid-buffered-audit"}},
		Body:   io.NopCloser(strings.NewReader(upstreamSSE)),
	}

	svc := &GatewayService{}
	result, err := svc.handleResponsesBufferedStreamingResponse(resp, c, "claude-sonnet-4.5", "claude-sonnet-4.5", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)

	evt := coll.Finalize(200, 0, "")
	require.NotNil(t, evt)

	// The raw output must be the marshalled Responses JSON, not just text.
	require.Equal(t, "json", evt.OutputFormat, "OutputFormat should be json for buffered (non-stream) response tee")
	// The body should contain Responses API fields.
	require.True(t,
		strings.Contains(evt.OutputBody, `"output"`) || strings.Contains(evt.OutputBody, `"id"`),
		"OutputBody should contain Responses JSON, got: %q", evt.OutputBody,
	)
}
