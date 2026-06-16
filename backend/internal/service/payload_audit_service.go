package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/audittoken"
	"github.com/Wei-Shaw/sub2api/pkg/snowflake"
	"github.com/redis/go-redis/v9" //nolint:depguard // payload audit owns redis interaction
)

const (
	settingKeyPayloadAuditEnabled = "payload_audit_enabled"
	settingKeyPayloadAuditConfig  = "payload_audit_config"
	redisKeyExportKeyLastUsed     = "payload_audit:export_key:last_used:%s" // %s = key id
	exportKeyLastUsedTTL          = 7 * 24 * time.Hour

	redisKeyConvExportJob    = "pa:convexport:%s"
	redisKeyConvExportResult = "pa:convexport:%s:html"
	convExportTTL            = 30 * time.Minute
)

// ConvExportJob is the status record for an async conversation export job.
type ConvExportJob struct {
	Status    string `json:"status"`           // "running" | "done" | "failed"
	Error     string `json:"error,omitempty"`
	SizeBytes int    `json:"size_bytes,omitempty"`
}

// PayloadAuditTTLSyncer is called when RetentionDays changes so the
// underlying storage can adjust its TTL (e.g. ALTER TABLE … MODIFY TTL).
type PayloadAuditTTLSyncer interface {
	AlterTTL(ctx context.Context, retentionDays int) error
}

// PayloadAuditWorkerID is a named int64 used by Wire to disambiguate the
// snowflake worker-id parameter from other int64 values.
type PayloadAuditWorkerID int64

