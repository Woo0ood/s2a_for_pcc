package service

// White-box tests for cross-turn attachment dedup and inline data:image ingestion.
//
// Codex /v1/responses bodies are cumulative: each turn re-sends ALL prior history
// including images. Without cross-turn attachment dedup, the same image is emitted
// once per turn (N turns → N copies → giant HTML). These tests pin:
//   1. pointer (offloaded blob) attachments dedup by sha across turns,
//   2. inline data:image attachments are ingested (previously dropped) and dedup
//      across turns by a cheap fingerprint,
//   3. the same holds through the streaming path (StreamingAssembler).

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeDedupRow builds a minimal /v1/responses event whose InputBody is the given
// raw JSON; OutputBody is empty.
func makeDedupRow(idx int, inputBody string) *PayloadAuditEvent {
	return &PayloadAuditEvent{
		ID:           int64(idx),
		Endpoint:     "/v1/responses",
		InputBody:    inputBody,
		OutputBody:   "",
		OutputFormat: "json",
		StatusCode:   200,
		Model:        "gpt-4o",
		CreatedAt:    time.Now().Add(time.Duration(idx) * time.Minute),
	}
}

// countAttachmentsBySHA tallies, across all turns, how many attachments carry each
// SHA256. Used to assert an image renders exactly once total.
func countAttachmentsBySHA(tr Transcript) map[string]int {
	counts := make(map[string]int)
	for _, turn := range tr.Turns {
		for _, att := range turn.Attachments {
			counts[att.SHA256]++
		}
	}
	return counts
}

// ─────────────────────────────────────────────────────────────────────────────
// Offload (pointer) attachment dedup across turns
// ─────────────────────────────────────────────────────────────────────────────

