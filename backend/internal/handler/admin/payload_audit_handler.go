package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Woo0ood/s2a_for_pcc/internal/pkg/response"
	"github.com/Woo0ood/s2a_for_pcc/internal/repository"
	"github.com/Woo0ood/s2a_for_pcc/internal/service"
	"github.com/gin-gonic/gin"
)

// PayloadAuditAdminRepo is the subset of repository.PayloadAuditRepo
// needed by the admin handler.
type PayloadAuditAdminRepo interface {
	List(ctx context.Context, filter repository.PayloadAuditListFilter) ([]*repository.PayloadAuditRow, *repository.PayloadAuditCursor, error)
	Get(ctx context.Context, id int64, createdAt time.Time) (*repository.PayloadAuditRow, error)
	ListPartitionsBefore(ctx context.Context, cutoff time.Time) ([]repository.PayloadAuditPartition, error)
}

// PayloadAuditAdminHandler exposes payload audit management endpoints.
type PayloadAuditAdminHandler struct {
	svc     *service.PayloadAuditService
	sink    *service.PayloadAuditSink
	cleanup *service.PayloadAuditCleanup
	repo    PayloadAuditAdminRepo
}

// NewPayloadAuditAdminHandler constructs a PayloadAuditAdminHandler.
func NewPayloadAuditAdminHandler(
	svc *service.PayloadAuditService,
	sink *service.PayloadAuditSink,
	cleanup *service.PayloadAuditCleanup,
	repo PayloadAuditAdminRepo,
) *PayloadAuditAdminHandler {
	return &PayloadAuditAdminHandler{svc: svc, sink: sink, cleanup: cleanup, repo: repo}
}

// --------------- GetConfig ---------------

type payloadAuditConfigResponse struct {
	Enabled bool                          `json:"enabled"`
	Config  payloadAuditConfigResponseCfg `json:"config"`
}

type payloadAuditConfigResponseCfg struct {
	AllGroups      bool                                 `json:"all_groups"`
	GroupIDs       []int64                              `json:"group_ids"`
	InputMaxBytes  int                                  `json:"input_max_bytes"`
	OutputMaxBytes int                                  `json:"output_max_bytes"`
	ExcerptBytes   int                                  `json:"excerpt_bytes"`
	RetentionDays  int                                  `json:"retention_days"`
	WorkerCount    int                                  `json:"worker_count"`
	QueueSize      int                                  `json:"queue_size"`
	QueueMaxBytes  int                                  `json:"queue_max_bytes"`
	BatchSize      int                                  `json:"batch_size"`
	BatchFlushMs   int                                  `json:"batch_flush_ms"`
	ExportAPIKeys  []payloadAuditExportKeyRedactedEntry `json:"export_api_keys"`
}

type payloadAuditExportKeyRedactedEntry struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	RateLimitPerMin int       `json:"rate_limit_per_min"`
	CreatedAt       time.Time `json:"created_at"`
	Disabled        bool      `json:"disabled"`
}

func redactExportKeys(keys []service.PayloadAuditExportKey) []payloadAuditExportKeyRedactedEntry {
	out := make([]payloadAuditExportKeyRedactedEntry, len(keys))
	for i, k := range keys {
		out[i] = payloadAuditExportKeyRedactedEntry{
			ID:              k.ID,
			Name:            k.Name,
			RateLimitPerMin: k.RateLimitPerMin,
			CreatedAt:       k.CreatedAt,
			Disabled:        k.Disabled,
		}
	}
	return out
}

// snapshotGroupIDs converts the map[int64]struct{} back to a slice.
func snapshotGroupIDs(m map[int64]struct{}) []int64 {
	if len(m) == 0 {
		return []int64{}
	}
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return out
}

