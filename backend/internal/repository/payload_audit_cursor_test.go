package repository

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestDecodeCursor_RejectsWrongSchemaVer(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"v":  99,
		"to": time.Now(),
		"lc": time.Now(),
		"li": int64(1),
	})
	bad := base64.URLEncoding.EncodeToString(raw)
	_, err := DecodeCursor(bad)
	if err == nil {
		t.Fatal("expected error for wrong schema_ver")
	}
}

func TestDecodeCursor_AcceptsValidCursor(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	orig := &PayloadAuditCursor{
		SchemaVer:   payloadAuditCursorSchemaVer,
		ToEffective: now,
		LastCreated: now.Add(-time.Second),
		LastID:      42,
	}
	encoded, err := EncodeCursor(orig)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	decoded, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if decoded.SchemaVer != orig.SchemaVer {
		t.Errorf("SchemaVer = %d, want %d", decoded.SchemaVer, orig.SchemaVer)
	}
	if decoded.LastID != orig.LastID {
		t.Errorf("LastID = %d, want %d", decoded.LastID, orig.LastID)
	}
}