// Three cumulative turns each carry the SAME blob pointer (image/png). The image
// must appear as an Attachment exactly ONCE across the whole transcript, not 3×.
// A second distinct sha appearing from turn 2 on must also appear exactly once →
// 2 unique attachments total.
func TestAssembleTranscript_OffloadAttachment_DedupAcrossTurns(t *testing.T) {
	const prefix = "payload-audit"
	const shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const mime = "image/png"
	pngA := []byte("\x89PNG\r\n\x1a\nAAAA-fake-png")
	pngB := []byte("\x89PNG\r\n\x1a\nBBBB-fake-png")

	store := &testBlobStore{data: map[string][]byte{
		blobKey(prefix, shaA): pngA,
		blobKey(prefix, shaB): pngB,
	}}
	resolver := NewBlobResolver(store, prefix)

	ptrA := EncodeBlobPointer(shaA, mime, len(pngA))
	ptrB := EncodeBlobPointer(shaB, mime, len(pngB))

	// Turn 1: A. Turn 2: A (repeat) + B. Turn 3: A + B (both repeat).
	rows := []*PayloadAuditEvent{
		makeDedupRow(1, `{"input":[{"role":"user","content":[{"type":"input_text","text":"`+ptrA+`"}]}]}`),
		makeDedupRow(2, `{"input":[{"role":"user","content":[{"type":"input_text","text":"`+ptrA+`"}]},{"role":"user","content":[{"type":"input_text","text":"`+ptrB+`"}]}]}`),
		makeDedupRow(3, `{"input":[{"role":"user","content":[{"type":"input_text","text":"`+ptrA+`"}]},{"role":"user","content":[{"type":"input_text","text":"`+ptrB+`"}]}]}`),
	}

	tr := AssembleTranscript(context.Background(), rows, resolver)

	counts := countAttachmentsBySHA(tr)
	if counts[shaA] != 1 {
		t.Errorf("image A: expected exactly 1 attachment across all turns, got %d", counts[shaA])
	}
	if counts[shaB] != 1 {
		t.Errorf("image B: expected exactly 1 attachment across all turns, got %d", counts[shaB])
	}

	total := 0
	for _, turn := range tr.Turns {
		total += len(turn.Attachments)
	}
	if total != 2 {
		t.Errorf("expected 2 unique attachments total across transcript, got %d", total)
	}

	// The surviving attachment must still be inlined (DataURI set) since it's an image.
	for _, turn := range tr.Turns {
		for _, att := range turn.Attachments {
			if !strings.HasPrefix(att.DataURI, "data:image/png;base64,") {
				t.Errorf("attachment sha=%s expected inlined DataURI, got %q", att.SHA256, att.DataURI)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Inline data:image ingestion + dedup across turns
// ─────────────────────────────────────────────────────────────────────────────

// Three cumulative turns each contain the SAME inline data:image/png;base64,…
// It must render exactly ONCE as an attachment whose DataURI is the inline base64
// itself (self-contained, no S3 link). Two distinct inline images → 2 unique.
func TestAssembleTranscript_InlineDataImage_IngestAndDedupAcrossTurns(t *testing.T) {
	const prefix = "payload-audit"
	// Resolver present but blob store empty: inline images must NOT touch the store.
	store := &testBlobStore{data: map[string][]byte{}}
	resolver := NewBlobResolver(store, prefix)

	imgA := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nAAAA-inline-image-payload-aaaaaaaaaaaaaaaa"))
	imgB := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nBBBB-inline-image-payload-bbbbbbbbbbbbbbbb"))
	dataURIA := "data:image/png;base64," + imgA
	dataURIB := "data:image/png;base64," + imgB

	block := func(uri string) string {
		return `{"type":"input_image","image_url":"` + uri + `","detail":"high"}`
	}

	rows := []*PayloadAuditEvent{
		makeDedupRow(1, `{"input":[{"role":"user","content":[`+block(dataURIA)+`]}]}`),
		makeDedupRow(2, `{"input":[{"role":"user","content":[`+block(dataURIA)+`]},{"role":"user","content":[`+block(dataURIB)+`]}]}`),
		makeDedupRow(3, `{"input":[{"role":"user","content":[`+block(dataURIA)+`]},{"role":"user","content":[`+block(dataURIB)+`]}]}`),
	}

	tr := AssembleTranscript(context.Background(), rows, resolver)

	// Compute expected display shas (sha256 over the base64 payload string).
	shaA := hexSHAOf(imgA)
	shaB := hexSHAOf(imgB)

	counts := countAttachmentsBySHA(tr)
	if counts[shaA] != 1 {
		t.Errorf("inline image A: expected exactly 1 attachment across all turns, got %d", counts[shaA])
	}
	if counts[shaB] != 1 {
		t.Errorf("inline image B: expected exactly 1 attachment across all turns, got %d", counts[shaB])
	}

	total := 0
	var sawA, sawB bool
	for _, turn := range tr.Turns {
		total += len(turn.Attachments)
		for _, att := range turn.Attachments {
			// Self-contained: DataURI is the inline base64 itself, ProxyPath empty.
			if att.ProxyPath != "" {
				t.Errorf("inline attachment sha=%s should have NO ProxyPath (self-contained), got %q", att.SHA256, att.ProxyPath)
			}
			switch att.SHA256 {
			case shaA:
				sawA = true
				if att.DataURI != dataURIA {
					t.Errorf("inline image A DataURI mismatch:\n got  %q\n want %q", att.DataURI, dataURIA)
				}
			case shaB:
				sawB = true
				if att.DataURI != dataURIB {
					t.Errorf("inline image B DataURI mismatch:\n got  %q\n want %q", att.DataURI, dataURIB)
				}
			}
		}
	}
	if total != 2 {
		t.Errorf("expected 2 unique inline attachments total, got %d", total)
	}
	if !sawA || !sawB {
		t.Errorf("expected both inline images to render (sawA=%v sawB=%v)", sawA, sawB)
	}
}

// hexSHAOf returns the lowercase-hex sha256 of s (matches the display caption sha
// computed by extractInlineImages over the base64 payload string).
func hexSHAOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ─────────────────────────────────────────────────────────────────────────────
// Image-aware item dedup: same message TEXT but DIFFERENT inline image
// ─────────────────────────────────────────────────────────────────────────────
//
// Inline images now flow through the per-item dedup (the item key folds in a
// cheap image fingerprint). Two cumulative turns whose user messages carry the
// SAME text but a DIFFERENT inline image must NOT collapse to one item — both
// images must be emitted. Conversely, same text + same image across turns is one
// item and one attachment. This pins that the image fingerprint is part of the
// dedup key (text-only dedup would wrongly drop the second image).
func TestAssembleTranscript_ImageAwareItemDedup_SameTextDifferentImage(t *testing.T) {
	const prefix = "payload-audit"
	store := &testBlobStore{data: map[string][]byte{}}
	resolver := NewBlobResolver(store, prefix)

	imgA := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nFIRST-image-payload-aaaaaaaaaaaaaaaaaaaa"))
	imgB := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nSECOND-image-payload-bbbbbbbbbbbbbbbbbbbb"))
	dataURIA := "data:image/png;base64," + imgA
	dataURIB := "data:image/png;base64," + imgB

	// SAME text in every message; only the image differs.
	const sameText = "describe this screenshot"
	msg := func(uri string) string {
		return `{"role":"user","content":[` +
			`{"type":"input_text","text":"` + sameText + `"},` +
			`{"type":"input_image","image_url":"` + uri + `","detail":"high"}` +
			`]}`
	}

	// Turn 1: msg(A). Turn 2 (cumulative): msg(A) + msg(B) — msg(A) is a repeat,
	// msg(B) is new and shares text with msg(A) but carries a different image.
	rows := []*PayloadAuditEvent{
		makeDedupRow(1, `{"input":[`+msg(dataURIA)+`]}`),
		makeDedupRow(2, `{"input":[`+msg(dataURIA)+`,`+msg(dataURIB)+`]}`),
	}

	tr := AssembleTranscript(context.Background(), rows, resolver)

	shaA := hexSHAOf(imgA)
	shaB := hexSHAOf(imgB)

	counts := countAttachmentsBySHA(tr)
	if counts[shaA] != 1 {
		t.Errorf("image A: expected exactly 1 attachment across turns, got %d", counts[shaA])
	}
	if counts[shaB] != 1 {
		t.Errorf("image B (same text, different image): expected exactly 1 attachment, got %d", counts[shaB])
	}

	totalAtts := 0
	for _, turn := range tr.Turns {
		totalAtts += len(turn.Attachments)
	}
	if totalAtts != 2 {
		t.Errorf("expected 2 unique attachments total (A and B), got %d", totalAtts)
	}

	// Turn 1 emits the first message (text + image A). Turn 2 emits only the NEW
	// message (text + image B) — the repeated msg(A) is deduped away.
	if got := len(tr.Turns[0].UserItems); got != 1 {
		t.Errorf("turn 1: expected 1 user item, got %d", got)
	}
	if got := len(tr.Turns[1].UserItems); got != 1 {
		t.Errorf("turn 2: expected 1 NEW user item (the differing-image message), got %d", got)
	}
	// The surviving item text is identical in both turns (image lives in the attachment).
	if tr.Turns[0].UserItems[0].Text != sameText {
		t.Errorf("turn 1 item text = %q, want %q", tr.Turns[0].UserItems[0].Text, sameText)
	}
	if len(tr.Turns[1].UserItems) == 1 && tr.Turns[1].UserItems[0].Text != sameText {
		t.Errorf("turn 2 item text = %q, want %q", tr.Turns[1].UserItems[0].Text, sameText)
	}
}

// Same text AND same image across turns → exactly one item and one attachment.
func TestAssembleTranscript_ImageAwareItemDedup_SameTextSameImage(t *testing.T) {
	const prefix = "payload-audit"
	store := &testBlobStore{data: map[string][]byte{}}
	resolver := NewBlobResolver(store, prefix)

	img := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nidentical-image-cccccccccccccccccccccccc"))
	dataURI := "data:image/png;base64," + img
	const sameText = "what is in this image"
	msg := `{"role":"user","content":[` +
		`{"type":"input_text","text":"` + sameText + `"},` +
		`{"type":"input_image","image_url":"` + dataURI + `"}` +
		`]}`

	rows := []*PayloadAuditEvent{
		makeDedupRow(1, `{"input":[`+msg+`]}`),
		makeDedupRow(2, `{"input":[`+msg+`,`+msg+`]}`), // cumulative repeat
		makeDedupRow(3, `{"input":[`+msg+`]}`),
	}

	tr := AssembleTranscript(context.Background(), rows, resolver)

	sha := hexSHAOf(img)
	if c := countAttachmentsBySHA(tr)[sha]; c != 1 {
		t.Errorf("identical image across turns: expected exactly 1 attachment, got %d", c)
	}
	totalItems := 0
	for _, turn := range tr.Turns {
		totalItems += len(turn.UserItems)
	}
	if totalItems != 1 {
		t.Errorf("identical message across turns: expected exactly 1 user item total, got %d", totalItems)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Linearity proxy: unique-image count is independent of turn count
// ─────────────────────────────────────────────────────────────────────────────
//
// Codex /v1/responses input is cumulative — turn N re-contains all prior images.
// The expensive per-image work must happen ONLY for NEW items, so the total
// attachment count must equal the number of UNIQUE images, regardless of how many
// turns re-send them. This asserts that property over a long synthetic history:
// each turn adds exactly one new image while re-sending all previous ones, so N
// turns yield exactly N unique attachments (and each is emitted exactly once).
func TestAssembleTranscript_InlineImage_LinearAcrossCumulativeTurns(t *testing.T) {
	const prefix = "payload-audit"
	store := &testBlobStore{data: map[string][]byte{}}
	resolver := NewBlobResolver(store, prefix)

	const nTurns = 40
	// Pre-build the per-turn message blocks (cumulative). Each image's bytes differ
	// at the LEADING edge (a per-image unique tag) so distinct images are reliably
	// distinguished — mirroring real screenshots, which differ throughout, not just
	// in one interior byte.
	blocks := make([]string, 0, nTurns)
	shas := make([]string, 0, nTurns)
	for i := 0; i < nTurns; i++ {
		tag := "IMG" + strconv.Itoa(i) + "x" + strings.Repeat("z", i%5)
		raw := []byte("\x89PNG\r\n\x1a\n" + tag + "-unique-image-payload-" + strconv.Itoa(i*7919))
		b64 := base64.StdEncoding.EncodeToString(raw)
		uri := "data:image/png;base64," + b64
		blocks = append(blocks, `{"role":"user","content":[{"type":"input_image","image_url":"`+uri+`"}]}`)
		shas = append(shas, hexSHAOf(b64))
	}

	rows := make([]*PayloadAuditEvent, 0, nTurns)
	for i := 0; i < nTurns; i++ {
		// Turn i carries blocks[0..i] — the full cumulative history so far.
		body := `{"input":[` + strings.Join(blocks[:i+1], ",") + `]}`
		rows = append(rows, makeDedupRow(i+1, body))
	}

	tr := AssembleTranscript(context.Background(), rows, resolver)

	// Each unique image emitted exactly once.
	counts := countAttachmentsBySHA(tr)
	for i, sha := range shas {
		if counts[sha] != 1 {
			t.Errorf("image #%d (sha=%s): expected exactly 1 attachment, got %d", i, sha, counts[sha])
		}
	}

	// Total attachments == unique images == nTurns, independent of the (quadratic)
	// number of image OCCURRENCES across all cumulative bodies.
	total := 0
	for _, turn := range tr.Turns {
		total += len(turn.Attachments)
	}
	if total != nTurns {
		t.Errorf("expected exactly %d unique attachments (one per unique image), got %d", nTurns, total)
	}

	// Each turn emits exactly its ONE new image as an attachment (prior images were
	// already emitted on first appearance and are deduped away here).
	for i, turn := range tr.Turns {
		if len(turn.Attachments) != 1 {
			t.Errorf("turn %d: expected exactly 1 new attachment, got %d", i+1, len(turn.Attachments))
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Inline images in NON-message items (tool results + embedded-in-code)
// ─────────────────────────────────────────────────────────────────────────────
//
// data:image URIs do not only appear in message input_image blocks. They also show
// up in function_call_output tool results (e.g. a screenshot tool) and embedded
// inside function_call arguments (e.g. an apply_patch diff writing a file with an
// inline icon). Each NEW item's raw JSON is scanned, matching the old whole-body
// scan's coverage; a message-only scan (the regression this guards) silently DROPPED
// these. They are also deduped across cumulative re-sends.
func TestAssembleTranscript_InlineImage_NonMessageItems(t *testing.T) {
	const prefix = "payload-audit"
	store := &testBlobStore{data: map[string][]byte{}}
	resolver := NewBlobResolver(store, prefix)

	// (a) image returned by a tool, carried in a function_call_output output array.
	toolImg := base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xffJPEG-tool-result-screenshot-aaaaaaaaaaaa"))
	toolURI := "data:image/jpeg;base64," + toolImg
	fcOutput := `{"type":"function_call_output","call_id":"call_1","output":[` +
		`{"type":"input_image","image_url":"` + toolURI + `"}]}`

	// (b) image embedded inside function_call arguments (NOT in any image_url block).
	codeImg := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nembedded-in-code-appicon-bbbbbbbbbbbb"))
	codeURI := "data:image/png;base64," + codeImg
	fcCall := `{"type":"function_call","name":"apply_patch","arguments":"*** Begin Patch\n+const appIcon = '` + codeURI + `';\n*** End Patch"}`

	userMsg := `{"role":"user","content":[{"type":"input_text","text":"go"}]}`

	body := `{"input":[` + userMsg + `,` + fcCall + `,` + fcOutput + `]}`
	rows := []*PayloadAuditEvent{
		makeDedupRow(1, body),
		makeDedupRow(2, body), // cumulative re-send — must NOT duplicate
	}

	tr := AssembleTranscript(context.Background(), rows, resolver)

	counts := countAttachmentsBySHA(tr)
	if c := counts[hexSHAOf(toolImg)]; c != 1 {
		t.Errorf("tool-output image (function_call_output): expected exactly 1 attachment, got %d", c)
	}
	if c := counts[hexSHAOf(codeImg)]; c != 1 {
		t.Errorf("embedded-in-arguments image (function_call): expected exactly 1 attachment, got %d", c)
	}
	total := 0
	for _, turn := range tr.Turns {
		total += len(turn.Attachments)
	}
	if total != 2 {
		t.Errorf("expected 2 unique attachments (tool image + embedded image), got %d", total)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Inline image respects the bounded per-image cap (in-process fallback path)
// ─────────────────────────────────────────────────────────────────────────────

// Under a bounded budget, an inline image whose decoded size exceeds the per-image
// cap must NOT be inlined (DataURI empty) and must leave a gap note — mirroring the
// oversized-pointer path so the in-process fallback stays bounded.
func TestExtractInlineImages_BoundedPerImageCap(t *testing.T) {
	// Build a base64 payload that decodes to > maxInlineImageBytes.
	raw := make([]byte, maxInlineImageBytes+16)
	for i := range raw {
		raw[i] = byte('a' + i%26)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	body := `{"image_url":"data:image/png;base64,` + b64 + `"}`

	budget := &inlineBudget{perImageMax: maxInlineImageBytes, totalMax: maxTotalInlineBytes}
	seen := make(map[string]bool)

	atts, gaps := extractInlineImages(body, seen, budget)
	if len(atts) != 1 {
		t.Fatalf("expected 1 inline attachment (placeholder), got %d", len(atts))
	}
	if atts[0].DataURI != "" {
		t.Errorf("oversized inline image must NOT be inlined; DataURI=%q", atts[0].DataURI)
	}
	if atts[0].Bytes != len(raw) {
		t.Errorf("Bytes = %d, want decoded length %d", atts[0].Bytes, len(raw))
	}
	var found bool
	for _, g := range gaps {
		if strings.Contains(g, "exceeds") && strings.Contains(g, "MiB cap") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected per-image cap gap note, got %v", gaps)
	}
}

// Under an unlimited budget (worker path: perImageMax=0/totalMax=0) the same
// oversized inline image IS kept (DataURI set).
func TestExtractInlineImages_UnlimitedKeepsLargeImage(t *testing.T) {
	raw := make([]byte, maxInlineImageBytes+16)
	for i := range raw {
		raw[i] = byte('a' + i%26)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	dataURI := "data:image/png;base64," + b64
	body := `{"image_url":"` + dataURI + `"}`

	budget := &inlineBudget{} // unlimited
	seen := make(map[string]bool)

	atts, gaps := extractInlineImages(body, seen, budget)
	if len(atts) != 1 {
		t.Fatalf("expected 1 inline attachment, got %d", len(atts))
	}
	if atts[0].DataURI != dataURI {
		t.Errorf("unlimited budget should keep large inline image DataURI intact")
	}
	if len(gaps) != 0 {
		t.Errorf("unlimited budget should emit no cap gap, got %v", gaps)
	}
}

// extractInlineImages must dedup repeated occurrences within a SINGLE body too,
// keying off the shared seen map.
func TestExtractInlineImages_DedupWithinBody(t *testing.T) {
	img := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nsame-image-data-1234567890abcdef"))
	dataURI := "data:image/png;base64," + img
	body := `{"a":"` + dataURI + `","b":"` + dataURI + `","c":"` + dataURI + `"}`

	budget := &inlineBudget{}
	seen := make(map[string]bool)

	atts, _ := extractInlineImages(body, seen, budget)
	if len(atts) != 1 {
		t.Fatalf("expected 1 deduped inline attachment within body, got %d", len(atts))
	}
	if atts[0].DataURI != dataURI {
		t.Errorf("DataURI mismatch: %q", atts[0].DataURI)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Streaming path: the same cross-turn attachment dedup must hold via Next()
// (the seenAttachments map carries across Next() calls inside StreamingAssembler).
// ─────────────────────────────────────────────────────────────────────────────

func TestStreamingAssembler_AttachmentDedupAcrossTurns(t *testing.T) {
	const prefix = "payload-audit"
	const shaA = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const mime = "image/png"
	pngA := []byte("\x89PNG\r\n\x1a\nstream-dedup-fake-png")

	store := &testBlobStore{data: map[string][]byte{blobKey(prefix, shaA): pngA}}
	resolver := NewBlobResolver(store, prefix)
	ptrA := EncodeBlobPointer(shaA, mime, len(pngA))

	// Same pointer repeated across 3 cumulative turns + a distinct inline image
	// from turn 2 on, to exercise both dedup keys through the streaming map.
	imgB := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nstream-inline-bbbbbbbbbbbbbbbbbbbb"))
	dataURIB := "data:image/png;base64," + imgB
	shaB := hexSHAOf(imgB)

	rows := []*PayloadAuditEvent{
		makeDedupRow(1, `{"input":[{"role":"user","content":[{"type":"input_text","text":"`+ptrA+`"}]}]}`),
		makeDedupRow(2, `{"input":[{"role":"user","content":[{"type":"input_text","text":"`+ptrA+`"},{"type":"input_image","image_url":"`+dataURIB+`"}]}]}`),
		makeDedupRow(3, `{"input":[{"role":"user","content":[{"type":"input_text","text":"`+ptrA+`"},{"type":"input_image","image_url":"`+dataURIB+`"}]}]}`),
	}

	asm := NewStreamingAssembler(resolver, nil, true /* unlimited inline */)

	counts := make(map[string]int)
	for _, row := range rows {
		turn, _ := asm.Next(context.Background(), row)
		for _, att := range turn.Attachments {
			counts[att.SHA256]++
		}
	}

	if counts[shaA] != 1 {
		t.Errorf("pointer image A: expected exactly 1 attachment across streamed turns, got %d", counts[shaA])
	}
	if counts[shaB] != 1 {
		t.Errorf("inline image B: expected exactly 1 attachment across streamed turns, got %d", counts[shaB])
	}
}
