package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ── fake cleanup repo ──

type fakeCleanupRepo struct {
	mu         sync.Mutex
	partitions []PayloadAuditPartition
	states     map[string]string // partition name → current state
	calls      []string          // log of method calls for assertions

	detachErr   map[string]error
	finalizeErr map[string]error
	dropErr     map[string]error
	stateErr    map[string]error
}

func newFakeCleanupRepo() *fakeCleanupRepo {
	return &fakeCleanupRepo{
		states:      make(map[string]string),
		detachErr:   make(map[string]error),
		finalizeErr: make(map[string]error),
		dropErr:     make(map[string]error),
		stateErr:    make(map[string]error),
	}
}

func (r *fakeCleanupRepo) ListPartitionsBefore(_ context.Context, _ time.Time) ([]PayloadAuditPartition, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "ListPartitionsBefore")
	out := make([]PayloadAuditPartition, len(r.partitions))
	copy(out, r.partitions)
	return out, nil
}

func (r *fakeCleanupRepo) PartitionState(_ context.Context, name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "PartitionState:"+name)
	if err, ok := r.stateErr[name]; ok {
		return "", err
	}
	s, ok := r.states[name]
	if !ok {
		return PartitionStateUnknown, nil
	}
	return s, nil
}

func (r *fakeCleanupRepo) DetachPartitionConcurrently(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "Detach:"+name)
	if err, ok := r.detachErr[name]; ok {
		return err
	}
	return nil
}

func (r *fakeCleanupRepo) FinalizePartitionDetach(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "Finalize:"+name)
	if err, ok := r.finalizeErr[name]; ok {
		return err
	}
	return nil
}

func (r *fakeCleanupRepo) DropPartition(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "Drop:"+name)
	if err, ok := r.dropErr[name]; ok {
		return err
	}
	return nil
}

// helper: set all partitions to a given state
func (r *fakeCleanupRepo) setAllStates(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.partitions {
		r.states[p.Name] = state
	}
}

// ── fake partition maintainer repo ──

type fakePartRepo struct {
	mu       sync.Mutex
	created  []time.Time
	failAt   map[string]error // month key "2006-01" → error
}

func (r *fakePartRepo) CreatePartition(_ context.Context, monthStart time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := monthStart.Format("2006-01")
	if err, ok := r.failAt[key]; ok {
		return err
	}
	r.created = append(r.created, monthStart)
	return nil
}

// ── helper to build a PayloadAuditService with a known RetentionDays ──

func newTestCleanupService(t *testing.T, retentionDays int) *PayloadAuditService {
	t.Helper()
	repo := newMockSettingsRepo()
	svc, err := ProvidePayloadAuditService(repo, nil, 0)
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
	repo := newFakeCleanupRepo()

	// 5 partitions all expired, all ATTACHED
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("payload_audit_logs_2023_%02d", i+1)
		repo.partitions = append(repo.partitions, PayloadAuditPartition{
			Name: name,
			End:  time.Date(2023, time.Month(i+2), 1, 0, 0, 0, 0, time.UTC),
		})
		repo.states[name] = PartitionStateAttached
	}

	cleanup := NewPayloadAuditCleanup(repo, svc)

	// Round 1: all ATTACHED → detach concurrently, deleted=0
	deleted, err := cleanup.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("round 1: expected deleted=0, got %d", deleted)
	}

	// Simulate PG completed detach: switch all to DETACHED
	repo.setAllStates(PartitionStateDetached)

	// Round 2: all DETACHED → drop, deleted=5
	deleted, err = cleanup.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if deleted != 5 {
		t.Fatalf("round 2: expected deleted=5, got %d", deleted)
	}
}

