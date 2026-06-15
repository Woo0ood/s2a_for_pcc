package service

// Inline-pass tests for AssembleTranscript: verifies that image blob attachments
// are inlined as data URIs, that per-image and total budget caps are enforced,
// and that non-image MIME types are left as proxy links.

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// makeInlineRow returns a minimal PayloadAuditEvent whose InputBody contains the
// given blob pointer as if it had been stored in a JSON string field.
func makeInlineRow(t *testing.T, endpoint, blobPointer string) *PayloadAuditEvent {
	t.Helper()
	// Wrap the pointer in a minimal JSON body so ResolveBody can scan it.
	return &PayloadAuditEvent{
		ID:           1,
		Endpoint:     endpoint,
		InputBody:    `{"content":"` + blobPointer + `"}`,
		OutputBody:   "",
		OutputFormat: "json",
		StatusCode:   200,
		Model:        "gpt-4o",
		CreatedAt:    time.Now(),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Happy path: image blob is inlined as a data URI
// ─────────────────────────────────────────────────────────────────────────────

func TestAssembleTranscript_InlineImageBlob_DataURI(t *testing.T) {
	const prefix = "payload-audit"
	const blobSHA = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	const mime = "image/png"
	pngBytes := []byte("\x89PNG\r\n\x1a\nhello-fake-png-data")

	key := blobKey(prefix, blobSHA)
	store := &testBlobStore{data: map[string][]byte{key: pngBytes}}
	resolver := NewBlobResolver(store, prefix)

	blobPtr := EncodeBlobPointer(blobSHA, mime, len(pngBytes))
	row := makeInlineRow(t, "/v1/chat/completions", blobPtr)

	tr := AssembleTranscript(context.Background(), []*PayloadAuditEvent{row}, resolver)

	if len(tr.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(tr.Turns))
	}
	atts := tr.Turns[0].Attachments
	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(atts))
	}
	att := atts[0]

	// DataURI must start with the correct scheme.
	if !strings.HasPrefix(att.DataURI, "data:image/png;base64,") {
		t.Fatalf("DataURI = %q, want prefix 'data:image/png;base64,'", att.DataURI)
	}

	// Decode the base64 payload and compare to original bytes.
	encoded := strings.TrimPrefix(att.DataURI, "data:image/png;base64,")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	if string(decoded) != string(pngBytes) {
		t.Fatalf("decoded bytes = %q, want %q", decoded, pngBytes)
	}

	// No gaps should appear for a successful inline.
	if len(tr.Manifest.Gaps) != 0 {
		t.Errorf("unexpected gaps: %v", tr.Manifest.Gaps)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Per-image size cap: Bytes field exceeds maxInlineImageBytes → DataURI empty + gap note
// ─────────────────────────────────────────────────────────────────────────────

func TestAssembleTranscript_InlineImageBlob_ExceedsSizeCap(t *testing.T) {
	const prefix = "payload-audit"
	const blobSHA = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	const mime = "image/jpeg"
	// Set the Bytes field larger than the per-image cap.
	oversizedBytes := maxInlineImageBytes + 1

	store := &testBlobStore{data: map[string][]byte{}} // store won't be hit for oversized images
	resolver := NewBlobResolver(store, prefix)

	blobPtr := EncodeBlobPointer(blobSHA, mime, oversizedBytes)
	row := makeInlineRow(t, "/v1/chat/completions", blobPtr)

	tr := AssembleTranscript(context.Background(), []*PayloadAuditEvent{row}, resolver)

	if len(tr.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(tr.Turns))
	}
	atts := tr.Turns[0].Attachments
	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(atts))
	}
	att := atts[0]

	// DataURI must be empty — not inlined.
	if att.DataURI != "" {
		t.Errorf("DataURI should be empty for oversized image, got %q", att.DataURI)
	}

	// A gap note mentioning the cap must be present.
	var found bool
	for _, g := range tr.Manifest.Gaps {
		if strings.Contains(g, "exceeds") && strings.Contains(g, "MiB cap") && strings.Contains(g, blobSHA) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected size-cap gap note, got: %v", tr.Manifest.Gaps)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Non-image MIME: stays as proxy link (DataURI empty, no gap note)
// ─────────────────────────────────────────────────────────────────────────────

func TestAssembleTranscript_InlineBlob_NonImageMIME_StaysLink(t *testing.T) {
	const prefix = "payload-audit"
	const blobSHA = "0000000000000000000000000000000000000000000000000000000000000001"
	const mime = "application/pdf"
	pdfBytes := []byte("%PDF-1.4 fake")

	key := blobKey(prefix, blobSHA)
	store := &testBlobStore{data: map[string][]byte{key: pdfBytes}}
	resolver := NewBlobResolver(store, prefix)

	blobPtr := EncodeBlobPointer(blobSHA, mime, len(pdfBytes))
	row := makeInlineRow(t, "/v1/chat/completions", blobPtr)

	tr := AssembleTranscript(context.Background(), []*PayloadAuditEvent{row}, resolver)

	if len(tr.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(tr.Turns))
	}
	atts := tr.Turns[0].Attachments
	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(atts))
	}
	att := atts[0]

	// DataURI must remain empty for non-image MIME.
	if att.DataURI != "" {
		t.Errorf("DataURI should be empty for non-image MIME, got %q", att.DataURI)
	}

	// ProxyPath must still be set so the attachment renders as a proxy link.
	if att.ProxyPath == "" {
		t.Errorf("ProxyPath should be set, got empty")
	}

	// No gap note should appear for a non-image skipped silently.
	if len(tr.Manifest.Gaps) != 0 {
		t.Errorf("expected no gaps for non-image skip, got: %v", tr.Manifest.Gaps)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// FetchBlob error → DataURI empty + gap note
// ─────────────────────────────────────────────────────────────────────────────

func TestAssembleTranscript_InlineImageBlob_FetchError_Gap(t *testing.T) {
	const prefix = "payload-audit"
	const blobSHA = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	const mime = "image/webp"

	// Store always errors on Get.
	store := &testBlobStore{data: nil}
	store.err = errFetchFailed{} // use a typed error
	resolver := NewBlobResolver(store, prefix)

	blobPtr := EncodeBlobPointer(blobSHA, mime, 100)
	row := makeInlineRow(t, "/v1/chat/completions", blobPtr)

	tr := AssembleTranscript(context.Background(), []*PayloadAuditEvent{row}, resolver)

	if len(tr.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(tr.Turns))
	}
	atts := tr.Turns[0].Attachments
	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(atts))
	}
	att := atts[0]

	if att.DataURI != "" {
		t.Errorf("DataURI should be empty on fetch error, got %q", att.DataURI)
	}

	var found bool
	for _, g := range tr.Manifest.Gaps {
		if strings.Contains(g, "blob inline fetch failed") && strings.Contains(g, blobSHA) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected fetch-failed gap note, got: %v", tr.Manifest.Gaps)
	}
}

