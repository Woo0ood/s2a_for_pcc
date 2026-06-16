package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	maxInlineImageBytes = 8 << 20  // 8 MiB per image (decoded)
	maxTotalInlineBytes = 32 << 20 // 32 MiB total per transcript
)

var imageMIMERe = regexp.MustCompile(`^image/[a-zA-Z0-9.+-]+$`)

// inlineImageRe matches an inline `data:image/...;base64,<payload>` URI embedded
// in a stored body (e.g. the `image_url` of an `input_image` content block).
// Submatch 1 = MIME ("image/png"), submatch 2 = the base64 payload. We use
// FindAllStringSubmatchIndex so callers get byte offsets (not 316 KB string
// copies per match) and only slice out the full data URI for NEW images.
var inlineImageRe = regexp.MustCompile(`data:(image/[a-zA-Z0-9.+-]+);base64,([A-Za-z0-9+/]+={0,2})`)

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
// inlineBudget — shared cross-turn image-inline accounting
// ─────────────────────────────────────────────────────────────────────────────

// inlineBudget tracks how many bytes of inline images have been embedded across
// turns. perImageMax<=0 or totalMax<=0 means unlimited.
type inlineBudget struct {
	perImageMax int // max bytes per single image; <=0 = unlimited
	totalMax    int // max bytes total across all images; <=0 = unlimited
	used        int // bytes consumed so far
	exhausted   bool
}

// ─────────────────────────────────────────────────────────────────────────────
// assembleTurn — per-row conversion (shared by in-memory and streaming paths)
// ─────────────────────────────────────────────────────────────────────────────

