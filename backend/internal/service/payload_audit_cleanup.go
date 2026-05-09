package service

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PayloadAuditCleanupRepo is the subset of repository methods needed by cleanup.
type PayloadAuditCleanupRepo interface {
	ListPartitionsBefore(ctx context.Context, cutoff time.Time) ([]PayloadAuditPartition, error)
	PartitionState(ctx context.Context, name string) (string, error)
	DetachPartitionConcurrently(ctx context.Context, name string) error
	FinalizePartitionDetach(ctx context.Context, name string) error
	DropPartition(ctx context.Context, name string) error
}

// PayloadAuditPartition holds metadata about a single partition (service-layer type).
type PayloadAuditPartition struct {
	Name string
	End  time.Time
}

const (
	PartitionStateAttached      = "ATTACHED"
	PartitionStateDetachPending = "DETACH_PENDING"
	PartitionStateDetached      = "DETACHED"
	PartitionStateUnknown       = "UNKNOWN"
)

// PayloadAuditCleanup drives the partition cleanup state machine.
type PayloadAuditCleanup struct {
	repo PayloadAuditCleanupRepo
	svc  *PayloadAuditService
}

// NewPayloadAuditCleanup creates a cleanup instance.
func NewPayloadAuditCleanup(repo PayloadAuditCleanupRepo, svc *PayloadAuditService) *PayloadAuditCleanup {
	return &PayloadAuditCleanup{repo: repo, svc: svc}
}

// RunOnce executes one cleanup cycle, returning the number of partitions dropped.
//
// State machine per partition:
//
//	ATTACHED       → DetachPartitionConcurrently (do NOT finalize in the same round)
//	DETACH_PENDING → FinalizePartitionDetach → DropPartition
//	DETACHED       → DropPartition
//	UNKNOWN        → skip
func (c *PayloadAuditCleanup) RunOnce(ctx context.Context) (deleted int, err error) {
	snap := c.svc.Snapshot()
	if snap == nil {
		return 0, errors.New("payload audit config not loaded")
	}
	retention := snap.RetentionDays
	if retention < 1 {
		retention = 180
	}
	cutoff := time.Now().UTC().Add(-time.Duration(retention) * 24 * time.Hour)

	parts, err := c.repo.ListPartitionsBefore(ctx, cutoff)
	if err != nil {
		return 0, err
	}

	for _, p := range parts {
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		state, err := c.repo.PartitionState(ctx, p.Name)
		if err != nil {
			slog.Error("payload_audit.cleanup_state_fail", "partition", p.Name, "err", err)
			continue
		}
		switch state {
		case PartitionStateAttached:
			if err := c.repo.DetachPartitionConcurrently(ctx, p.Name); err != nil {
				slog.Error("payload_audit.cleanup_detach_fail", "partition", p.Name, "err", err)
				continue
			}
			slog.Info("payload_audit.cleanup_detach_started", "partition", p.Name)
			// Do not finalize in the same round; let PG complete the detach first.

		case PartitionStateDetachPending:
			if err := c.repo.FinalizePartitionDetach(ctx, p.Name); err != nil {
				slog.Error("payload_audit.cleanup_finalize_fail", "partition", p.Name, "err", err)
				continue
			}
			if err := c.repo.DropPartition(ctx, p.Name); err != nil {
				slog.Error("payload_audit.cleanup_drop_fail_after_finalize", "partition", p.Name, "err", err)
				continue
			}
			deleted++
			slog.Info("payload_audit.cleanup_dropped", "partition", p.Name, "via", "finalize")

		case PartitionStateDetached:
			if err := c.repo.DropPartition(ctx, p.Name); err != nil {
				slog.Error("payload_audit.cleanup_drop_fail", "partition", p.Name, "err", err)
				continue
			}
			deleted++
			slog.Info("payload_audit.cleanup_dropped", "partition", p.Name, "via", "direct")

		case PartitionStateUnknown:
			slog.Warn("payload_audit.cleanup_unknown_partition", "partition", p.Name)
		}
	}
	return deleted, nil
}

// === Partition Maintainer ===

// PayloadAuditPartitionMaintainerRepo is the subset needed for partition creation.
type PayloadAuditPartitionMaintainerRepo interface {
	CreatePartition(ctx context.Context, monthStart time.Time) error
}

// PayloadAuditPartitionMaintainer pre-creates future monthly partitions.
type PayloadAuditPartitionMaintainer struct {
	repo PayloadAuditPartitionMaintainerRepo
}

// NewPayloadAuditPartitionMaintainer creates a maintainer instance.
func NewPayloadAuditPartitionMaintainer(repo PayloadAuditPartitionMaintainerRepo) *PayloadAuditPartitionMaintainer {
	return &PayloadAuditPartitionMaintainer{repo: repo}
}

// RunOnce creates monthly partitions from the current month up to now+lookahead.
// Individual creation failures are logged but do not stop subsequent months.
func (m *PayloadAuditPartitionMaintainer) RunOnce(ctx context.Context, lookahead time.Duration) error {
	if lookahead <= 0 {
		lookahead = 60 * 24 * time.Hour
	}
	now := time.Now().UTC()
	end := now.Add(lookahead)
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var lastErr error
	for cur.Before(end) {
		if err := m.repo.CreatePartition(ctx, cur); err != nil {
			slog.Error("payload_audit.partition_create_fail", "month", cur.Format("2006-01"), "err", err)
			lastErr = err
		}
		cur = cur.AddDate(0, 1, 0)
	}
	return lastErr
}
