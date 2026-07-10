package service

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
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
	ConversationKey, ResponseID, PreviousResponseID string
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

	convKey    string
	prevRespID string
	respID     string

	// Captured outside meta so a later SetMetadata cannot clobber them:
	// clientIP/requestID are stamped at handler entry; upstreamModel is
	// extracted from the response as it streams in.
	clientIP      string
	requestID     string
	upstreamModel string

	outputBuf     strings.Builder
	outputBytes   int // total bytes appended (including truncated portion)
	outputTrunc   bool
	outputOmitted bool
	rawOutput     strings.Builder // structured raw response events (SSE/JSON); preferred over text for OutputBody

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

// SetRequestContext records request-scoped identity captured at handler entry:
// the client IP (resolved via the forwarded-header chain, not the direct proxy
// peer) and the server-generated request id. Stored outside meta so the later
// per-handler SetMetadata call cannot wipe them.
func (c *PayloadAuditCollector) SetRequestContext(clientIP, requestID string) {
	if !c.enabled {
		return
	}
	c.clientIP = clientIP
	c.requestID = requestID
}

// SetInput copies and optionally truncates the request body. When offload is
// enabled, inline base64 blobs (and an oversized remainder) are replaced with
// pointers and staged for upload; the actual upload happens at finalize time.
// setInputPanicHook is a test-only seam to exercise SetInput's panic recovery.
// It is nil in production (a single nil-check); tests set it to force a panic
// mid-collection. Real inputs cannot panic SetInput today (gjson + a linear
// regex), so this is the only way to deterministically cover the recover path.
var setInputPanicHook func()

// recoverCollect swallows any panic from a synchronous in-path collection method
// so audit collection can NEVER break the LLM request/response path. Deferred at
// the top of each in-path mutator; recover() works because recoverCollect itself
// is the deferred function.
func (c *PayloadAuditCollector) recoverCollect(method string) {
	if r := recover(); r != nil {
		slog.Error("payload_audit.collect_panic", "method", method, "endpoint", c.meta.Endpoint, "panic", r)
	}
}

func (c *PayloadAuditCollector) SetInput(body []byte, format string) {
	if !c.enabled {
		return
	}
	// Collection must never break the request path. On any panic, discard staged
	// offload state (so settleOffload won't commit a dangling pointer with no
	// backing object) and fall back to a minimal inline marker so a row still lands.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("payload_audit.set_input_panic", "endpoint", c.meta.Endpoint, "panic", r)
			c.pendingBlobs = nil
			c.pendingBody = nil
			c.originalForRevert = nil
			c.inputOffloaded = false
			c.inputTrunc = false
			c.inputFormat = format
			c.inputBody = []byte("[payload-audit: input capture failed]")
		}
	}()
	if setInputPanicHook != nil {
		setInputPanicHook()
	}
	c.inputBytes = len(body)
	if ck, prev := ExtractRequestConvIDs(c.meta.Endpoint, body); ck != "" || prev != "" {
		c.convKey, c.prevRespID = ck, prev
	}
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
	// Oversized remainder: offload the whole (already blob-rewritten) body. Note
	// pendingBody.Data is `work`, so it may itself contain s2a-blob:// pointers —
	// that is intended (no duplication); reconstruction resolves blobs separately.
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

// SetResponseID records the upstream response id (called by output capture once the id is known).
func (c *PayloadAuditCollector) SetResponseID(id string) {
	if c.enabled && id != "" {
		c.respID = id
	}
}

// InputBodyForTest returns the current inputBody as a string (for use in tests).
func (c *PayloadAuditCollector) InputBodyForTest() string { return string(c.inputBody) }

// RevertOffload undoes a staged offload on upload failure: clears pending state
// and falls back to normal truncation of the original body.
func (c *PayloadAuditCollector) RevertOffload(original []byte) {
	c.pendingBlobs, c.pendingBody, c.inputOffloaded = nil, nil, false
	c.originalForRevert, c.inputTrunc = nil, false
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
	defer c.recoverCollect("AppendOutput")
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

// AppendRawEvent appends a raw upstream response event (an SSE chunk or the full
// JSON body) for structured-fidelity capture. Stored verbatim up to OutputMaxBytes;
// the assistant's tool calls/args/results live here, while the flattened text from
// AppendOutput is kept only for the human-readable excerpt. Opportunistically
// captures the upstream response id from the first event that carries one.
func (c *PayloadAuditCollector) AppendRawEvent(b []byte) {
	if !c.enabled || len(b) == 0 {
		return
	}
	defer c.recoverCollect("AppendRawEvent")
	if c.respID == "" {
		if id := ExtractResponseID(c.meta.Endpoint, b); id != "" {
			c.respID = id
		}
	}
	if c.upstreamModel == "" {
		if m := ExtractUpstreamModel(c.meta.Endpoint, b); m != "" {
			c.upstreamModel = m
		}
	}
	cap := c.snap.OutputMaxBytes
	if cap <= 0 {
		c.rawOutput.Write(b)
		return
	}
	remaining := cap - c.rawOutput.Len()
	if remaining <= 0 {
		c.outputTrunc = true
		return
	}
	if len(b) > remaining {
		c.rawOutput.Write(b[:remaining])
		c.outputTrunc = true
		return
	}
	c.rawOutput.Write(b)
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

	// Prefer structured raw events for the stored body (faithful tool calls/args);
	// fall back to flattened text. The excerpt always uses the human-readable text.
	outputBody, outputFormat := outputText, "text"
	if c.rawOutput.Len() > 0 {
		outputBody = c.rawOutput.String()
		if c.meta.Stream {
			outputFormat = "sse"
		} else {
			outputFormat = "json"
		}
	}

	// Prefer the request-context / response-extracted values; fall back to
	// whatever a handler may have placed on meta (keeps meta a valid source too).
	clientIP := c.clientIP
	if clientIP == "" {
		clientIP = c.meta.ClientIP
	}
	requestID := c.requestID
	if requestID == "" {
		requestID = c.meta.RequestID
	}
	upstreamModel := c.upstreamModel
	if upstreamModel == "" {
		upstreamModel = c.meta.UpstreamModel
	}

	return &PayloadAuditEvent{
		RequestID:       requestID,
		UserID:          c.meta.UserID,
		UserEmail:       c.meta.UserEmail,
		APIKeyID:        c.meta.APIKeyID,
		APIKeyName:      c.meta.APIKeyName,
		GroupID:         c.meta.GroupID,
		GroupName:       c.meta.GroupName,
		ClientIP:        clientIP,
		Endpoint:        c.meta.Endpoint,
		Provider:        c.meta.Provider,
		Model:           c.meta.Model,
		UpstreamModel:   upstreamModel,
		Stream:          c.meta.Stream,
		StatusCode:      statusCode,
		DurationMs:      int(dur / time.Millisecond),
		InputExcerpt:    inputExcerpt,
		OutputExcerpt:   outputExcerpt,
		InputBody:       string(c.inputBody),
		OutputBody:      outputBody,
		InputFormat:     c.inputFormat,
		OutputFormat:    outputFormat,
		InputBytes:      c.inputBytes,
		OutputBytes:     c.outputBytes,
		InputTruncated:  c.inputTrunc,
		OutputTruncated: c.outputTrunc,
		OutputOmitted:   c.outputOmitted,
		InputOffloaded:     c.inputOffloaded,
		ConversationKey:    c.convKey,
		ResponseID:         c.respID,
		PreviousResponseID: c.prevRespID,
		ErrorMessage:       errMsg,
		CreatedAt:          time.Now(),
	}
}