// assembleTurn converts a single PayloadAuditEvent into a Turn. It also inlines
// image attachments against the shared budget (when resolver != nil).
//
// Cross-turn attachment dedup: Codex /v1/responses bodies are cumulative — every
// turn re-sends ALL prior history including images. seenAttachments is a
// conversation-scoped set (created once, shared across turns) that ensures each
// unique image — whether an offloaded blob pointer or an inline data:image — is
// emitted as an Attachment exactly ONCE across the whole transcript, instead of
// once per turn (which produced N copies → giant HTML).
//
// Returns the Turn and any gap strings generated for this row.
func assembleTurn(
	ctx context.Context,
	row *PayloadAuditEvent,
	index int,
	resolver *BlobResolver,
	seenHashes map[string]bool,
	seenAttachments map[string]bool,
	responseIDSet map[string]bool,
	budget *inlineBudget,
) (Turn, []string) {
	// Resolve pointers in bodies.
	resolvedInput, inputAtts, inputGaps := resolver.ResolveBody(ctx, row.InputBody)
	resolvedOutput, outputAtts, outputGaps := resolver.ResolveBody(ctx, row.OutputBody)

	gaps := append(inputGaps, outputGaps...)

	// ── Build this turn's attachments as the cross-turn dedup-survivors of: ──
	//   (a) pointer (offloaded blob) attachments — keyed by their known sha; and
	//   (b) inline data:image attachments parsed out of the resolved bodies.
	var atts []Attachment

	// (a) Pointer attachments: dedup by the pointer's sha (FREE — already known).
	// Each unique offloaded image is kept once across the whole transcript.
	for _, att := range append(inputAtts, outputAtts...) {
		key := "ptr:" + att.SHA256
		if seenAttachments[key] {
			continue
		}
		seenAttachments[key] = true
		atts = append(atts, att)
	}

	// (b) Inline data:image attachments.
	// These were previously DROPPED entirely (extractResponsesContentText only
	// reads block.text). They render via the deduped Attachments section, kept
	// self-contained as base64 (DataURI = the inline data: URI — no S3 link).
	//
	// INPUT inline images are extracted DURING item parsing (parseUserItems),
	// tied to the per-item cross-turn dedup: Codex /v1/responses input is
	// cumulative (turn N re-contains every prior image), and the item dedup
	// already iterates that cumulative input to find NEW items — so we do the
	// expensive per-image work ONLY for new items → each unique image once
	// (linear), instead of re-scanning the whole body every turn (quadratic).
	//
	// OUTPUT is NOT cumulative and is small, so the whole-body scan there stays
	// cheap and correct — keep extractInlineImages for the output path only.
	userItems, userAtts, userGaps := parseUserItems(row.Endpoint, resolvedInput, seenHashes, seenAttachments, budget)
	inlineOutputAtts, inlineOutputGaps := extractInlineImages(resolvedOutput, seenAttachments, budget)
	atts = append(atts, userAtts...)
	atts = append(atts, inlineOutputAtts...)
	gaps = append(gaps, userGaps...)
	gaps = append(gaps, inlineOutputGaps...)

	// Parse assistant items from the resolved output.
	assistantItems := parseAssistantItems(row.OutputFormat, resolvedOutput)

	// Check gap conditions.
	if row.PreviousResponseID != "" && !responseIDSet[row.PreviousResponseID] {
		gaps = append(gaps, fmt.Sprintf(
			"此前历史不在留存范围 (previous_response_id=%s)", row.PreviousResponseID,
		))
	}

	turn := Turn{
		Index:          index,
		CreatedAt:      row.CreatedAt,
		Model:          row.Model,
		StatusCode:     row.StatusCode,
		UserItems:      userItems,
		Assistant:      assistantItems,
		Attachments:    atts,
		RawInputBytes:  len(row.InputBody),
		RawOutputBytes: len(row.OutputBody),
	}

	// ── Inline pass: fetch and embed POINTER image blobs as data URIs ────────
	// Only runs when a resolver is configured. Inline data:image attachments
	// from extractInlineImages already carry a DataURI (self-contained base64),
	// so they are skipped here — only pointer attachments (DataURI still empty)
	// are downloaded from the blob store.
	if resolver != nil {
		for ai := range turn.Attachments {
			att := &turn.Attachments[ai]

			// Already inlined (inline data:image attachment) — leave it.
			if att.DataURI != "" {
				continue
			}

			// Only inline images; non-image attachments stay as proxy links.
			if !imageMIMERe.MatchString(att.MIME) {
				continue
			}

			// Per-image size cap (if set).
			if budget.perImageMax > 0 && att.Bytes > budget.perImageMax {
				gaps = append(gaps,
					fmt.Sprintf("image not inlined (exceeds %d MiB cap): sha=%s bytes=%d",
						budget.perImageMax>>20, att.SHA256, att.Bytes))
				continue
			}

			// Total budget cap (if set) — emit the exhaustion note once.
			if budget.totalMax > 0 && budget.used+att.Bytes > budget.totalMax {
				if !budget.exhausted {
					budget.exhausted = true
					gaps = append(gaps,
						"inline image budget exhausted; remaining images shown as links only")
				}
				continue
			}

			// Fetch blob. FetchBlob's returned MIME is always "application/octet-stream";
			// we use att.MIME (from the pointer) which was already validated by imageMIMERe.
			data, _, err := resolver.FetchBlob(ctx, att.SHA256)
			if err != nil {
				gaps = append(gaps,
					fmt.Sprintf("blob inline fetch failed (sha=%s): %v", att.SHA256, err))
				continue
			}

			att.DataURI = "data:" + att.MIME + ";base64," + base64.StdEncoding.EncodeToString(data)
			budget.used += len(data)
		}
	}

	return turn, gaps
}

// ─────────────────────────────────────────────────────────────────────────────
// extractInlineImages — ingest inline data:image URIs, dedup cross-turn
// ─────────────────────────────────────────────────────────────────────────────

