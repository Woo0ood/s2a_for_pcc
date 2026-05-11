package repository

import (
	"context"
	"time"

	"github.com/Woo0ood/sub2api/internal/service"
)

// PayloadAuditCleanupAdapter implements service.PayloadAuditCleanupRepo
// by delegating to PayloadAuditRepo and converting between
// repository and service partition types.
type PayloadAuditCleanupAdapter struct {
	repo *PayloadAuditRepo
}

func NewPayloadAuditCleanupAdapter(repo *PayloadAuditRepo) *PayloadAuditCleanupAdapter {
	return &PayloadAuditCleanupAdapter{repo: repo}
}

func (a *PayloadAuditCleanupAdapter) ListPartitionsBefore(ctx context.Context, cutoff time.Time) ([]service.PayloadAuditPartition, error) {
	parts, err := a.repo.ListPartitionsBefore(ctx, cutoff)
	if err != nil {
		return nil, err
	}
	out := make([]service.PayloadAuditPartition, len(parts))
	for i, p := range parts {
		out[i] = service.PayloadAuditPartition{Name: p.Name, End: p.End}
	}
	return out, nil
}

func (a *PayloadAuditCleanupAdapter) PartitionState(ctx context.Context, name string) (string, error) {
	return a.repo.PartitionState(ctx, name)
}

func (a *PayloadAuditCleanupAdapter) DetachPartitionConcurrently(ctx context.Context, name string) error {
	return a.repo.DetachPartitionConcurrently(ctx, name)
}

func (a *PayloadAuditCleanupAdapter) FinalizePartitionDetach(ctx context.Context, name string) error {
	return a.repo.FinalizePartitionDetach(ctx, name)
}

func (a *PayloadAuditCleanupAdapter) DropPartition(ctx context.Context, name string) error {
	return a.repo.DropPartition(ctx, name)
}

// PayloadAuditPartitionMaintainerAdapter implements service.PayloadAuditPartitionMaintainerRepo.
type PayloadAuditPartitionMaintainerAdapter struct {
	repo *PayloadAuditRepo
}

func NewPayloadAuditPartitionMaintainerAdapter(repo *PayloadAuditRepo) *PayloadAuditPartitionMaintainerAdapter {
	return &PayloadAuditPartitionMaintainerAdapter{repo: repo}
}

func (a *PayloadAuditPartitionMaintainerAdapter) CreatePartition(ctx context.Context, monthStart time.Time) error {
	return a.repo.CreatePartition(ctx, monthStart)
}