// payloadAuditSettingsRepo is the subset of SettingRepository needed by PayloadAuditService.
type payloadAuditSettingsRepo interface {
	GetValue(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
}

// PayloadAuditService manages payload audit configuration lifecycle,
// including hot-reload of ConfigSnapshot and export key CRUD.
type PayloadAuditService struct {
	settings     payloadAuditSettingsRepo
	rdb          *redis.Client
	idgen        *snowflake.Generator
	ttlSyncer    PayloadAuditTTLSyncer
	storeFactory BackupObjectStoreFactory
	encryptor    SecretEncryptor
	snap         atomic.Pointer[ConfigSnapshot]
	gen          atomic.Uint64
	cfgMu        sync.Mutex // serialises read-modify-write of payload_audit_config
	uploader     atomic.Pointer[PayloadAuditUploader]
	resolver     atomic.Pointer[BlobResolver]
}

// ProvidePayloadAuditService loads settings and builds the initial snapshot.
// On load failure, an empty disabled snapshot is installed so the service can start.
func ProvidePayloadAuditService(settings SettingRepository, rdb *redis.Client, workerID PayloadAuditWorkerID, ttlSyncer PayloadAuditTTLSyncer, storeFactory BackupObjectStoreFactory, encryptor SecretEncryptor) (*PayloadAuditService, error) {
	gen, err := snowflake.New(int64(workerID))
	if err != nil {
		return nil, fmt.Errorf("payload audit snowflake init: %w", err)
	}
	s := &PayloadAuditService{settings: settings, rdb: rdb, idgen: gen, ttlSyncer: ttlSyncer, storeFactory: storeFactory, encryptor: encryptor}
	if err := s.LoadFromSettings(context.Background()); err != nil {
		s.snap.Store(buildSnapshot(false, &PayloadAuditConfig{}, 0))
		return s, nil
	}
	return s, nil
}

// Uploader returns the current offload uploader, or nil when offload is disabled/unconfigured.
func (s *PayloadAuditService) Uploader() *PayloadAuditUploader { return s.uploader.Load() }

// Resolver returns the current BlobResolver, or nil when offload is disabled/unconfigured.
func (s *PayloadAuditService) Resolver() *BlobResolver { return s.resolver.Load() }

// InstallSnapshotForTest swaps in a caller-built ConfigSnapshot. Test-only:
// lets handler tests exercise config-dependent branches (e.g. export-worker URL)
// without standing up the full settings/Redis load path.
func (s *PayloadAuditService) InstallSnapshotForTest(snap *ConfigSnapshot) { s.snap.Store(snap) }

// InstallResolverForTest swaps in a caller-built BlobResolver. Test-only:
// lets handler tests stream a fake S3 result via Resolver().StreamObject.
func (s *PayloadAuditService) InstallResolverForTest(r *BlobResolver) { s.resolver.Store(r) }

// rebuildUploader (re)builds the offload uploader and blob resolver from the given config,
// or clears them. Both share the same store instance built once here.
// Called after each snapshot swap so they track hot-reloaded config.
func (s *PayloadAuditService) rebuildUploader(cfg *PayloadAuditConfig, enabled bool) {
	if !enabled || !cfg.OffloadEnabled || cfg.BlobStore == nil || !cfg.BlobStore.IsConfigured() || s.storeFactory == nil {
		s.uploader.Store(nil)
		s.resolver.Store(nil)
		return
	}
	dec := *cfg.BlobStore // copy; decrypt secret without mutating the snapshot's config
	if dec.SecretAccessKey != "" && s.encryptor != nil {
		if plain, err := s.encryptor.Decrypt(dec.SecretAccessKey); err != nil {
			slog.Warn("payload_audit.blobstore_secret_decrypt_failed", "err", err) // keep original (compat with unencrypted)
		} else {
			dec.SecretAccessKey = plain
		}
	}
	store, err := NewPayloadAuditBlobStore(context.Background(), s.storeFactory, &dec)
	if err != nil {
		slog.Error("payload_audit.blobstore_build_failed", "err", err)
		s.uploader.Store(nil)
		s.resolver.Store(nil)
		return
	}
	// Build uploader and resolver from the same store; prefix shared.
	s.uploader.Store(NewPayloadAuditUploader(store, cfg.BlobStorePrefix, cfg.WorkerCount))
	s.resolver.Store(NewBlobResolver(store, cfg.BlobStorePrefix))
}

// NextID returns the next snowflake ID for a payload audit event.
func (s *PayloadAuditService) NextID() (int64, error) {
	return s.idgen.NextID()
}

// Snapshot returns the current immutable configuration snapshot.
func (s *PayloadAuditService) Snapshot() *ConfigSnapshot { return s.snap.Load() }

// LoadFromSettings reads the two setting keys and rebuilds the snapshot.
func (s *PayloadAuditService) LoadFromSettings(ctx context.Context) error {
	enabledStr, _ := s.settings.GetValue(ctx, settingKeyPayloadAuditEnabled)
	enabled := enabledStr == "true"

	cfgStr, _ := s.settings.GetValue(ctx, settingKeyPayloadAuditConfig)
	var cfg PayloadAuditConfig
	if cfgStr != "" {
		if err := json.Unmarshal([]byte(cfgStr), &cfg); err != nil {
			return err
		}
	}
	if err := validatePayloadAuditConfig(&cfg); err != nil {
		return err
	}
	s.snap.Store(buildSnapshot(enabled, &cfg, s.gen.Add(1)))
	s.rebuildUploader(&cfg, enabled)
	return nil
}

// UpdateConfig validates, persists, and atomically swaps the snapshot.
// Returns needRebuildSink=true when queue_size or queue_max_bytes changed.
func (s *PayloadAuditService) UpdateConfig(ctx context.Context, enabled bool, cfg PayloadAuditConfig) (needRebuildSink bool, err error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	// Mirror backup: keep or encrypt the blob_store secret before validation.
	if cfg.BlobStore != nil {
		if cfg.BlobStore.SecretAccessKey == "" {
			// keep existing encrypted secret
			if old := s.snap.Load(); old != nil && old.BlobStore != nil {
				cfg.BlobStore.SecretAccessKey = old.BlobStore.SecretAccessKey
			}
		} else if s.encryptor != nil {
			enc, err := s.encryptor.Encrypt(cfg.BlobStore.SecretAccessKey)
			if err != nil {
				return false, fmt.Errorf("encrypt blob_store secret: %w", err)
			}
			cfg.BlobStore.SecretAccessKey = enc
		}
	}

	if err := validatePayloadAuditConfig(&cfg); err != nil {
		return false, err
	}

	old := s.Snapshot()
	needRebuildSink = old != nil && (old.QueueSize != cfg.QueueSize || old.QueueMaxBytes != cfg.QueueMaxBytes)

	cfgBytes, _ := json.Marshal(cfg)
	if err := s.settings.Set(ctx, settingKeyPayloadAuditConfig, string(cfgBytes)); err != nil {
		return false, err
	}
	enabledStr := "false"
	if enabled {
		enabledStr = "true"
	}
	if err := s.settings.Set(ctx, settingKeyPayloadAuditEnabled, enabledStr); err != nil {
		return false, err
	}
	s.snap.Store(buildSnapshot(enabled, &cfg, s.gen.Add(1)))
	s.rebuildUploader(&cfg, enabled)

	// Sync TTL to ClickHouse when retention_days changes (non-blocking).
	if s.ttlSyncer != nil && old != nil && old.RetentionDays != cfg.RetentionDays {
		if err := s.ttlSyncer.AlterTTL(ctx, cfg.RetentionDays); err != nil {
			slog.Error("payload_audit.alter_ttl_fail", "err", err)
		}
	}

	return needRebuildSink, nil
}

// === Export Keys CRUD ===

// ErrExportKeyNotFound is returned when a delete targets a nonexistent key id.
var ErrExportKeyNotFound = errors.New("payload audit export key not found")

// CreateExportKey generates a new audit token and persists its hash.
// Returns the clear-text token (shown once) and the key metadata.
func (s *PayloadAuditService) CreateExportKey(ctx context.Context, name string, ratePerMin int) (clearToken string, key PayloadAuditExportKey, err error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	cfgStr, err := s.settings.GetValue(ctx, settingKeyPayloadAuditConfig)
	if err != nil && !isSettingNotFound(err) {
		return "", PayloadAuditExportKey{}, err
	}
	var cfg PayloadAuditConfig
	if cfgStr != "" {
		_ = json.Unmarshal([]byte(cfgStr), &cfg)
	}

	tok, hashed := audittoken.GenerateAuditToken()
	keyID := generateExportKeyID()
	rate := ratePerMin
	if rate <= 0 {
		rate = 60
	}
	newKey := PayloadAuditExportKey{
		ID:              keyID,
		Name:            name,
		HashedToken:     hashed,
		RateLimitPerMin: rate,
		CreatedAt:       time.Now(),
		Disabled:        false,
	}
	cfg.ExportAPIKeys = append(cfg.ExportAPIKeys, newKey)
	raw, _ := json.Marshal(cfg)
	if err := s.settings.Set(ctx, settingKeyPayloadAuditConfig, string(raw)); err != nil {
		return "", PayloadAuditExportKey{}, err
	}

	enabledStr, _ := s.settings.GetValue(ctx, settingKeyPayloadAuditEnabled)
	s.snap.Store(buildSnapshot(enabledStr == "true", &cfg, s.gen.Add(1)))
	return tok, newKey, nil
}

// DeleteExportKey removes an export key by id.
func (s *PayloadAuditService) DeleteExportKey(ctx context.Context, id string) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	cfgStr, err := s.settings.GetValue(ctx, settingKeyPayloadAuditConfig)
	if err != nil && !isSettingNotFound(err) {
		return err
	}
	var cfg PayloadAuditConfig
	if cfgStr != "" {
		_ = json.Unmarshal([]byte(cfgStr), &cfg)
	}

	found := false
	out := cfg.ExportAPIKeys[:0]
	for _, k := range cfg.ExportAPIKeys {
		if k.ID == id {
			found = true
			continue
		}
		out = append(out, k)
	}
	if !found {
		return ErrExportKeyNotFound
	}
	cfg.ExportAPIKeys = out

	raw, _ := json.Marshal(cfg)
	if err := s.settings.Set(ctx, settingKeyPayloadAuditConfig, string(raw)); err != nil {
		return err
	}

	enabledStr, _ := s.settings.GetValue(ctx, settingKeyPayloadAuditEnabled)
	s.snap.Store(buildSnapshot(enabledStr == "true", &cfg, s.gen.Add(1)))
	return nil
}

