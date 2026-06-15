package service

import (
	"testing"
)

func TestValidatePayloadAuditConfig_OffloadDefaults(t *testing.T) {
	cfg := &PayloadAuditConfig{RetentionDays: 180, BatchSize: 1, BatchFlushMs: 1}
	if err := validatePayloadAuditConfig(cfg); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.BlobOffloadMinBytes != 0 {
		t.Fatalf("min bytes should stay 0 (disabled) when unset, got %d", cfg.BlobOffloadMinBytes)
	}
}

func TestValidatePayloadAuditConfig_OffloadNegativeRejected(t *testing.T) {
	cfg := &PayloadAuditConfig{RetentionDays: 180, BatchSize: 1, BatchFlushMs: 1, BlobOffloadMinBytes: -1}
	if err := validatePayloadAuditConfig(cfg); err == nil {
		t.Fatal("expected error for negative BlobOffloadMinBytes")
	}
}

func TestValidatePayloadAuditConfig_OffloadEnabledWithoutBlobStore(t *testing.T) {
	cfg := &PayloadAuditConfig{RetentionDays: 180, BatchSize: 1, BatchFlushMs: 1, OffloadEnabled: true}
	if err := validatePayloadAuditConfig(cfg); err == nil {
		t.Fatal("expected error when offload_enabled without blob_store")
	}
}
