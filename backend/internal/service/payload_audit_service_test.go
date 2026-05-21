package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- in-memory mock of payloadAuditSettingsRepo ---

type mockSettingsRepo struct {
	mu   sync.Mutex
	data map[string]string
}

func newMockSettingsRepo() *mockSettingsRepo {
	return &mockSettingsRepo{data: make(map[string]string)}
}

func (m *mockSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}

func (m *mockSettingsRepo) Set(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *mockSettingsRepo) Get(_ context.Context, key string) (*Setting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return nil, ErrSettingNotFound
	}
	return &Setting{Key: key, Value: v, UpdatedAt: time.Now()}, nil
}

func (m *mockSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := m.data[k]; ok {
			result[k] = v
		}
	}
	return result, nil
}

func (m *mockSettingsRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range settings {
		m.data[k] = v
	}
	return nil
}

func (m *mockSettingsRepo) GetAll(_ context.Context) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]string, len(m.data))
	for k, v := range m.data {
		result[k] = v
	}
	return result, nil
}

func (m *mockSettingsRepo) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

// --- helper ---

func newTestPayloadAuditService(t *testing.T) *PayloadAuditService {
	t.Helper()
	repo := newMockSettingsRepo()
	svc, err := ProvidePayloadAuditService(repo, nil, 0)
	if err != nil {
		t.Fatalf("ProvidePayloadAuditService: %v", err)
	}
	return svc
}

// --- tests ---

