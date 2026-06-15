package service

import "testing"

func TestBlobPointerRoundTrip(t *testing.T) {
	p := EncodeBlobPointer("abc123", "image/png", 16072888)
	if p != "s2a-blob://sha256-abc123?mime=image%2Fpng&bytes=16072888" {
		t.Fatalf("unexpected pointer: %s", p)
	}
	got, ok := ParsePointer(p)
	if !ok || got.Kind != PointerKindBlob || got.SHA256 != "abc123" || got.MIME != "image/png" || got.Bytes != 16072888 {
		t.Fatalf("parse mismatch: %+v ok=%v", got, ok)
	}
}

func TestBodyPointerRoundTrip(t *testing.T) {
	p := EncodeBodyPointer("deadbeef", 999)
	got, ok := ParsePointer(p)
	if !ok || got.Kind != PointerKindBody || got.SHA256 != "deadbeef" || got.Bytes != 999 {
		t.Fatalf("parse mismatch: %+v ok=%v", got, ok)
	}
}

func TestParsePointer_NotAPointer(t *testing.T) {
	for _, s := range []string{"", "hello", "data:image/png;base64,iVBOR", "https://x/y"} {
		if _, ok := ParsePointer(s); ok {
			t.Fatalf("should not parse: %q", s)
		}
	}
}