// GetConfig returns the current payload audit configuration.
// GET /admin/payload-audit/config
func (h *PayloadAuditAdminHandler) GetConfig(c *gin.Context) {
	snap := h.svc.Snapshot()
	if snap == nil {
		response.Success(c, payloadAuditConfigResponse{})
		return
	}
	response.Success(c, payloadAuditConfigResponse{
		Enabled: snap.Enabled,
		Config: payloadAuditConfigResponseCfg{
			AllGroups:      snap.AllGroups,
			GroupIDs:       snapshotGroupIDs(snap.GroupIDs),
			InputMaxBytes:  snap.InputMaxBytes,
			OutputMaxBytes: snap.OutputMaxBytes,
			ExcerptBytes:   snap.ExcerptBytes,
			RetentionDays:  snap.RetentionDays,
			WorkerCount:    snap.WorkerCount,
			QueueSize:      snap.QueueSize,
			QueueMaxBytes:  snap.QueueMaxBytes,
			BatchSize:      snap.BatchSize,
			BatchFlushMs:   snap.BatchFlushMs,
			ExportAPIKeys:  redactExportKeys(snap.ExportKeys),
		},
	})
}

// --------------- UpdateConfig ---------------

type updatePayloadAuditConfigRequest struct {
	Enabled *bool                       `json:"enabled"`
	Config  *service.PayloadAuditConfig `json:"config"`
}

// UpdateConfig validates and persists new payload audit configuration.
// PUT /admin/payload-audit/config
func (h *PayloadAuditAdminHandler) UpdateConfig(c *gin.Context) {
	var req updatePayloadAuditConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.Config == nil {
		response.BadRequest(c, "config is required")
		return
	}

	enabled := false
	if req.Enabled != nil {
		enabled = *req.Enabled
	} else {
		// Preserve current state if not specified.
		snap := h.svc.Snapshot()
		if snap != nil {
			enabled = snap.Enabled
		}
	}

	// Preserve existing export keys — they are managed via dedicated endpoints.
	snap := h.svc.Snapshot()
	if snap != nil {
		req.Config.ExportAPIKeys = make([]service.PayloadAuditExportKey, len(snap.ExportKeys))
		copy(req.Config.ExportAPIKeys, snap.ExportKeys)
	}

	needRebuild, err := h.svc.UpdateConfig(c.Request.Context(), enabled, *req.Config)
	if err != nil {
		if errors.Is(err, service.ErrInvalidPayloadAuditConfig) {
			response.BadRequest(c, err.Error())
			return
		}
		slog.Error("payload_audit.admin_update_config_fail", "err", err)
		response.Error(c, http.StatusInternalServerError, "failed to update config")
		return
	}
	response.Success(c, gin.H{"need_rebuild_sink": needRebuild})
}

// --------------- GetStatus ---------------

type payloadAuditStatusResponse struct {
	Enabled bool                      `json:"enabled"`
	Workers payloadAuditWorkerStatus  `json:"workers"`
	Queue   payloadAuditQueueStatus   `json:"queue"`
	Stats   service.SinkStats         `json:"stats_24h"`
	Storage payloadAuditStorageStatus `json:"storage"`
}

type payloadAuditWorkerStatus struct {
	Configured int `json:"configured"`
}

type payloadAuditQueueStatus struct {
	Size      int   `json:"size"`
	Depth     int64 `json:"depth"`
	UsagePct  float64 `json:"usage_pct"`
	BytesUsed int64 `json:"bytes_used"`
	BytesMax  int   `json:"bytes_max"`
}

type payloadAuditStorageStatus struct {
	CurrentPartition string                        `json:"current_partition"`
	Partitions       []repository.PayloadAuditPartition `json:"partitions"`
}