func TestConfigSnapshot_HotReload(t *testing.T) {
	ctx := context.Background()
	svc := newTestPayloadAuditService(t)

	s1 := svc.Snapshot()
	if s1 == nil {
		t.Fatal("initial snapshot is nil")
	}

	needRebuild, err := svc.UpdateConfig(ctx, true, PayloadAuditConfig{
		ExcerptBytes:  256,
		RetentionDays: 30,
		BatchSize:     50,
		BatchFlushMs:  100,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = needRebuild

	s2 := svc.Snapshot()
	if s2.ExcerptBytes != 256 {
		t.Fatalf("expected ExcerptBytes=256, got %d", s2.ExcerptBytes)
	}
	if !s2.Enabled {
		t.Fatal("expected Enabled=true")
	}
	// old snapshot must not be mutated
	if s1.ExcerptBytes == 256 {
		t.Fatal("old snapshot was mutated")
	}
	if s2.Generation <= s1.Generation {
		t.Fatal("generation should increase")
	}
}

func TestConfigSnapshot_QueueResizeFlagsRebuild(t *testing.T) {
	ctx := context.Background()
	svc := newTestPayloadAuditService(t)

	// Set initial config with a known QueueSize.
	_, err := svc.UpdateConfig(ctx, true, PayloadAuditConfig{
		QueueSize:     100,
		RetentionDays: 30,
		BatchSize:     50,
		BatchFlushMs:  100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Change QueueSize → should signal rebuild.
	needRebuild, err := svc.UpdateConfig(ctx, true, PayloadAuditConfig{
		QueueSize:     200,
		RetentionDays: 30,
		BatchSize:     50,
		BatchFlushMs:  100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !needRebuild {
		t.Fatal("should signal rebuild when QueueSize changes")
	}

	// Same config again → should NOT signal rebuild.
	needRebuild, err = svc.UpdateConfig(ctx, true, PayloadAuditConfig{
		QueueSize:     200,
		RetentionDays: 30,
		BatchSize:     50,
		BatchFlushMs:  100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if needRebuild {
		t.Fatal("should not signal rebuild when QueueSize unchanged")
	}
}

func TestConfigSnapshot_Validation(t *testing.T) {
	ctx := context.Background()
	svc := newTestPayloadAuditService(t)

	// excerpt_bytes < 64 (and non-zero) → reject
	_, err := svc.UpdateConfig(ctx, true, PayloadAuditConfig{ExcerptBytes: 10})
	if err == nil {
		t.Fatal("should reject excerpt_bytes < 64")
	}

	// excerpt_bytes > 2048 → reject
	_, err = svc.UpdateConfig(ctx, true, PayloadAuditConfig{ExcerptBytes: 4096})
	if err == nil {
		t.Fatal("should reject excerpt_bytes > 2048")
	}

	// worker_count > 32 → reject
	_, err = svc.UpdateConfig(ctx, true, PayloadAuditConfig{WorkerCount: 100})
	if err == nil {
		t.Fatal("should reject worker_count > 32")
	}

	// worker_count < 0 → reject
	_, err = svc.UpdateConfig(ctx, true, PayloadAuditConfig{WorkerCount: -1})
	if err == nil {
		t.Fatal("should reject negative worker_count")
	}

	// queue_size < 0 → reject
	_, err = svc.UpdateConfig(ctx, true, PayloadAuditConfig{QueueSize: -1})
	if err == nil {
		t.Fatal("should reject negative queue_size")
	}

	// excerpt_bytes=0 is allowed (means disabled)
	_, err = svc.UpdateConfig(ctx, true, PayloadAuditConfig{ExcerptBytes: 0})
	if err != nil {
		t.Fatalf("excerpt_bytes=0 should be valid: %v", err)
	}

	// excerpt_bytes in valid range
	_, err = svc.UpdateConfig(ctx, true, PayloadAuditConfig{ExcerptBytes: 512})
	if err != nil {
		t.Fatalf("excerpt_bytes=512 should be valid: %v", err)
	}
}

func TestConfigSnapshot_GroupInScope(t *testing.T) {
	ctx := context.Background()
	svc := newTestPayloadAuditService(t)

	gid := int64(42)

	// Disabled → never in scope.
	_, _ = svc.UpdateConfig(ctx, false, PayloadAuditConfig{
		AllGroups: true,
	})
	snap := svc.Snapshot()
	if snap.GroupInScope(&gid) {
		t.Fatal("disabled snapshot should not be in scope")
	}

	// AllGroups=true, enabled → all in scope.
	_, _ = svc.UpdateConfig(ctx, true, PayloadAuditConfig{
		AllGroups: true,
	})
	snap = svc.Snapshot()
	if !snap.GroupInScope(&gid) {
		t.Fatal("all_groups=true should match any gid")
	}
	if !snap.GroupInScope(nil) {
		t.Fatal("all_groups=true should match nil gid")
	}

	// Specific group IDs.
	_, _ = svc.UpdateConfig(ctx, true, PayloadAuditConfig{
		GroupIDs: []int64{42, 99},
	})
	snap = svc.Snapshot()
	if !snap.GroupInScope(&gid) {
		t.Fatal("gid 42 should be in scope")
	}
	other := int64(1)
	if snap.GroupInScope(&other) {
		t.Fatal("gid 1 should not be in scope")
	}
	if snap.GroupInScope(nil) {
		t.Fatal("nil gid should not be in scope with specific group ids")
	}
}

func TestExportKey_CreateListDelete(t *testing.T) {
	ctx := context.Background()
	svc := newTestPayloadAuditService(t)

	tok, k1, err := svc.CreateExportKey(ctx, "test-key", 60)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "sk-pa-") {
		t.Fatalf("token should have sk-pa- prefix, got %q", tok)
	}
	if k1.HashedToken == "" {
		t.Fatal("hashed_token should not be empty")
	}
	if !strings.HasPrefix(k1.ID, "ak_") {
		t.Fatalf("key ID should have ak_ prefix, got %q", k1.ID)
	}

	list, err := svc.ListExportKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != k1.ID {
		t.Fatalf("expected 1 key with id=%q, got %d keys", k1.ID, len(list))
	}

	// Verify token works through snapshot lookup.
	snap := svc.Snapshot()
	if snap.FindExportKey(tok) == nil {
		t.Fatal("FindExportKey failed for valid token")
	}

	// Delete the key.
	if err := svc.DeleteExportKey(ctx, k1.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = svc.ListExportKeys(ctx)
	if len(list) != 0 {
		t.Fatalf("expected 0 keys after delete, got %d", len(list))
	}

	// Verify snapshot no longer finds the token.
	snap = svc.Snapshot()
	if snap.FindExportKey(tok) != nil {
		t.Fatal("FindExportKey should return nil after delete")
	}

	// Delete nonexistent → ErrExportKeyNotFound.
	if err := svc.DeleteExportKey(ctx, "nonexistent"); !errors.Is(err, ErrExportKeyNotFound) {
		t.Fatalf("expected ErrExportKeyNotFound, got %v", err)
	}
}

func TestExportKey_DefaultRate(t *testing.T) {
	ctx := context.Background()
	svc := newTestPayloadAuditService(t)

	_, k, err := svc.CreateExportKey(ctx, "no-rate", 0)
	if err != nil {
		t.Fatal(err)
	}
	if k.RateLimitPerMin != 60 {
		t.Fatalf("expected default rate=60, got %d", k.RateLimitPerMin)
	}
}

func TestExportKey_DisabledKeyNotInLookup(t *testing.T) {
	ctx := context.Background()
	svc := newTestPayloadAuditService(t)

	tok, k1, err := svc.CreateExportKey(ctx, "will-disable", 60)
	if err != nil {
		t.Fatal(err)
	}

	// Manually update the config to disable the key.
	snap := svc.Snapshot()
	_ = snap
	// Read current config, set disabled, re-save.
	cfg := PayloadAuditConfig{
		ExportAPIKeys: []PayloadAuditExportKey{
			{
				ID:              k1.ID,
				Name:            k1.Name,
				HashedToken:     k1.HashedToken,
				RateLimitPerMin: k1.RateLimitPerMin,
				CreatedAt:       k1.CreatedAt,
				Disabled:        true,
			},
		},
	}
	_, err = svc.UpdateConfig(ctx, true, cfg)
	if err != nil {
		t.Fatal(err)
	}

	snap = svc.Snapshot()
	if snap.FindExportKey(tok) != nil {
		t.Fatal("disabled key should not be in ExportKeysByHash lookup")
	}
	// But it should still be in the ExportKeys slice.
	if len(snap.ExportKeys) != 1 {
		t.Fatal("disabled key should still be in ExportKeys list")
	}
}

func TestLoadFromSettings_InvalidJSON(t *testing.T) {
	repo := newMockSettingsRepo()
	_ = repo.Set(context.Background(), settingKeyPayloadAuditConfig, "not-json")

	svc, err := ProvidePayloadAuditService(repo, nil, 0)
	if err != nil {
		t.Fatal("ProvidePayloadAuditService should not return error on bad load")
	}
	// Should fall back to disabled snapshot.
	snap := svc.Snapshot()
	if snap == nil || snap.Enabled {
		t.Fatal("should have disabled fallback snapshot")
	}
}

func TestConfigSnapshot_RetentionDaysDefault(t *testing.T) {
	ctx := context.Background()
	svc := newTestPayloadAuditService(t)

	// retention_days=0 → silently defaults to 180.
	_, err := svc.UpdateConfig(ctx, true, PayloadAuditConfig{RetentionDays: 0})
	if err != nil {
		t.Fatal(err)
	}
	snap := svc.Snapshot()
	if snap.RetentionDays != 180 {
		t.Fatalf("expected default retention_days=180, got %d", snap.RetentionDays)
	}
}

func TestConfigSnapshot_BatchDefaults(t *testing.T) {
	ctx := context.Background()
	svc := newTestPayloadAuditService(t)

	// batch_size=0 and batch_flush_ms=0 → silently defaults.
	_, err := svc.UpdateConfig(ctx, true, PayloadAuditConfig{
		BatchSize:    0,
		BatchFlushMs: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := svc.Snapshot()
	if snap.BatchSize != 100 {
		t.Fatalf("expected default batch_size=100, got %d", snap.BatchSize)
	}
	if snap.BatchFlushMs != 200 {
		t.Fatalf("expected default batch_flush_ms=200, got %d", snap.BatchFlushMs)
	}
}
