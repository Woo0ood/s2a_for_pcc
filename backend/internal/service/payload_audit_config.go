package service

import (
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/audittoken"
)

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
	Generation     uint64
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
		return fmt.Errorf("excerpt_bytes must be in [64,2048] or 0 (disabled)")
	}
	if cfg.RetentionDays < 1 {
		cfg.RetentionDays = 180
	}
	if cfg.WorkerCount < 0 || cfg.WorkerCount > 32 {
		return fmt.Errorf("worker_count must be in [0,32]")
	}
	if cfg.QueueSize < 0 {
		return fmt.Errorf("queue_size negative")
	}
	if cfg.QueueMaxBytes < 0 {
		return fmt.Errorf("queue_max_bytes negative")
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 100
	}
	if cfg.BatchFlushMs < 1 {
		cfg.BatchFlushMs = 200
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
		ExportKeys:     append([]PayloadAuditExportKey(nil), cfg.ExportAPIKeys...),
		Generation:     gen,
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
