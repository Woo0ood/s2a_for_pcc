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