// extractInlineImages scans body for inline `data:image/...;base64,<payload>`
// URIs (the `image_url` of Codex `input_image` content blocks, which the message
// text extractor drops) and returns one self-contained Attachment per UNIQUE
// image. Uniqueness is tracked in the shared, conversation-scoped `seen` map so
// the same image is emitted once across all (cumulative) turns.
//
// Efficiency — this MUST stay linear in the cumulative body size even though
// turn N re-contains every prior image:
//   - We match with FindAllStringSubmatchIndex, so each match yields byte OFFSETS,
//     not a copy of the (potentially 316 KB) base64 payload.
//   - The dedup-check key is CHEAP — computed from the match's metadata only:
//     "inl:" + mime + ":" + len(b64) + ":" + b64[:24] + ":" + b64[len-24:].
//     We do NOT decode or sha256 the whole payload just to decide if it's new.
//   - Full work (slicing the data: URI string, sha256 for the display caption)
//     happens ONCE per unique image — i.e. only when the cheap key is unseen.
// For a 1300-turn conversation that re-sends the same image every turn, this is
// O(total bytes scanned) with O(1) heavy work per unique image, not O(N²).
func extractInlineImages(body string, seen map[string]bool, budget *inlineBudget) ([]Attachment, []string) {
	if body == "" || !strings.Contains(body, "data:image/") {
		return nil, nil
	}

	matches := inlineImageRe.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return nil, nil
	}

	var atts []Attachment
	var gaps []string

	for _, m := range matches {
		// m layout: [fullStart,fullEnd, mimeStart,mimeEnd, b64Start,b64End].
		fullStart, fullEnd := m[0], m[1]
		mime := body[m[2]:m[3]]
		b64Start, b64End := m[4], m[5]
		b64Len := b64End - b64Start

		// Cheap fingerprint computed from offsets only — no full-payload copy/hash.
		key := inlineKey(mime, body, b64Start, b64End)
		if seen[key] {
			continue
		}
		seen[key] = true

		// NEW image → do the heavy work exactly once.
		dataURI := body[fullStart:fullEnd]                          // full "data:...;base64,...."
		b64 := body[b64Start:b64End]                                // base64 payload (slice, no copy)
		decodedLen := base64.StdEncoding.DecodedLen(b64Len)         // upper bound is fine for display
		sum := sha256.Sum256([]byte(b64))                           // display caption sha (once)
		shaHex := hex.EncodeToString(sum[:])

		att := Attachment{
			SHA256: shaHex,
			MIME:   mime,
			Bytes:  decodedLen,
			// No ProxyPath: inline images are self-contained; DataURI set below
			// (possibly skipped under a bounded budget — see caps).
		}

		// Per-image size cap (bounded budget only). Mirrors the oversized-pointer
		// path: leave DataURI empty + a gap note so the in-process fallback stays
		// bounded. Under unlimited budget (perImageMax<=0) we always keep it.
		if budget != nil && budget.perImageMax > 0 && decodedLen > budget.perImageMax {
			gaps = append(gaps,
				fmt.Sprintf("inline image not inlined (exceeds %d MiB cap): sha=%s bytes=%d",
					budget.perImageMax>>20, shaHex, decodedLen))
			atts = append(atts, att)
			continue
		}

		// Total budget cap (bounded budget only) — emit the exhaustion note once.
		if budget != nil && budget.totalMax > 0 && budget.used+decodedLen > budget.totalMax {
			if !budget.exhausted {
				budget.exhausted = true
				gaps = append(gaps,
					"inline image budget exhausted; remaining images shown as links only")
			}
			atts = append(atts, att)
			continue
		}

		att.DataURI = dataURI
		if budget != nil {
			budget.used += decodedLen
		}
		atts = append(atts, att)
	}

	return atts, gaps
}

// inlineKey builds a CHEAP dedup fingerprint for an inline data:image occurrence
// without decoding/hashing the whole base64 payload. It combines the MIME, the
// payload length, and the first/last 24 base64 chars — enough to distinguish
// distinct images while staying O(1) per occurrence (the body slice is shared,
// not copied; only the two 24-char ends are concatenated into the key).
func inlineKey(mime, body string, b64Start, b64End int) string {
	b64Len := b64End - b64Start
	const edge = 24
	var head, tail string
	if b64Len <= 2*edge {
		head = body[b64Start:b64End]
		tail = ""
	} else {
		head = body[b64Start : b64Start+edge]
		tail = body[b64End-edge : b64End]
	}
	return "inl:" + mime + ":" + fmt.Sprint(b64Len) + ":" + head + ":" + tail
}

