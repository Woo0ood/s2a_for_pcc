package service

import (
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/audittoken"
)

// ErrInvalidPayloadAuditConfig is returned when payload audit config fails validation.
var ErrInvalidPayloadAuditConfig = errors.New("invalid payload audit config")

// PayloadAuditExportKey represents an API key for the payload audit export endpoint.
type PayloadAuditExportKey struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	HashedToken     string    `json:"hashed_token"`
	RateLimitPerMin int       `json:"rate_limit_per_min"`
	CreatedAt       time.Time `json:"created_at"`
	Disabled        bool      `json:"disabled"`
}

// PayloadAuditConfig mirrors the JSON stored in settings.payload_audit_config.
type PayloadAuditConfig struct {
	AllGroups     bool                    `json:"all_groups"`
	GroupIDs      []int64                 `json:"group_ids"`
	InputMaxBytes int                    `json:"input_max_bytes"`
	OutputMaxBytes int                   `json:"output_max_bytes"`
	ExcerptBytes  int                    `json:"excerpt_bytes"`
	RetentionDays int                    `json:"retention_days"`
	WorkerCount   int                    `json:"worker_count"`
	QueueSize     int                    `json:"queue_size"`
	QueueMaxBytes int                    `json:"queue_max_bytes"`
	BatchSize     int                    `json:"batch_size"`
	BatchFlushMs  int                    `json:"batch_flush_ms"`
	ExportAPIKeys []PayloadAuditExportKey `json:"export_api_keys"`

	// 旁路（offload）到对象存储的配置。
	OffloadEnabled             bool            `json:"offload_enabled"`
	BlobOffloadMinBytes        int             `json:"blob_offload_min_bytes"`        // 0 = 用默认；内联 base64 解码后 >= 该值才旁路
	BlobStorePrefix            string          `json:"blob_store_prefix"`             // 默认 "payload-audit/"
	OffloadRetentionMarginDays int             `json:"offload_retention_margin_days"` // 对象保留 = RetentionDays + margin
	BlobStore                  *BackupS3Config `json:"blob_store,omitempty"`          // 独立 S3 配置；secret 经 SecretEncryptor
}

// ConfigSnapshot is an immutable point-in-time snapshot of payload audit configuration.
// A new instance is created on every UpdateConfig call.
type ConfigSnapshot struct {
	Enabled        bool
	AllGroups      bool
	GroupIDs       map[int64]struct{}
	InputMaxBytes  int
	OutputMaxBytes int
	ExcerptBytes   int
	RetentionDays  int
	WorkerCount    int
	QueueSize      int
	QueueMaxBytes  int
	BatchSize      int
	BatchFlushMs   int
	ExportKeys       []PayloadAuditExportKey
	ExportKeysByHash map[string]*PayloadAuditExportKey
	Generation       uint64
	OffloadEnabled      bool
	BlobOffloadMinBytes int
	BlobStorePrefix     string
}

// GroupInScope reports whether the given group ID falls within the audit scope.
func (s *ConfigSnapshot) GroupInScope(gid *int64) bool {
	if s == nil || !s.Enabled {
		return false
	}
	if s.AllGroups {
		return true
	}
	if gid == nil {
		return false
	}
	_, ok := s.GroupIDs[*gid]
	return ok
}

// FindExportKey looks up an export key by its raw token (hashed internally).
func (s *ConfigSnapshot) FindExportKey(token string) *PayloadAuditExportKey {
	if s == nil {
		return nil
	}
	h := audittoken.HashAuditToken(token)
	return s.ExportKeysByHash[h]
}

// validatePayloadAuditConfig checks config invariants; returns error on violation.
// Some fields are silently defaulted instead of rejected.
func validatePayloadAuditConfig(cfg *PayloadAuditConfig) error {
	if cfg.ExcerptBytes != 0 && (cfg.ExcerptBytes < 64 || cfg.ExcerptBytes > 2048) {
		return fmt.Errorf("%w: excerpt_bytes must be in [64,2048] or 0 (disabled)", ErrInvalidPayloadAuditConfig)
	}
	if cfg.RetentionDays < 1 {
		cfg.RetentionDays = 180
	}
	if cfg.WorkerCount < 0 || cfg.WorkerCount > 32 {
		return fmt.Errorf("%w: worker_count must be in [0,32]", ErrInvalidPayloadAuditConfig)
	}
	if cfg.QueueSize < 0 {
		return fmt.Errorf("%w: queue_size negative", ErrInvalidPayloadAuditConfig)
	}
	if cfg.QueueMaxBytes < 0 {
		return fmt.Errorf("%w: queue_max_bytes negative", ErrInvalidPayloadAuditConfig)
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 100
	}
	if cfg.BatchFlushMs < 1 {
		cfg.BatchFlushMs = 200
	}
	if cfg.BlobOffloadMinBytes < 0 {
		return fmt.Errorf("%w: blob_offload_min_bytes negative", ErrInvalidPayloadAuditConfig)
	}
	if cfg.OffloadRetentionMarginDays < 0 {
		return fmt.Errorf("%w: offload_retention_margin_days negative", ErrInvalidPayloadAuditConfig)
	}
	if cfg.OffloadEnabled && (cfg.BlobStore == nil || !cfg.BlobStore.IsConfigured()) {
		return fmt.Errorf("%w: offload_enabled requires a configured blob_store", ErrInvalidPayloadAuditConfig)
	}
	return nil
}

// buildSnapshot constructs an immutable ConfigSnapshot from the mutable config.
func buildSnapshot(enabled bool, cfg *PayloadAuditConfig, gen uint64) *ConfigSnapshot {
	s := &ConfigSnapshot{
		Enabled:        enabled,
		AllGroups:      cfg.AllGroups,
		InputMaxBytes:  cfg.InputMaxBytes,
		OutputMaxBytes: cfg.OutputMaxBytes,
		ExcerptBytes:   cfg.ExcerptBytes,
		RetentionDays:  cfg.RetentionDays,
		WorkerCount:    cfg.WorkerCount,
		QueueSize:      cfg.QueueSize,
		QueueMaxBytes:  cfg.QueueMaxBytes,
		BatchSize:      cfg.BatchSize,
		BatchFlushMs:   cfg.BatchFlushMs,
		ExportKeys:          append([]PayloadAuditExportKey(nil), cfg.ExportAPIKeys...),
		Generation:          gen,
		OffloadEnabled:      enabled && cfg.OffloadEnabled,
		BlobOffloadMinBytes: cfg.BlobOffloadMinBytes,
		BlobStorePrefix:     cfg.BlobStorePrefix,
	}
	s.GroupIDs = make(map[int64]struct{}, len(cfg.GroupIDs))
	for _, id := range cfg.GroupIDs {
		s.GroupIDs[id] = struct{}{}
	}
	s.ExportKeysByHash = make(map[string]*PayloadAuditExportKey, len(s.ExportKeys))
	for i := range s.ExportKeys {
		if !s.ExportKeys[i].Disabled {
			s.ExportKeysByHash[s.ExportKeys[i].HashedToken] = &s.ExportKeys[i]
		}
	}
	return s
}