// ListExportKeys returns a copy of the current export keys from the snapshot.
func (s *PayloadAuditService) ListExportKeys(_ context.Context) ([]PayloadAuditExportKey, error) {
	snap := s.Snapshot()
	if snap == nil {
		return nil, nil
	}
	out := make([]PayloadAuditExportKey, len(snap.ExportKeys))
	copy(out, snap.ExportKeys)
	return out, nil
}

// === Last-Used tracking via Redis ===

// MarkExportKeyUsed records a last-used timestamp in Redis (fire-and-forget).
func (s *PayloadAuditService) MarkExportKeyUsed(_ context.Context, id string) {
	if s.rdb == nil {
		return
	}
	go func() {
		_ = s.rdb.Set(context.Background(),
			fmt.Sprintf(redisKeyExportKeyLastUsed, id),
			time.Now().UTC().Format(time.RFC3339),
			exportKeyLastUsedTTL).Err()
	}()
}

// ExportKeyLastUsed retrieves the last-used timestamp from Redis.
func (s *PayloadAuditService) ExportKeyLastUsed(ctx context.Context, id string) (time.Time, bool) {
	if s.rdb == nil {
		return time.Time{}, false
	}
	val, err := s.rdb.Get(ctx, fmt.Sprintf(redisKeyExportKeyLastUsed, id)).Result()
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, val)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// === Async Conversation Export Jobs (Redis-backed) ===

