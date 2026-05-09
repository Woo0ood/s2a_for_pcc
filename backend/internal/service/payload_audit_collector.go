package service

import (
	"strings"
	"sync/atomic"
	"time"
)

// PayloadAuditEvent is the service-layer representation of an audit event.
// It mirrors repository.PayloadAuditEvent; the sink layer converts between the two.
type PayloadAuditEvent struct {
	RequestID                                      string
	UserID, APIKeyID, GroupID                       *int64
	UserEmail, APIKeyName, GroupName, ClientIP      string
	Endpoint, Provider, Model, UpstreamModel       string
	Stream                                         bool
	StatusCode, DurationMs                         int
	InputExcerpt, OutputExcerpt                    string
	InputBody, OutputBody                          string
	InputFormat, OutputFormat                      string
	InputBytes, OutputBytes                        int
	InputTruncated, OutputTruncated, OutputOmitted bool
	ErrorMessage                                   string
	CreatedAt                                      time.Time
}

// PayloadAuditMetadata holds request-scoped metadata for audit logging.
type PayloadAuditMetadata struct {
	RequestID                                  string
	UserEmail, APIKeyName, GroupName, ClientIP  string
	UserID, APIKeyID, GroupID                   *int64
	Endpoint, Provider, Model, UpstreamModel   string
	Stream                                     bool
}

// PayloadAuditCollector accumulates input/output data during a request lifecycle
// and produces a PayloadAuditEvent on finalization.
//
// All methods are safe to call on a disabled (nil-snap or out-of-scope) collector;
// they fast-path return without allocating.
type PayloadAuditCollector struct {
	snap        *ConfigSnapshot
	enabled     bool
	meta        PayloadAuditMetadata
	inputBody   []byte
	inputBytes  int // pre-truncation original size
	inputTrunc  bool
	inputFormat string

	outputBuf     strings.Builder
	outputBytes   int // total bytes appended (including truncated portion)
	outputTrunc   bool
	outputOmitted bool

	finalized atomic.Bool
}

// NewPayloadAuditCollector creates a collector. When snap is nil or disabled,
// the returned collector is inert — all methods are no-ops.
func NewPayloadAuditCollector(snap *ConfigSnapshot) *PayloadAuditCollector {
	if snap == nil || !snap.Enabled {
		return &PayloadAuditCollector{}
	}
	return &PayloadAuditCollector{snap: snap, enabled: true}
}

// Enabled reports whether this collector is active.
func (c *PayloadAuditCollector) Enabled() bool { return c != nil && c.enabled }

// SetMetadata sets request metadata. If the group is not in scope the collector
// is disabled for the remainder of the request.
func (c *PayloadAuditCollector) SetMetadata(m PayloadAuditMetadata) {
	if !c.enabled {
		return
	}
	if !c.snap.GroupInScope(m.GroupID) {
		c.enabled = false
		return
	}
	c.meta = m
}

// Metadata returns the stored metadata.
func (c *PayloadAuditCollector) Metadata() PayloadAuditMetadata { return c.meta }

// SetInput copies and optionally truncates the request body.
func (c *PayloadAuditCollector) SetInput(body []byte, format string) {
	if !c.enabled {
		return
	}
	c.inputBytes = len(body)
	c.inputFormat = format
	cap := c.snap.InputMaxBytes
	if cap > 0 && len(body) > cap {
		dst := make([]byte, cap)
		copy(dst, body[:cap])
		c.inputBody = dst
		c.inputTrunc = true
	} else {
		dst := make([]byte, len(body))
		copy(dst, body)
		c.inputBody = dst
	}
}

// AppendOutput appends streaming delta text. Once the output cap is reached,
// further text is counted but not stored.
func (c *PayloadAuditCollector) AppendOutput(s string) {
	if !c.enabled {
		return
	}
	c.outputBytes += len(s)
	if c.outputTrunc {
		return
	}
	cap := c.snap.OutputMaxBytes
	if cap <= 0 {
		c.outputBuf.WriteString(s)
		return
	}
	remaining := cap - c.outputBuf.Len()
	if remaining <= 0 {
		c.outputTrunc = true
		return
	}
	if len(s) > remaining {
		c.outputBuf.WriteString(s[:remaining])
		c.outputTrunc = true
		return
	}
	c.outputBuf.WriteString(s)
}

// MarkOutputOmitted marks the output as intentionally omitted (e.g. embeddings).
func (c *PayloadAuditCollector) MarkOutputOmitted() {
	if !c.enabled {
		return
	}
	c.outputOmitted = true
}

// Finalize builds and returns a PayloadAuditEvent. Only the first call returns
// a non-nil event; subsequent calls return nil (CAS-protected).
func (c *PayloadAuditCollector) Finalize(statusCode int, dur time.Duration, errMsg string) *PayloadAuditEvent {
	if !c.enabled {
		return nil
	}
	if !c.finalized.CompareAndSwap(false, true) {
		return nil
	}
	return c.buildEvent(statusCode, dur, errMsg)
}

func (c *PayloadAuditCollector) buildEvent(statusCode int, dur time.Duration, errMsg string) *PayloadAuditEvent {
	inputText, _ := ExtractInputText(c.meta.Provider, c.meta.Endpoint, c.inputBody)
	outputText := c.outputBuf.String()

	inputExcerpt := excerpt(inputText, c.snap.ExcerptBytes)
	outputExcerpt := excerpt(outputText, c.snap.ExcerptBytes)

	return &PayloadAuditEvent{
		RequestID:       c.meta.RequestID,
		UserID:          c.meta.UserID,
		UserEmail:       c.meta.UserEmail,
		APIKeyID:        c.meta.APIKeyID,
		APIKeyName:      c.meta.APIKeyName,
		GroupID:         c.meta.GroupID,
		GroupName:       c.meta.GroupName,
		ClientIP:        c.meta.ClientIP,
		Endpoint:        c.meta.Endpoint,
		Provider:        c.meta.Provider,
		Model:           c.meta.Model,
		UpstreamModel:   c.meta.UpstreamModel,
		Stream:          c.meta.Stream,
		StatusCode:      statusCode,
		DurationMs:      int(dur / time.Millisecond),
		InputExcerpt:    inputExcerpt,
		OutputExcerpt:   outputExcerpt,
		InputBody:       string(c.inputBody),
		OutputBody:      outputText,
		InputFormat:     c.inputFormat,
		OutputFormat:    "text",
		InputBytes:      c.inputBytes,
		OutputBytes:     c.outputBytes,
		InputTruncated:  c.inputTrunc,
		OutputTruncated: c.outputTrunc,
		OutputOmitted:   c.outputOmitted,
		ErrorMessage:    errMsg,
		CreatedAt:       time.Now(),
	}
}
