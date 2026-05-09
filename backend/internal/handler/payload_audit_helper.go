package handler

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AttachPayloadAuditCollector creates a collector at handler entry and fills metadata.
// Always returns non-nil (disabled collector when svc is nil or audit is off).
//
// Call at the top of the handler, right after reading the request body.
// Immediately defer FinalizePayloadAudit afterwards.
func AttachPayloadAuditCollector(c *gin.Context, svc *service.PayloadAuditService, provider, endpoint string) *service.PayloadAuditCollector {
	if svc == nil {
		return service.NewPayloadAuditCollector(nil)
	}
	snap := svc.Snapshot()
	coll := service.NewPayloadAuditCollector(snap)
	if !coll.Enabled() {
		return coll
	}

	meta := service.PayloadAuditMetadata{
		Endpoint: endpoint,
		Provider: provider,
		ClientIP: c.ClientIP(),
	}
	// AuthSubject only carries UserID; other identity fields (APIKeyID,
	// APIKeyName, UserEmail, GroupID, GroupName) are set by the caller
	// from the resolved APIKey, mirroring the content-moderation helper pattern.
	if subj, ok := middleware.GetAuthSubjectFromContext(c); ok {
		if subj.UserID > 0 {
			meta.UserID = &subj.UserID
		}
	}
	coll.SetMetadata(meta)
	return coll
}

// FinalizePayloadAudit emits the audit event to the sink on handler exit.
// No-op when collector is nil, disabled, out-of-scope, or already finalized.
func FinalizePayloadAudit(coll *service.PayloadAuditCollector, sink *service.PayloadAuditSink, statusCode int, dur time.Duration, errMsg string) {
	if coll == nil {
		return
	}
	evt := coll.Finalize(statusCode, dur, errMsg)
	if evt == nil {
		return
	}
	if sink != nil {
		sink.TryEnqueue(evt)
	}
}

// int64PtrIfPositive returns a pointer to v if v > 0, otherwise nil.
func int64PtrIfPositive(v int64) *int64 {
	if v > 0 {
		return &v
	}
	return nil
}