func TestCleanup_HandlesPendingDetach(t *testing.T) {
	svc := newTestCleanupService(t, 30)
	repo := newFakeCleanupRepo()

	name := "payload_audit_logs_2023_01"
	repo.partitions = append(repo.partitions, PayloadAuditPartition{
		Name: name,
		End:  time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC),
	})
	repo.states[name] = PartitionStateDetachPending

	cleanup := NewPayloadAuditCleanup(repo, svc)
	deleted, err := cleanup.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected deleted=1, got %d", deleted)
	}

	// Verify call sequence: Finalize then Drop
	found := false
	for i, c := range repo.calls {
		if c == "Finalize:"+name {
			if i+1 < len(repo.calls) && repo.calls[i+1] == "Drop:"+name {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected Finalize then Drop, got calls: %v", repo.calls)
	}
}

func TestCleanup_HandlesDetachedState(t *testing.T) {
	svc := newTestCleanupService(t, 30)
	repo := newFakeCleanupRepo()

	name := "payload_audit_logs_2023_03"
	repo.partitions = append(repo.partitions, PayloadAuditPartition{
		Name: name,
		End:  time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	repo.states[name] = PartitionStateDetached

	cleanup := NewPayloadAuditCleanup(repo, svc)
	deleted, err := cleanup.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected deleted=1, got %d", deleted)
	}
}

func TestCleanup_DropFailureContinues(t *testing.T) {
	svc := newTestCleanupService(t, 30)
	repo := newFakeCleanupRepo()

	p1 := "payload_audit_logs_2023_01"
	p2 := "payload_audit_logs_2023_02"
	repo.partitions = append(repo.partitions,
		PayloadAuditPartition{Name: p1, End: time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC)},
		PayloadAuditPartition{Name: p2, End: time.Date(2023, 3, 1, 0, 0, 0, 0, time.UTC)},
	)
	repo.states[p1] = PartitionStateDetached
	repo.states[p2] = PartitionStateDetached
	repo.dropErr[p1] = errors.New("disk full")

	cleanup := NewPayloadAuditCleanup(repo, svc)
	deleted, err := cleanup.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected deleted=1 (second partition), got %d", deleted)
	}
}

func TestCleanup_RespectsContextCancel(t *testing.T) {
	svc := newTestCleanupService(t, 30)

	// 100 partitions, all ATTACHED
	partitions := make([]PayloadAuditPartition, 100)
	states := make(map[string]string, 100)
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("payload_audit_logs_2022_%02d", i)
		partitions[i] = PayloadAuditPartition{
			Name: name,
			End:  time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		states[name] = PartitionStateAttached
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use a cancelling wrapper that cancels ctx after the 3rd PartitionState call
	repo := &cancellingCleanupRepo{
		partitions:       partitions,
		states:           states,
		cancelAfterCalls: 3,
		cancel:           cancel,
	}

	cleanup := NewPayloadAuditCleanup(repo, svc)
	deleted, err := cleanup.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if deleted != 0 {
		t.Fatalf("expected deleted=0 (all ATTACHED, no drops), got %d", deleted)
	}
	// Should have processed far fewer than 100
	if repo.stateCallCount >= 100 {
		t.Fatalf("expected early exit, but processed all 100 partitions")
	}
}

// cancellingCleanupRepo cancels the context after N PartitionState calls.
type cancellingCleanupRepo struct {
	partitions       []PayloadAuditPartition
	states           map[string]string
	cancelAfterCalls int
	cancel           context.CancelFunc
	stateCallCount   int
}

func (r *cancellingCleanupRepo) ListPartitionsBefore(_ context.Context, _ time.Time) ([]PayloadAuditPartition, error) {
	out := make([]PayloadAuditPartition, len(r.partitions))
	copy(out, r.partitions)
	return out, nil
}

func (r *cancellingCleanupRepo) PartitionState(_ context.Context, name string) (string, error) {
	r.stateCallCount++
	if r.stateCallCount >= r.cancelAfterCalls {
		r.cancel()
	}
	s, ok := r.states[name]
	if !ok {
		return PartitionStateUnknown, nil
	}
	return s, nil
}

func (r *cancellingCleanupRepo) DetachPartitionConcurrently(_ context.Context, _ string) error {
	return nil
}

func (r *cancellingCleanupRepo) FinalizePartitionDetach(_ context.Context, _ string) error {
	return nil
}

func (r *cancellingCleanupRepo) DropPartition(_ context.Context, _ string) error {
	return nil
}

func TestPartitionMaintainer_CreatesFutureMonths(t *testing.T) {
	repo := &fakePartRepo{failAt: make(map[string]error)}
	m := NewPayloadAuditPartitionMaintainer(repo)
	if err := m.RunOnce(context.Background(), 60*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(repo.created) < 2 {
		t.Fatalf("expected >= 2 partitions, got %d", len(repo.created))
	}
	for _, ts := range repo.created {
		if ts.Day() != 1 || ts.Hour() != 0 {
			t.Fatalf("not month start: %v", ts)
		}
	}
}

func TestPartitionMaintainer_ContinuesAfterCreateError(t *testing.T) {
	now := time.Now().UTC()
	firstMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	failKey := firstMonth.Format("2006-01")

	repo := &fakePartRepo{
		failAt: map[string]error{
			failKey: errors.New("permission denied"),
		},
	}
	m := NewPayloadAuditPartitionMaintainer(repo)
	err := m.RunOnce(context.Background(), 90*24*time.Hour)
	if err == nil {
		t.Fatal("expected error from failed month")
	}
	// Should still have created subsequent months
	if len(repo.created) < 1 {
		t.Fatalf("expected >= 1 successful partition, got %d", len(repo.created))
	}
}