// GetStatus returns runtime status of the payload audit system.
// GET /admin/payload-audit/status
func (h *PayloadAuditAdminHandler) GetStatus(c *gin.Context) {
	snap := h.svc.Snapshot()
	stats := h.sink.Stats()

	workerCount := 0
	queueSize := 0
	queueMaxBytes := 0
	enabled := false
	if snap != nil {
		workerCount = snap.WorkerCount
		queueSize = snap.QueueSize
		queueMaxBytes = snap.QueueMaxBytes
		enabled = snap.Enabled
	}

	// Queue usage percentage.
	var usagePct float64
	if queueSize > 0 {
		usagePct = float64(stats.QueueDepth) / float64(queueSize) * 100
	}

	// Current partition name.
	now := time.Now().UTC()
	currentPartition := fmt.Sprintf("payload_audit_logs_%04d_%02d", now.Year(), now.Month())

	// Fetch all known partitions (look ahead 1 year).
	cutoff := now.Add(365 * 24 * time.Hour)
	partitions, _ := h.repo.ListPartitionsBefore(c.Request.Context(), cutoff)
	if partitions == nil {
		partitions = []repository.PayloadAuditPartition{}
	}

	response.Success(c, payloadAuditStatusResponse{
		Enabled: enabled,
		Workers: payloadAuditWorkerStatus{
			Configured: workerCount,
		},
		Queue: payloadAuditQueueStatus{
			Size:      queueSize,
			Depth:     stats.QueueDepth,
			UsagePct:  usagePct,
			BytesUsed: stats.QueueBytesUsed,
			BytesMax:  queueMaxBytes,
		},
		Stats:   stats,
		Storage: payloadAuditStorageStatus{
			CurrentPartition: currentPartition,
			Partitions:       partitions,
		},
	})
}

// --------------- ListPayloads ---------------

const (
	includeBodyNone    = "none"
	includeBodyExcerpt = "excerpt"
	includeBodyFull    = "full"
	maxTimeWindowDays  = 31
)

// ListPayloads lists payload audit log entries.
// GET /admin/payload-audit/payloads
func (h *PayloadAuditAdminHandler) ListPayloads(c *gin.Context) {
	fromStr := strings.TrimSpace(c.Query("from"))
	toStr := strings.TrimSpace(c.Query("to"))

	if fromStr == "" || toStr == "" {
		response.BadRequest(c, "from and to are required")
		return
	}

	from, err := parsePayloadAuditTime(fromStr)
	if err != nil {
		response.BadRequest(c, "Invalid from: "+err.Error())
		return
	}
	to, err := parsePayloadAuditTime(toStr)
	if err != nil {
		response.BadRequest(c, "Invalid to: "+err.Error())
		return
	}

	if to.Sub(from) > maxTimeWindowDays*24*time.Hour {
		response.BadRequest(c, fmt.Sprintf("Time window exceeds %d days", maxTimeWindowDays))
		return
	}
	if to.Before(from) {
		response.BadRequest(c, "to must be after from")
		return
	}

	filter := repository.PayloadAuditListFilter{
		From: from,
		To:   to,
	}

	if v := strings.TrimSpace(c.Query("user_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid user_id")
			return
		}
		filter.UserID = &id
	}
	if v := strings.TrimSpace(c.Query("group_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		filter.GroupID = &id
	}
	if v := strings.TrimSpace(c.Query("api_key_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid api_key_id")
			return
		}
		filter.APIKeyID = &id
	}
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			response.BadRequest(c, "Invalid limit")
			return
		}
		if n > 500 {
			n = 500
		}
		filter.Limit = n
	}
	if v := strings.TrimSpace(c.Query("keyword")); v != "" {
		if filter.To.Sub(filter.From) > 7*24*time.Hour {
			response.BadRequest(c, "keyword search requires time window <= 7 days")
			return
		}
		filter.KeywordILike = v
	}
	if v := strings.TrimSpace(c.Query("cursor")); v != "" {
		cur, err := repository.DecodeCursor(v)
		if err != nil {
			response.BadRequest(c, "Invalid cursor: "+err.Error())
			return
		}
		filter.Cursor = cur
	}

	if filter.Limit <= 0 {
		filter.Limit = 100
	} else if filter.Limit > 500 {
		filter.Limit = 500
	}

	rows, nextCursor, err := h.repo.List(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	includeBody := strings.TrimSpace(c.DefaultQuery("include_body", includeBodyExcerpt))
	switch includeBody {
	case includeBodyNone:
		for _, r := range rows {
			r.InputExcerpt = ""
			r.OutputExcerpt = ""
			r.InputBody = ""
			r.OutputBody = ""
		}
	case includeBodyExcerpt, "":
		for _, r := range rows {
			r.InputBody = ""
			r.OutputBody = ""
		}
	case includeBodyFull:
		// return as-is
	default:
		response.BadRequest(c, "Invalid include_body: must be none, excerpt, or full")
		return
	}

	var nextCursorStr string
	if nextCursor != nil {
		nextCursorStr, _ = repository.EncodeCursor(nextCursor)
	}

	response.Success(c, gin.H{
		"data":        rows,
		"next_cursor": nextCursorStr,
	})
}

