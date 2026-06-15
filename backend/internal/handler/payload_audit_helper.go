package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// offloadUploadTimeout bounds the post-response wait for blob/body uploads
// before falling back to inline retention.
const offloadUploadTimeout = 30 * time.Second

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
// Also records per-event Prometheus observations (input/output bytes, truncated).
func FinalizePayloadAudit(coll *service.PayloadAuditCollector, svc *service.PayloadAuditService, sink *service.PayloadAuditSink, statusCode int, dur time.Duration, errMsg string) {
	if coll == nil {
		return
	}
	// Durable-before-pointer: upload any staged offload objects; only commit the
	// pointer when the object is safely stored, otherwise revert to inline.
	if svc != nil {
		settleOffload(coll, svc.Uploader())
	}
	evt := coll.Finalize(statusCode, dur, errMsg)
	if evt == nil {
		return
	}
	if svc != nil {
		id, err := svc.NextID()
		if err != nil {
			slog.Warn("payload_audit.id_gen_fail", "err", err)
			return
		}
		evt.ID = id
	}
	if sink != nil {
		if pm := sink.PromMetrics(); pm != nil {
			pm.InputBytesHist.Observe(float64(evt.InputBytes))
			pm.OutputBytesHist.Observe(float64(evt.OutputBytes))
			if evt.InputTruncated {
				pm.TruncatedInput.Inc()
			}
			if evt.OutputTruncated {
				pm.TruncatedOutput.Inc()
			}
		}
		sink.TryEnqueue(evt)
	}
}

// settleOffload uploads the collector's staged offload objects (inline blobs and
// an oversized body). On full success it marks the input as offloaded so the
// pointer-bearing body is persisted; on any failure it reverts to inline
// retention (subject to normal truncation) so no evidence is lost to a dangling
// pointer. No-op when offload is disabled (up == nil) or nothing is staged.
func settleOffload(coll *service.PayloadAuditCollector, up *service.PayloadAuditUploader) {
	blobs := coll.PendingBlobs()
	body := coll.PendingBody()
	if len(blobs) == 0 && body == nil {
		return // nothing staged
	}
	if up == nil {
		// Staged for offload but no uploader available (offload misconfigured at
		// runtime): cannot persist the object, so revert to inline rather than
		// leave a dangling pointer with no backing object.
		coll.RevertOffload(coll.OriginalForRevert())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), offloadUploadTimeout)
	defer cancel()
	ok := true
	for _, b := range blobs {
		if err := up.PutBlob(ctx, b); err != nil {
			slog.Warn("payload_audit.blob_upload_fail", "sha256", b.SHA256, "err", err)
			ok = false
			break
		}
	}
	if ok && body != nil {
		if err := up.PutBody(ctx, body.SHA256, body.Data); err != nil {
			slog.Warn("payload_audit.body_upload_fail", "sha256", body.SHA256, "err", err)
			ok = false
		}
	}
	if ok {
		coll.MarkInputOffloaded()
	} else {
		coll.RevertOffload(coll.OriginalForRevert())
	}
}

// int64PtrIfPositive returns a pointer to v if v > 0, otherwise nil.
func int64PtrIfPositive(v int64) *int64 {
	if v > 0 {
		return &v
	}
	return nil
}