// ─────────────────────────────────────────────────────────────────────────────
// Per-item inline-image helpers (used by the input parsers)
// ─────────────────────────────────────────────────────────────────────────────

// parseDataImageURI parses a single `data:image/<mime>;base64,<payload>` URI value
// (the unquoted contents of an input_image block's `image_url`). It returns the
// MIME ("image/png"), the base64 payload (a SLICE of uri — no copy), and ok=true
// when uri is a well-formed base64 image data URI. Anything else (https URL,
// non-image data:, non-base64) returns ok=false and is ignored.
//
// O(1)-ish in the prefix length: it validates the fixed "data:" / ";base64," shape
// and that the MIME matches imageMIMERe; it does NOT scan the (large) payload.
func parseDataImageURI(uri string) (mime, b64 string, ok bool) {
	const scheme = "data:"
	if !strings.HasPrefix(uri, scheme) {
		return "", "", false
	}
	rest := uri[len(scheme):] // "image/png;base64,XXXX"
	semi := strings.IndexByte(rest, ';')
	if semi < 0 {
		return "", "", false
	}
	mime = rest[:semi]
	if !imageMIMERe.MatchString(mime) {
		return "", "", false
	}
	const b64marker = ";base64,"
	if !strings.HasPrefix(rest[semi:], b64marker) {
		return "", "", false
	}
	b64 = rest[semi+len(b64marker):]
	if b64 == "" {
		return "", "", false
	}
	return mime, b64, true
}

// inlineImageFingerprint builds the CHEAP dedup fingerprint for an inline image
// from its MIME + base64-payload length + first/last 24 chars — the same scheme as
// inlineKey, but taking the payload as a slice rather than body offsets. O(1) per
// occurrence (only the two 24-char ends are concatenated; the payload is a slice).
// This is folded into the per-item dedup key so messages that differ ONLY by image
// do not wrongly dedup, while staying linear in occurrences.
func inlineImageFingerprint(mime, b64 string) string {
	const edge = 24
	var head, tail string
	if len(b64) <= 2*edge {
		head, tail = b64, ""
	} else {
		head, tail = b64[:edge], b64[len(b64)-edge:]
	}
	return mime + ":" + fmt.Sprint(len(b64)) + ":" + head + ":" + tail
}