// errFetchFailed is a sentinel error used to simulate FetchBlob failures.
type errFetchFailed struct{}

func (errFetchFailed) Error() string { return "simulated fetch failure" }

// ─────────────────────────────────────────────────────────────────────────────
// Nil resolver: inline pass does not run, DataURI stays empty
// ─────────────────────────────────────────────────────────────────────────────

func TestAssembleTranscript_InlineImageBlob_NilResolver_NoDataURI(t *testing.T) {
	// Without a resolver, ResolveBody is a no-op, so there are no Attachments
	// to begin with. But even if we build rows with pointers, nil resolver is safe.
	const blobSHA = "1111111111111111111111111111111111111111111111111111111111111111"
	blobPtr := EncodeBlobPointer(blobSHA, "image/png", 64)
	row := makeInlineRow(t, "/v1/chat/completions", blobPtr)

	// nil resolver → ResolveBody returns body unchanged, no Attachments collected,
	// inline pass skipped entirely.
	tr := AssembleTranscript(context.Background(), []*PayloadAuditEvent{row}, nil)

	if len(tr.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(tr.Turns))
	}
	// No attachments because resolver is nil.
	if len(tr.Turns[0].Attachments) != 0 {
		t.Errorf("expected no attachments with nil resolver, got %d", len(tr.Turns[0].Attachments))
	}
}
