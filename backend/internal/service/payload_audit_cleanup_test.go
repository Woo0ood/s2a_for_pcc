package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ── fake cleanup repo (ClickHouse) ──

type fakeCleanupRepo struct {
	dropped []string
	err     error
}

func (r *fakeCleanupRepo) DropExpiredMonthlyPartitions(_ context.Context, _ time.Time) ([]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.dropped, nil
}

// ── helper to build a PayloadAuditService with a known RetentionDays ──

func newTestCleanupService(t *testing.T, retentionDays int) *PayloadAuditService {
	t.Helper()
	repo := newMockSettingsRepo()
	svc, err := ProvidePayloadAuditService(repo, nil, 0, nil, nil, nil)
	if err != nil {
		t.Fatalf("ProvidePayloadAuditService: %v", err)
	}
	if _, err := svc.UpdateConfig(context.Background(), true, PayloadAuditConfig{
		RetentionDays: retentionDays,
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	return svc
}

// ── tests ──

func TestCleanup_DropsExpiredPartitions(t *testing.T) {
	svc := newTestCleanupService(t, 30)
	repo := &fakeCleanupRepo{
		dropped: []string{"202301", "202302", "202303"},
	}

	cleanup := NewPayloadAuditCleanup(repo, svc)
	deleted, err := cleanup.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("expected deleted=3, got %d", deleted)
	}
}

func TestCleanup_NoPartitionsToClean(t *testing.T) {
	svc := newTestCleanupService(t, 30)
	repo := &fakeCleanupRepo{
		dropped: nil,
	}

	cleanup := NewPayloadAuditCleanup(repo, svc)
	deleted, err := cleanup.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("expected deleted=0, got %d", deleted)
	}
}

func TestCleanup_RepoError(t *testing.T) {
	svc := newTestCleanupService(t, 30)
	repo := &fakeCleanupRepo{
		err: errors.New("connection refused"),
	}

	cleanup := NewPayloadAuditCleanup(repo, svc)
	_, err := cleanup.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanup_DefaultRetention(t *testing.T) {
	// When no retention is configured, default to 180 days.
	repo := newMockSettingsRepo()
	svc, err := ProvidePayloadAuditService(repo, nil, 0, nil, nil, nil)
	if err != nil {
		t.Fatalf("ProvidePayloadAuditService: %v", err)
	}

	fakeRepo := &fakeCleanupRepo{}
	cleanup := NewPayloadAuditCleanup(fakeRepo, svc)
	deleted, err := cleanup.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("expected deleted=0 (no partitions), got %d", deleted)
	}
}
