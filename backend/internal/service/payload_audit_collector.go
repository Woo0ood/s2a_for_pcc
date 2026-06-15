package service

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync/atomic"
	"time"
)

// PayloadAuditCollectorCtxKey is the gin context key used to pass the
// collector from the handler layer to the service layer without changing
// function signatures.
const PayloadAuditCollectorCtxKey = "payload_audit_collector"

// GetPayloadAuditCollector retrieves the collector from gin.Context.
// Returns nil if absent or disabled.
func GetPayloadAuditCollector(c interface{ Get(string) (any, bool) }) *PayloadAuditCollector {
	v, ok := c.Get(PayloadAuditCollectorCtxKey)
	if !ok {
		return nil
	}
	coll, _ := v.(*PayloadAuditCollector)
	if coll == nil || !coll.Enabled() {
		return nil
	}
	return coll
}

// PayloadAuditEvent is the service-layer representation of an audit event.
// It mirrors repository.PayloadAuditEvent; the sink layer converts between the two.
type PayloadAuditEvent struct {
	// populated by handler before TryEnqueue
	ID                                             int64
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
	InputOffloaded                                 bool
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

	pendingBlobs      []ExtractedBlob
	pendingBody       *ExtractedBlob
	originalForRevert []byte
	inputOffloaded    bool

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

// SetInput copies and optionally truncates the request body. When offload is
// enabled, inline base64 blobs (and an oversized remainder) are replaced with
// pointers and staged for upload; the actual upload happens at finalize time.
func (c *PayloadAuditCollector) SetInput(body []byte, format string) {
	if !c.enabled {
		return
	}
	c.inputBytes = len(body)
	c.inputFormat = format

	work := body
	if c.snap.OffloadEnabled {
		rewritten, blobs := RewriteInlineBlobs(body, c.snap.BlobOffloadMinBytes)
		if len(blobs) > 0 {
			work = rewritten
			c.pendingBlobs = blobs
		}
	}

	maxBytes := c.snap.InputMaxBytes
	if c.snap.OffloadEnabled && maxBytes > 0 && len(work) > maxBytes {
		sum := sha256.Sum256(work)
		sha := hex.EncodeToString(sum[:])
		cp := make([]byte, len(work))
		copy(cp, work)
		c.pendingBody = &ExtractedBlob{SHA256: sha, MIME: "application/json", Bytes: len(work), Data: cp}
		c.inputBody = []byte(EncodeBodyPointer(sha, len(work)))
		c.originalForRevert = body
		return
	}

	if len(c.pendingBlobs) > 0 {
		c.originalForRevert = body
	}

	if maxBytes > 0 && len(work) > maxBytes {
		dst := make([]byte, maxBytes)
		copy(dst, work[:maxBytes])
		c.inputBody = dst
		c.inputTrunc = true
		return
	}
	dst := make([]byte, len(work))
	copy(dst, work)
	c.inputBody = dst
}

// PendingBlobs returns blobs staged for offload upload (set by SetInput).
func (c *PayloadAuditCollector) PendingBlobs() []ExtractedBlob { return c.pendingBlobs }

// PendingBody returns an oversized body staged for whole-body offload, or nil.
func (c *PayloadAuditCollector) PendingBody() *ExtractedBlob { return c.pendingBody }

// OriginalForRevert returns the original pre-rewrite body, used to revert on upload failure.
func (c *PayloadAuditCollector) OriginalForRevert() []byte { return c.originalForRevert }

// MarkInputOffloaded marks the input as successfully offloaded (called by Task 7 uploader).
func (c *PayloadAuditCollector) MarkInputOffloaded() { c.inputOffloaded = true }

// InputBodyForTest returns the current inputBody as a string (for use in tests).
func (c *PayloadAuditCollector) InputBodyForTest() string { return string(c.inputBody) }

// RevertOffload undoes a staged offload on upload failure: clears pending state
// and falls back to normal truncation of the original body.
func (c *PayloadAuditCollector) RevertOffload(original []byte) {
	c.pendingBlobs, c.pendingBody, c.inputOffloaded = nil, nil, false
	maxBytes := c.snap.InputMaxBytes
	if maxBytes > 0 && len(original) > maxBytes {
		dst := make([]byte, maxBytes)
		copy(dst, original[:maxBytes])
		c.inputBody = dst
		c.inputTrunc = true
		return
	}
	dst := make([]byte, len(original))
	copy(dst, original)
	c.inputBody = dst
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
		InputOffloaded:  c.inputOffloaded,
		ErrorMessage:    errMsg,
		CreatedAt:       time.Now(),
	}
}
