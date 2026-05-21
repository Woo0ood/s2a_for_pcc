package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// PayloadAuditSinkAdapter implements service.PayloadAuditRepository.
// It converts service.PayloadAuditEvent → repository.PayloadAuditEvent
// and delegates to the real PayloadAuditRepo.BatchInsert.
type PayloadAuditSinkAdapter struct {
	repo *PayloadAuditRepo
}

func NewPayloadAuditSinkAdapter(repo *PayloadAuditRepo) *PayloadAuditSinkAdapter {
	return &PayloadAuditSinkAdapter{repo: repo}
}

func (a *PayloadAuditSinkAdapter) BatchInsert(ctx context.Context, events []*service.PayloadAuditEvent) error {
	repoEvents := make([]*PayloadAuditEvent, len(events))
	for i, e := range events {
		repoEvents[i] = &PayloadAuditEvent{
			ID:              e.ID,
			CreatedAt:       e.CreatedAt,
			RequestID:       e.RequestID,
			UserID:          e.UserID,
			UserEmail:       e.UserEmail,
			APIKeyID:        e.APIKeyID,
			APIKeyName:      e.APIKeyName,
			GroupID:         e.GroupID,
			GroupName:       e.GroupName,
			ClientIP:        e.ClientIP,
			Endpoint:        e.Endpoint,
			Provider:        e.Provider,
			Model:           e.Model,
			UpstreamModel:   e.UpstreamModel,
			Stream:          e.Stream,
			StatusCode:      e.StatusCode,
			DurationMs:      e.DurationMs,
			InputExcerpt:    e.InputExcerpt,
			OutputExcerpt:   e.OutputExcerpt,
			InputBody:       e.InputBody,
			OutputBody:      e.OutputBody,
			InputFormat:     e.InputFormat,
			OutputFormat:    e.OutputFormat,
			InputBytes:      e.InputBytes,
			OutputBytes:     e.OutputBytes,
			InputTruncated:  e.InputTruncated,
			OutputTruncated: e.OutputTruncated,
			OutputOmitted:   e.OutputOmitted,
			ErrorMessage:    e.ErrorMessage,
		}
	}
	return a.repo.BatchInsert(ctx, repoEvents)
}