// CreateConvExportJob allocates a new job record in Redis with status "running".
func (s *PayloadAuditService) CreateConvExportJob(ctx context.Context) (string, error) {
	if s.rdb == nil {
		return "", errors.New("redis not configured")
	}
	id, err := s.idgen.NextID()
	if err != nil {
		return "", err
	}
	jobID := strconv.FormatInt(id, 10)
	b, _ := json.Marshal(ConvExportJob{Status: "running"})
	if err := s.rdb.Set(ctx, fmt.Sprintf(redisKeyConvExportJob, jobID), b, convExportTTL).Err(); err != nil {
		return "", err
	}
	return jobID, nil
}

// FinishConvExportJob stores the rendered HTML result and marks the job "done".
func (s *PayloadAuditService) FinishConvExportJob(ctx context.Context, jobID string, html []byte) {
	if s.rdb == nil {
		return
	}
	s.rdb.Set(ctx, fmt.Sprintf(redisKeyConvExportResult, jobID), html, convExportTTL)
	b, _ := json.Marshal(ConvExportJob{Status: "done", SizeBytes: len(html)})
	s.rdb.Set(ctx, fmt.Sprintf(redisKeyConvExportJob, jobID), b, convExportTTL)
}

// FailConvExportJob marks a job as "failed" with an error message.
func (s *PayloadAuditService) FailConvExportJob(ctx context.Context, jobID, errMsg string) {
	if s.rdb == nil {
		return
	}
	b, _ := json.Marshal(ConvExportJob{Status: "failed", Error: errMsg})
	s.rdb.Set(ctx, fmt.Sprintf(redisKeyConvExportJob, jobID), b, convExportTTL)
}

// GetConvExportJob retrieves the job status record. Returns nil (no error) when not found.
func (s *PayloadAuditService) GetConvExportJob(ctx context.Context, jobID string) (*ConvExportJob, error) {
	if s.rdb == nil {
		return nil, errors.New("redis not configured")
	}
	val, err := s.rdb.Get(ctx, fmt.Sprintf(redisKeyConvExportJob, jobID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j ConvExportJob
	if err := json.Unmarshal([]byte(val), &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// GetConvExportResult retrieves the rendered HTML bytes. Returns nil (no error) when not found (expired).
func (s *PayloadAuditService) GetConvExportResult(ctx context.Context, jobID string) ([]byte, error) {
	if s.rdb == nil {
		return nil, errors.New("redis not configured")
	}
	val, err := s.rdb.Get(ctx, fmt.Sprintf(redisKeyConvExportResult, jobID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return val, err
}

// generateExportKeyID produces a 24-character random hex string prefixed with "ak_".
func generateExportKeyID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "ak_" + hex.EncodeToString(b[:])
}

// isSettingNotFound checks whether an error is ErrSettingNotFound.
func isSettingNotFound(err error) bool {
	return errors.Is(err, ErrSettingNotFound)
}
