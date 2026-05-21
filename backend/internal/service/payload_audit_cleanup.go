package service

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PayloadAuditCleanupRepo is the subset of repository methods needed by cleanup.
// After the ClickHouse migration this is a simple drop-expired-partitions call.
type PayloadAuditCleanupRepo interface {
	DropExpiredMonthlyPartitions(ctx context.Context, cutoff time.Time) ([]string, error)
}

// PayloadAuditCleanup drives cleanup of expired ClickHouse partitions.
type PayloadAuditCleanup struct {
	repo PayloadAuditCleanupRepo
	svc  *PayloadAuditService
}

// NewPayloadAuditCleanup creates a cleanup instance.
func NewPayloadAuditCleanup(repo PayloadAuditCleanupRepo, svc *PayloadAuditService) *PayloadAuditCleanup {
	return &PayloadAuditCleanup{repo: repo, svc: svc}
}

// RunOnce executes one cleanup cycle, returning the number of partitions dropped.
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

	dropped, err := c.repo.DropExpiredMonthlyPartitions(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	for _, p := range dropped {
		slog.Info("payload_audit.cleanup_dropped", "partition", p)
	}
	return len(dropped), nil
}
