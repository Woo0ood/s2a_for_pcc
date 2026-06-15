package service

import (
	"encoding/base64"
	"strings"
	"testing"
)

func dataURI(mime string, raw []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

func TestRewriteInlineBlobs_ReplacesOverThreshold(t *testing.T) {
	raw := make([]byte, 1024)
	for i := range raw {
		raw[i] = byte(i)
	}
	body := []byte(`{"image_url":"` + dataURI("image/png", raw) + `"}`)
	out, blobs := RewriteInlineBlobs(body, 512)
	if len(blobs) != 1 {
		t.Fatalf("want 1 blob, got %d", len(blobs))
	}
	if blobs[0].MIME != "image/png" || blobs[0].Bytes != 1024 {
		t.Fatalf("blob meta wrong: %+v", blobs[0])
	}
	if !strings.Contains(string(out), "s2a-blob://sha256-"+blobs[0].SHA256) {
		t.Fatalf("pointer not in output: %s", out)
	}
	if strings.Contains(string(out), "base64,") {
		t.Fatalf("base64 still present: %s", out)
	}
}

func TestRewriteInlineBlobs_KeepsUnderThreshold(t *testing.T) {
	body := []byte(`{"image_url":"` + dataURI("image/png", []byte("tiny")) + `"}`)
	out, blobs := RewriteInlineBlobs(body, 512)
	if len(blobs) != 0 || string(out) != string(body) {
		t.Fatalf("small blob should be untouched; blobs=%d", len(blobs))
	}
}

func TestRewriteInlineBlobs_MultipleAndNonImage(t *testing.T) {
	a := make([]byte, 800)
	b := make([]byte, 800)
	body := []byte(`{"a":"` + dataURI("image/jpeg", a) + `","b":"` + dataURI("application/pdf", b) + `"}`)
	out, blobs := RewriteInlineBlobs(body, 512)
	if len(blobs) != 2 {
		t.Fatalf("want 2 blobs, got %d", len(blobs))
	}
	if !strings.HasPrefix(string(out), `{"a":"s2a-blob://`) {
		t.Fatalf("first not replaced: %s", out)
	}
}

func TestRewriteInlineBlobs_InvalidBase64Skipped(t *testing.T) {
	body := []byte(`{"x":"data:image/png;base64,` + strings.Repeat("A", 801) + `"}`)
	_, blobs := RewriteInlineBlobs(body, 100)
	if len(blobs) != 0 {
		t.Fatalf("invalid base64 must be skipped, got %d", len(blobs))
	}
}

func TestRewriteInlineBlobs_SameContentSameSHA(t *testing.T) {
	raw := make([]byte, 1024)
	b1 := []byte(`{"x":"` + dataURI("image/png", raw) + `"}`)
	b2 := []byte(`{"y":"` + dataURI("image/png", raw) + `"}`)
	_, blobs1 := RewriteInlineBlobs(b1, 512)
	_, blobs2 := RewriteInlineBlobs(b2, 512)
	if blobs1[0].SHA256 != blobs2[0].SHA256 {
		t.Fatal("same content should yield same sha")
	}
}