// emitInlineImageAttachment does the EXPENSIVE per-image work for a NEW item's
// inline image — exactly once per unique image. It dedups against seenAttachments
// by "inl:"+fingerprint, computes the display-caption sha256 (over the base64
// payload, matching extractInlineImages), and builds a self-contained Attachment
// (DataURI = the full data: URI). It respects the shared inlineBudget identically
// to extractInlineImages: under a bounded budget an oversized image (or one that
// would overflow the total) gets an empty DataURI + a gap note; unlimited budget
// (caps<=0) always inlines.
//
// Returns (attachment, ok, gap): ok=false means the image was already seen (skip);
// gap is "" unless a cap note was produced.
func emitInlineImageAttachment(mime, b64, dataURI, fingerprint string, seenAttachments map[string]bool, budget *inlineBudget) (Attachment, bool, string) {
	key := "inl:" + fingerprint
	if seenAttachments[key] {
		return Attachment{}, false, ""
	}
	seenAttachments[key] = true

	decodedLen := base64.StdEncoding.DecodedLen(len(b64)) // upper bound is fine for display
	sum := sha256.Sum256([]byte(b64))                     // display caption sha (once)
	shaHex := hex.EncodeToString(sum[:])

	att := Attachment{
		SHA256: shaHex,
		MIME:   mime,
		Bytes:  decodedLen,
		// No ProxyPath: inline images are self-contained; DataURI set below
		// (possibly skipped under a bounded budget — see caps).
	}

	// Per-image size cap (bounded budget only).
	if budget != nil && budget.perImageMax > 0 && decodedLen > budget.perImageMax {
		return att, true, fmt.Sprintf(
			"inline image not inlined (exceeds %d MiB cap): sha=%s bytes=%d",
			budget.perImageMax>>20, shaHex, decodedLen)
	}

	// Total budget cap (bounded budget only) — emit the exhaustion note once.
	if budget != nil && budget.totalMax > 0 && budget.used+decodedLen > budget.totalMax {
		var gap string
		if !budget.exhausted {
			budget.exhausted = true
			gap = "inline image budget exhausted; remaining images shown as links only"
		}
		return att, true, gap
	}

	att.DataURI = dataURI
	if budget != nil {
		budget.used += decodedLen
	}
	return att, true, ""
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

	// seenAttachments dedups image attachments (pointer + inline data:image)
	// across turns — see assembleTurn. Created once, shared across all turns.
	seenAttachments := make(map[string]bool)

	budget := &inlineBudget{
		perImageMax: maxInlineImageBytes,
		totalMax:    maxTotalInlineBytes,
	}

	var turns []Turn
	var allGaps []string
	outputTruncated := false

	for idx, row := range rows {
		turn, gaps := assembleTurn(ctx, row, idx+1, resolver, seenHashes, seenAttachments, responseIDSet, budget)
		turns = append(turns, turn)
		allGaps = append(allGaps, gaps...)
		if row.OutputTruncated {
			outputTruncated = true
		}
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

// parseUserItems extracts new input items from a resolved body, AND the inline
// data:image attachments carried by those NEW items.
//
// Codex /v1/responses (and chat) input is cumulative: turn N re-contains every
// prior message and every prior inline image. Item dedup already iterates that
// cumulative input to find which items are NEW; we piggy-back inline-image
// extraction on that single pass so the expensive per-image work (slice the
// data: URI, sha256 for the display caption) runs ONLY for new items — each
// unique image once (linear), instead of a separate whole-body base64 re-scan
// every turn (quadratic in turn count).
//
//   - For /v1/responses: dedup by content-hash + a cheap per-image fingerprint
//     (item.id is absent on Codex inputs).
//   - For chat/completions: same, over messages[].content[].
//   - Other endpoints: a single raw Item, no inline-image extraction.
//
// Returns (items, attachments, gaps). gaps carries the same inline-budget notes
// (oversized image / budget exhausted) the input path emitted before.
func parseUserItems(endpoint, body string, seenHashes, seenAttachments map[string]bool, budget *inlineBudget) ([]Item, []Attachment, []string) {
	switch {
	case strings.Contains(endpoint, "/responses"):
		return parseResponsesInputItems(body, seenHashes, seenAttachments, budget)
	case strings.Contains(endpoint, "/chat/completions"):
		return parseChatInputItems(body, seenHashes, seenAttachments, budget)
	default:
		return parseRawInputItem(body), nil, nil
	}
}

// itemHash returns a stable content-based key for deduplication across turns.
// Uses \x00 as a field separator (never appears in UTF-8 text fields).
func itemHash(it Item) string {
	return it.Role + "\x00" + it.Type + "\x00" + it.Text + "\x00" + it.ToolName + "\x00" + it.ToolArgs + "\x00" + it.ToolOutput
}

// inlineImg holds one parsed inline data:image carried by a message, alongside its
// cheap fingerprint. Kept tiny: mime/b64/dataURI are SLICES of the body (no copies).
type inlineImg struct {
	mime        string
	b64         string
	dataURI     string
	fingerprint string
}

// parseResponsesInputItems parses the OpenAI Responses API request body.
//
// It handles three cases for the "input" field:
//   - absent / unknown structure → raw fallback
//   - string → single user message
//   - array → iterate items via responseItemToItemWithImages
//
// It also reads the top-level "instructions" string, which carries the
// system/developer prompt, and emits it as a system role item.
//
// All items are deduplicated across turns via seenHashes; the dedup key is the
// item content-hash FOLDED with each inline image's cheap fingerprint, so two
// messages with identical text but different images are NOT collapsed. Inline
// images of NEW items are emitted as deduped self-contained attachments here (see
// parseUserItems for why this is linear).
func parseResponsesInputItems(body string, seenHashes, seenAttachments map[string]bool, budget *inlineBudget) ([]Item, []Attachment, []string) {
	var items []Item
	var atts []Attachment
	var gaps []string

	// emit applies image-aware cross-turn dedup, then (for NEW items) does the
	// expensive per-image work exactly once. imgs is nil for non-message items.
	emit := func(it Item, imgs []inlineImg) {
		key := itemDedupKey(it, imgs)
		if seenHashes[key] {
			return
		}
		seenHashes[key] = true
		items = append(items, it)
		for _, im := range imgs {
			att, ok, gap := emitInlineImageAttachment(im.mime, im.b64, im.dataURI, im.fingerprint, seenAttachments, budget)
			if ok {
				atts = append(atts, att)
			}
			if gap != "" {
				gaps = append(gaps, gap)
			}
		}
	}

	// 1. Top-level "instructions" → system prompt.
	if instructions := strings.TrimSpace(gjson.Get(body, "instructions").String()); instructions != "" {
		emit(Item{Role: "system", Type: "message", Text: instructions}, nil)
	}

	// 2. "input" field.
	input := gjson.Get(body, "input")
	switch {
	case !input.Exists():
		// No input field at all; return what we have (may just be instructions).
		if len(items) == 0 {
			return parseRawInputItem(body), nil, nil
		}
	case input.Type == gjson.String:
		// Plain string input (e.g. simple "say hi in one word").
		if s := input.String(); s != "" {
			emit(Item{Role: "user", Type: "message", Text: s}, nil)
		}
	case input.IsArray():
		input.ForEach(func(_, elem gjson.Result) bool {
			it, imgs := responseItemToItemWithImages(elem)
			emit(it, imgs)
			return true
		})
	default:
		// Unexpected input shape; raw fallback for that field.
		emit(Item{Type: "raw", Text: input.Raw}, nil)
	}

	return items, atts, gaps
}

// itemDedupKey is the cross-turn dedup key: the item content-hash folded with each
// inline image's cheap fingerprint. Text-only items (imgs empty) reduce to exactly
// itemHash(it), preserving prior behavior bit-for-bit.
func itemDedupKey(it Item, imgs []inlineImg) string {
	h := itemHash(it)
	if len(imgs) == 0 {
		return h
	}
	var b strings.Builder
	b.WriteString(h)
	for _, im := range imgs {
		b.WriteByte(0)
		b.WriteString(im.fingerprint)
	}
	return b.String()
}

// gatherInlineImages walks a Responses/chat message `content` array and returns one
// inlineImg per input_image (or image_url) block carrying a base64 data:image URI.
// The image_url value is read via .Raw (a SLICE of the body, no 470 KB .String()
// copy); only well-formed base64 image data URIs are kept. Returns nil if content
// is not an array or carries no inline images (the common text-only case).
func gatherInlineImages(content gjson.Result) []inlineImg {
	if !content.IsArray() {
		return nil
	}
	var imgs []inlineImg
	content.ForEach(func(_, block gjson.Result) bool {
		uri := rawImageURL(block.Get("image_url"))
		if uri == "" {
			return true
		}
		mime, b64, ok := parseDataImageURI(uri)
		if !ok {
			return true
		}
		imgs = append(imgs, inlineImg{
			mime:        mime,
			b64:         b64,
			dataURI:     uri,
			fingerprint: inlineImageFingerprint(mime, b64),
		})
		return true
	})
	return imgs
}

// rawImageURL returns the inline `image_url` value as a SLICE of the body (no copy)
// when it is a plain JSON string. It handles both shapes seen in the wild:
//   - responses input_image: "image_url" is the string itself
//   - chat image_url part:   "image_url" is {"url": "...."}
// Non-string / absent values return "". gjson's .Raw on a string is the quoted,
// possibly-escaped JSON literal; base64 data URIs contain none of the JSON escape
// metacharacters, so stripping the surrounding quotes yields the exact value
// without unescaping or allocating.
func rawImageURL(imageURL gjson.Result) string {
	v := imageURL
	if v.IsObject() {
		v = v.Get("url")
	}
	if v.Type != gjson.String {
		return ""
	}
	raw := v.Raw // includes surrounding quotes
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		inner := raw[1 : len(raw)-1]
		// Only safe to use the raw slice when there are no escapes; data: image URIs
		// never contain '\'. If an escape is present (not an image data URI we care
		// about), fall back to the decoded string.
		if !strings.ContainsRune(inner, '\\') {
			return inner
		}
		return v.String()
	}
	return v.String()
}

// responseItemToItemWithImages maps a single JSON object from `input[]` to an Item
// and, for message items, the inline data:image attachments carried by its content
// (images stay OUT of Item.Text — they render via the deduped Attachments section).
// Non-message items return a nil image slice.
func responseItemToItemWithImages(elem gjson.Result) (Item, []inlineImg) {
	itemType := elem.Get("type").String()
	// Codex input messages frequently OMIT "type" — they are identified by a
	// "role" + "content" (e.g. {"role":"user","content":[{"type":"input_text",…}]}).
	// Treat type=="message" OR (no type + has role) as a message.
	if itemType == "message" || (itemType == "" && elem.Get("role").Exists()) {
		role := elem.Get("role").String()
		if role == "" {
			role = "user"
		}
		content := elem.Get("content")
		return Item{Role: role, Type: "message", Text: extractResponsesContentText(content)}, gatherInlineImages(content)
	}
	return responseItemToItem(elem), nil
}

// responseItemToItem maps a single non-message-or-any JSON object from `input[]`
// to an Item. Kept as a standalone for the non-message item types; message items
// go through responseItemToItemWithImages (which also extracts inline images).
func responseItemToItem(elem gjson.Result) Item {
	itemType := elem.Get("type").String()
	if itemType == "message" || (itemType == "" && elem.Get("role").Exists()) {
		role := elem.Get("role").String()
		if role == "" {
			role = "user"
		}
		return Item{Role: role, Type: "message", Text: extractResponsesContentText(elem.Get("content"))}
	}
	switch itemType {
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
// messages, we dedup on an image-aware key (itemHash folded with each inline
// image's fingerprint) keyed against the shared seenHashes map. Inline images
// (messages[].content[] image_url parts carrying base64 data URIs) of NEW
// messages are emitted as deduped self-contained attachments — chat history is
// also cumulative, so this keeps inline-image extraction linear (see parseUserItems).
func parseChatInputItems(body string, seenHashes, seenAttachments map[string]bool, budget *inlineBudget) ([]Item, []Attachment, []string) {
	messagesArr := gjson.Get(body, "messages")
	if !messagesArr.Exists() || !messagesArr.IsArray() {
		return parseRawInputItem(body), nil, nil
	}

	var items []Item
	var atts []Attachment
	var gaps []string
	messagesArr.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := msg.Get("content")
		it := Item{Role: role, Type: "message", Text: extractChatContent(content)}
		imgs := gatherInlineImages(content)

		key := itemDedupKey(it, imgs)
		if seenHashes[key] {
			return true // already shown in a previous turn
		}
		seenHashes[key] = true
		items = append(items, it)
		for _, im := range imgs {
			att, ok, gap := emitInlineImageAttachment(im.mime, im.b64, im.dataURI, im.fingerprint, seenAttachments, budget)
			if ok {
				atts = append(atts, att)
			}
			if gap != "" {
				gaps = append(gaps, gap)
			}
		}
		return true
	})
	return items, atts, gaps
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
