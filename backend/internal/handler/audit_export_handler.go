package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Woo0ood/s2a_for_pcc/internal/pkg/response"
	"github.com/Woo0ood/s2a_for_pcc/internal/repository" //nolint:depguard // export API uses repo filter/cursor types
	"github.com/Woo0ood/s2a_for_pcc/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

// AuditExportRepo is the subset of repository.PayloadAuditRepo needed by the export handler.
type AuditExportRepo interface {
	List(ctx context.Context, filter repository.PayloadAuditListFilter) ([]*repository.PayloadAuditRow, *repository.PayloadAuditCursor, error)
	Get(ctx context.Context, id int64, createdAt time.Time) (*repository.PayloadAuditRow, error)
}

// AuditExportOpsLogger is an optional logger for recording export access events.
// If nil, access logging is skipped.
type AuditExportOpsLogger interface {
	Log(keyName string, params string, rowCount int, durationMs int64)
}

// slogAuditExportOpsLogger logs access events via slog (placeholder implementation).
type slogAuditExportOpsLogger struct{}

func (s *slogAuditExportOpsLogger) Log(keyName string, params string, rowCount int, durationMs int64) {
	slog.Info("payload_audit.export_access",
		"key_name", keyName,
		"params", params,
		"row_count", rowCount,
		"duration_ms", durationMs,
	)
}

// NewSlogAuditExportOpsLogger returns a placeholder ops logger that writes to slog.
func NewSlogAuditExportOpsLogger() AuditExportOpsLogger {
	return &slogAuditExportOpsLogger{}
}

// AuditExportHandler handles external audit export API endpoints.
type AuditExportHandler struct {
	repo      AuditExportRepo
	opsLogger AuditExportOpsLogger // may be nil
}

// NewAuditExportHandler constructs an AuditExportHandler.
func NewAuditExportHandler(repo AuditExportRepo, opsLogger AuditExportOpsLogger) *AuditExportHandler {
	return &AuditExportHandler{repo: repo, opsLogger: opsLogger}
}

// VerifyAuth confirms that the caller's bearer token is valid.
// POST /api/v1/audit/auth/verify
func (h *AuditExportHandler) VerifyAuth(c *gin.Context) {
	keyName, _ := c.Get(middleware.AuditExportKeyNameCtxKey)
	c.JSON(http.StatusOK, gin.H{"key_name": keyName})
}

// --------------- shared constants ---------------

const (
	exportIncludeBodyNone     = "none"
	exportIncludeBodyExcerpt  = "excerpt"
	exportIncludeBodyFull     = "full"
	exportMaxTimeWindowDays   = 31
	exportMaxTimeWindowNDJSON = 7
)

// --------------- list filter parsing ---------------

func parseExportListFilter(c *gin.Context) (repository.PayloadAuditListFilter, string, error) {
	fromStr := strings.TrimSpace(c.Query("from"))
	toStr := strings.TrimSpace(c.Query("to"))

	if fromStr == "" || toStr == "" {
		return repository.PayloadAuditListFilter{}, "", fmt.Errorf("from and to are required")
	}

	from, err := parseExportTime(fromStr)
	if err != nil {
		return repository.PayloadAuditListFilter{}, "", fmt.Errorf("Invalid from: %s", err.Error())
	}
	to, err := parseExportTime(toStr)
	if err != nil {
		return repository.PayloadAuditListFilter{}, "", fmt.Errorf("Invalid to: %s", err.Error())
	}

	if to.Before(from) {
		return repository.PayloadAuditListFilter{}, "", fmt.Errorf("to must be after from")
	}
	if to.Sub(from) > exportMaxTimeWindowDays*24*time.Hour {
		return repository.PayloadAuditListFilter{}, "", fmt.Errorf("Time window exceeds %d days", exportMaxTimeWindowDays)
	}

	filter := repository.PayloadAuditListFilter{
		From: from,
		To:   to,
	}

	if v := strings.TrimSpace(c.Query("user_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return repository.PayloadAuditListFilter{}, "", fmt.Errorf("Invalid user_id")
		}
		filter.UserID = &id
	}
	if v := strings.TrimSpace(c.Query("group_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return repository.PayloadAuditListFilter{}, "", fmt.Errorf("Invalid group_id")
		}
		filter.GroupID = &id
	}
	if v := strings.TrimSpace(c.Query("api_key_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return repository.PayloadAuditListFilter{}, "", fmt.Errorf("Invalid api_key_id")
		}
		filter.APIKeyID = &id
	}

	// > 7 days requires user_id or group_id filter
	if to.Sub(from) > 7*24*time.Hour && filter.UserID == nil && filter.GroupID == nil {
		return repository.PayloadAuditListFilter{}, "", fmt.Errorf("time window > 7 days requires user_id or group_id filter")
	}

	if v := strings.TrimSpace(c.Query("keyword")); v != "" {
		if filter.To.Sub(filter.From) > 7*24*time.Hour {
			return repository.PayloadAuditListFilter{}, "", fmt.Errorf("keyword search requires time window <= 7 days")
		}
		filter.KeywordILike = v
	}

	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return repository.PayloadAuditListFilter{}, "", fmt.Errorf("Invalid limit")
		}
		filter.Limit = n
	}

	// Clamp limit to [1, 500], default 100
	if filter.Limit <= 0 {
		filter.Limit = 100
	} else if filter.Limit > 500 {
		filter.Limit = 500
	}

	if v := strings.TrimSpace(c.Query("cursor")); v != "" {
		cur, err := repository.DecodeCursor(v)
		if err != nil {
			return repository.PayloadAuditListFilter{}, "", fmt.Errorf("Invalid cursor: %s", err.Error())
		}
		filter.Cursor = cur
	}

	// Build a summary of params for access logging
	paramSummary := fmt.Sprintf("from=%s&to=%s", fromStr, toStr)
	if filter.UserID != nil {
		paramSummary += fmt.Sprintf("&user_id=%d", *filter.UserID)
	}
	if filter.GroupID != nil {
		paramSummary += fmt.Sprintf("&group_id=%d", *filter.GroupID)
	}
	if filter.KeywordILike != "" {
		paramSummary += "&keyword=" + filter.KeywordILike
	}

	return filter, paramSummary, nil
}