// --------------- GetPayload ---------------

// GetPayload returns a single payload audit log entry.
// GET /admin/payload-audit/payloads/:id
func (h *PayloadAuditAdminHandler) GetPayload(c *gin.Context) {
	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid id")
		return
	}

	var createdAt time.Time
	if v := strings.TrimSpace(c.Query("created_at")); v != "" {
		t, err := parsePayloadAuditTime(v)
		if err != nil {
			response.BadRequest(c, "Invalid created_at: "+err.Error())
			return
		}
		createdAt = t
	}

	row, err := h.repo.Get(c.Request.Context(), id, createdAt)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if row == nil {
		response.Error(c, http.StatusNotFound, "Payload not found")
		return
	}
	response.Success(c, row)
}

// --------------- ListExportKeys ---------------

type exportKeyListEntry struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	RateLimitPerMin int        `json:"rate_limit_per_min"`
	CreatedAt       time.Time  `json:"created_at"`
	Disabled        bool       `json:"disabled"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
}

// ListExportKeys returns all export keys with last-used timestamps.
// GET /admin/payload-audit/export-keys
func (h *PayloadAuditAdminHandler) ListExportKeys(c *gin.Context) {
	keys, err := h.svc.ListExportKeys(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]exportKeyListEntry, len(keys))
	for i, k := range keys {
		entry := exportKeyListEntry{
			ID:              k.ID,
			Name:            k.Name,
			RateLimitPerMin: k.RateLimitPerMin,
			CreatedAt:       k.CreatedAt,
			Disabled:        k.Disabled,
		}
		if t, ok := h.svc.ExportKeyLastUsed(c.Request.Context(), k.ID); ok {
			entry.LastUsedAt = &t
		}
		out[i] = entry
	}
	response.Success(c, out)
}

// --------------- CreateExportKey ---------------

type createExportKeyRequest struct {
	Name            string `json:"name"`
	RateLimitPerMin int    `json:"rate_limit_per_min"`
}

// CreateExportKey generates a new export API key.
// POST /admin/payload-audit/export-keys
func (h *PayloadAuditAdminHandler) CreateExportKey(c *gin.Context) {
	var req createExportKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		response.BadRequest(c, "name is required")
		return
	}

	tok, key, err := h.svc.CreateExportKey(c.Request.Context(), req.Name, req.RateLimitPerMin)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"token": tok,
		"key": payloadAuditExportKeyRedactedEntry{
			ID:              key.ID,
			Name:            key.Name,
			RateLimitPerMin: key.RateLimitPerMin,
			CreatedAt:       key.CreatedAt,
			Disabled:        key.Disabled,
		},
	})
}

// --------------- DeleteExportKey ---------------

// DeleteExportKey removes an export API key by id.
// DELETE /admin/payload-audit/export-keys/:id
func (h *PayloadAuditAdminHandler) DeleteExportKey(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		response.BadRequest(c, "id is required")
		return
	}
	if err := h.svc.DeleteExportKey(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true})
}

// --------------- RunCleanup ---------------

// RunCleanup triggers a manual partition cleanup.
// POST /admin/payload-audit/cleanup
func (h *PayloadAuditAdminHandler) RunCleanup(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	start := time.Now()
	deleted, err := h.cleanup.RunOnce(ctx)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"deleted":     deleted,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

// --------------- helpers ---------------

func parsePayloadAuditTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty time string")
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", raw)
	return t, err
}
