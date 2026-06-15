package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// testBlobStore is a fake PayloadAuditBlobStore used in blobresolver tests.
type testBlobStore struct {
	data map[string][]byte
	err  error // non-nil: Get always returns this error
}

func (s *testBlobStore) Put(_ context.Context, _ string, _ []byte, _ string) error { return nil }

func (s *testBlobStore) Get(_ context.Context, key string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.data[key]
	if !ok {
		return nil, errors.New("key not found: " + key)
	}
	return v, nil
}

// makeBodyKey returns the object-store key for a body SHA using the same logic as blobresolver.
func makeBodyKey(prefix, sha string) string { return bodyKey(prefix, sha) }

// ---------- body pointer tests ----------

func TestBlobResolver_BodyPointer_ReplacesWithContent(t *testing.T) {
	const prefix = "payload-audit"
	const bodySHA = "abc123"
	const originalText = `{"messages":[{"role":"user","content":"hello"}]}`

	key := makeBodyKey(prefix, bodySHA)
	store := &testBlobStore{data: map[string][]byte{key: []byte(originalText)}}
	r := NewBlobResolver(store, prefix)

	// Body value stored as a JSON string value: the pointer sits between quotes.
	bodyPointer := EncodeBodyPointer(bodySHA, len(originalText))
	// Simulate a stored body where the field value IS the pointer (enclosed in quotes by the parent JSON).
	input := bodyPointer

	resolved, atts, gaps := r.ResolveBody(context.Background(), input)

	if resolved != originalText {
		t.Fatalf("expected resolved body = %q, got %q", originalText, resolved)
	}
	if len(atts) != 0 {
		t.Fatalf("expected no attachments, got %v", atts)
	}
	if len(gaps) != 0 {
		t.Fatalf("expected no gaps, got %v", gaps)
	}
}

func TestBlobResolver_BodyPointer_DownloadError_Gap(t *testing.T) {
	const prefix = "payload-audit"
	const bodySHA = "deadbeef"
	store := &testBlobStore{err: errors.New("network timeout")}
	r := NewBlobResolver(store, prefix)

	bodyPointer := EncodeBodyPointer(bodySHA, 100)
	resolved, atts, gaps := r.ResolveBody(context.Background(), bodyPointer)

	if !strings.Contains(resolved, "[s2a-body unavailable sha="+bodySHA+"]") {
		t.Fatalf("expected unavailable placeholder, got %q", resolved)
	}
	if len(atts) != 0 {
		t.Fatalf("expected no attachments, got %v", atts)
	}
	if len(gaps) != 1 {
		t.Fatalf("expected 1 gap, got %v", gaps)
	}
}

// ---------- blob pointer tests ----------

func TestBlobResolver_BlobPointer_PlaceholderAndAttachment(t *testing.T) {
	const prefix = "payload-audit"
	const blobSHA = "ff00aa"
	const mime = "image/png"
	const size = 4096
	store := &testBlobStore{data: map[string][]byte{}}
	r := NewBlobResolver(store, prefix)

	blobPointer := EncodeBlobPointer(blobSHA, mime, size)
	resolved, atts, gaps := r.ResolveBody(context.Background(), blobPointer)

	if !strings.Contains(resolved, "[image") {
		t.Fatalf("expected image placeholder in resolved body, got %q", resolved)
	}
	if len(gaps) != 0 {
		t.Fatalf("expected no gaps, got %v", gaps)
	}
	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %v", atts)
	}
	att := atts[0]
	if att.SHA256 != blobSHA {
		t.Fatalf("attachment SHA256 = %q, want %q", att.SHA256, blobSHA)
	}
	if att.MIME != mime {
		t.Fatalf("attachment MIME = %q, want %q", att.MIME, mime)
	}
	if att.Bytes != size {
		t.Fatalf("attachment Bytes = %d, want %d", att.Bytes, size)
	}
	if att.ProxyPath != "blobs/"+blobSHA {
		t.Fatalf("attachment ProxyPath = %q, want %q", att.ProxyPath, "blobs/"+blobSHA)
	}
}

func TestBlobResolver_BlobPointer_NoDownloadAttempted(t *testing.T) {
	// store always errors on Get — blob pointer must NOT trigger a download.
	const prefix = "payload-audit"
	store := &testBlobStore{err: errors.New("should not be called")}
	r := NewBlobResolver(store, prefix)

	blobPointer := EncodeBlobPointer("sha999", "image/jpeg", 512)
	_, _, gaps := r.ResolveBody(context.Background(), blobPointer)

	if len(gaps) != 0 {
		t.Fatalf("blob pointer must not download or produce gaps, got %v", gaps)
	}
}

// ---------- mixed / edge cases ----------

func TestBlobResolver_NilStore_ReturnsBodyUnchanged(t *testing.T) {
	r := &BlobResolver{store: nil, prefix: "p"}
	body := "no pointers here"
	resolved, atts, gaps := r.ResolveBody(context.Background(), body)
	if resolved != body || len(atts) != 0 || len(gaps) != 0 {
		t.Fatalf("unexpected output: resolved=%q atts=%v gaps=%v", resolved, atts, gaps)
	}
}

func TestBlobResolver_NoPointers_ReturnsBodyUnchanged(t *testing.T) {
	store := &testBlobStore{data: map[string][]byte{}}
	r := NewBlobResolver(store, "payload-audit")
	body := `{"role":"user","content":"hello world"}`
	resolved, atts, gaps := r.ResolveBody(context.Background(), body)
	if resolved != body || len(atts) != 0 || len(gaps) != 0 {
		t.Fatalf("unexpected output for no-pointer body: resolved=%q", resolved)
	}
}

func TestBlobResolver_MultiplePointers(t *testing.T) {
	const prefix = "payload-audit"
	const bodySHA = "body111"
	const blobSHA = "blob222"
	const originalText = "restored text content"

	key := makeBodyKey(prefix, bodySHA)
	store := &testBlobStore{data: map[string][]byte{key: []byte(originalText)}}
	r := NewBlobResolver(store, prefix)

	bodyPointer := EncodeBodyPointer(bodySHA, len(originalText))
	blobPointer := EncodeBlobPointer(blobSHA, "image/png", 128)

	// Both pointers together separated by a space (non-JSON context for simplicity).
	input := bodyPointer + " " + blobPointer

	resolved, atts, gaps := r.ResolveBody(context.Background(), input)

	if !strings.Contains(resolved, originalText) {
		t.Fatalf("expected restored text in resolved, got %q", resolved)
	}
	if !strings.Contains(resolved, "[image") {
		t.Fatalf("expected blob placeholder in resolved, got %q", resolved)
	}
	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(atts))
	}
	if len(gaps) != 0 {
		t.Fatalf("expected no gaps, got %v", gaps)
	}
}