func applyIncludeBodyMode(rows []*repository.PayloadAuditRow, mode string) {
	switch mode {
	case exportIncludeBodyNone:
		for _, r := range rows {
			r.InputExcerpt = ""
			r.OutputExcerpt = ""
			r.InputBody = ""
			r.OutputBody = ""
		}
	case exportIncludeBodyExcerpt, "":
		for _, r := range rows {
			r.InputBody = ""
			r.OutputBody = ""
		}
	case exportIncludeBodyFull:
		// return as-is
	}
}

// --------------- ListPayloads ---------------

// ListPayloads lists payload audit log entries with cursor pagination.
// GET /api/v1/audit/exports/payloads
func (h *AuditExportHandler) ListPayloads(c *gin.Context) {
	start := time.Now()

	filter, paramSummary, err := parseExportListFilter(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	includeBody := strings.TrimSpace(c.DefaultQuery("include_body", exportIncludeBodyExcerpt))
	switch includeBody {
	case exportIncludeBodyNone, exportIncludeBodyExcerpt, exportIncludeBodyFull:
		// ok
	default:
		response.BadRequest(c, "Invalid include_body: must be none, excerpt, or full")
		return
	}

	rows, nextCursor, err := h.repo.List(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	applyIncludeBodyMode(rows, includeBody)

	var nextCursorStr string
	if nextCursor != nil {
		nextCursorStr, _ = repository.EncodeCursor(nextCursor)
	}

	c.JSON(http.StatusOK, gin.H{
		"data":        rows,
		"next_cursor": nextCursorStr,
	})

	// Async access log
	h.logAccess(c, paramSummary, len(rows), time.Since(start).Milliseconds())
}

// --------------- GetPayload ---------------

// GetPayload returns a single payload audit entry with full body.
// GET /api/v1/audit/exports/payloads/:id
func (h *AuditExportHandler) GetPayload(c *gin.Context) {
	start := time.Now()

	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid id")
		return
	}

	var createdAt time.Time
	if v := strings.TrimSpace(c.Query("created_at")); v != "" {
		t, err := parseExportTime(v)
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

	c.JSON(http.StatusOK, gin.H{"data": row})

	// Async access log
	h.logAccess(c, fmt.Sprintf("id=%d", id), 1, time.Since(start).Milliseconds())
}

// --------------- StreamNDJSON ---------------

// StreamNDJSON streams payload audit entries as newline-delimited JSON.
// GET /api/v1/audit/exports/payloads.ndjson
func (h *AuditExportHandler) StreamNDJSON(c *gin.Context) {
	start := time.Now()

	filter, paramSummary, err := parseExportListFilter(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	// NDJSON: enforce <= 7 day window
	if filter.To.Sub(filter.From) > exportMaxTimeWindowNDJSON*24*time.Hour {
		response.BadRequest(c, fmt.Sprintf("NDJSON streaming requires time window <= %d days", exportMaxTimeWindowNDJSON))
		return
	}

	// Force full body for NDJSON
	// include_body is forced to full

	c.Header("Content-Type", "application/x-ndjson")
	c.Status(http.StatusOK)

	totalRows := 0
	for {
		rows, nextCursor, err := h.repo.List(c.Request.Context(), filter)
		if err != nil {
			// Mid-stream error: write error line and abort
			errLine, _ := json.Marshal(gin.H{"error": err.Error()})
			_, _ = c.Writer.Write(errLine)
			_, _ = c.Writer.Write([]byte("\n"))
			c.Writer.Flush()
			break
		}

		for _, row := range rows {
			line, marshalErr := json.Marshal(row)
			if marshalErr != nil {
				errLine, _ := json.Marshal(gin.H{"error": marshalErr.Error()})
				_, _ = c.Writer.Write(errLine)
				_, _ = c.Writer.Write([]byte("\n"))
				c.Writer.Flush()
				h.logAccess(c, paramSummary, totalRows, time.Since(start).Milliseconds())
				return
			}
			_, _ = c.Writer.Write(line)
			_, _ = c.Writer.Write([]byte("\n"))
		}
		c.Writer.Flush()
		totalRows += len(rows)

		if nextCursor == nil {
			break
		}
		filter.Cursor = nextCursor
	}

	h.logAccess(c, paramSummary, totalRows, time.Since(start).Milliseconds())
}

// --------------- helpers ---------------

func (h *AuditExportHandler) logAccess(c *gin.Context, params string, rowCount int, durationMs int64) {
	if h.opsLogger == nil {
		return
	}
	keyName, _ := c.Get(middleware.AuditExportKeyNameCtxKey)
	name, _ := keyName.(string)
	go h.opsLogger.Log(name, params, rowCount, durationMs)
}

func parseExportTime(raw string) (time.Time, error) {
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
