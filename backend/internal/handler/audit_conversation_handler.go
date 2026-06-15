package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AuditConversationRepo is the subset of repository.PayloadAuditCHRepo needed
// by the conversation export handler.
type AuditConversationRepo interface {
	Get(ctx context.Context, id int64, createdAt time.Time) (*repository.PayloadAuditRow, error)
	ListConversation(ctx context.Context, convKey string, from, to time.Time, limit int) ([]*repository.PayloadAuditRow, error)
	// ListByCacheKeyNeedle recovers historical conversations (conversation_key column empty)
	// by searching for a substring needle in input_body within the given user+time window.
	ListByCacheKeyNeedle(ctx context.Context, userID *int64, needle string, from, to time.Time, limit int) ([]*repository.PayloadAuditRow, error)
}

// AuditConversationHandler serves the conversation export and blob proxy endpoints.
type AuditConversationHandler struct {
	repo AuditConversationRepo
	svc  *service.PayloadAuditService
}

// NewAuditConversationHandler constructs an AuditConversationHandler.
func NewAuditConversationHandler(repo AuditConversationRepo, svc *service.PayloadAuditService) *AuditConversationHandler {
	return &AuditConversationHandler{repo: repo, svc: svc}
}

// ProvideAuditConversationRepo adapts the CH repo to AuditConversationRepo.
func ProvideAuditConversationRepo(repo *repository.PayloadAuditCHRepo) AuditConversationRepo {
	return repo
}

// ─────────────────────────────────────────────────────────────────────────────
// Repo → service event shim
// ─────────────────────────────────────────────────────────────────────────────

// repoRowToServiceEvent converts a repository.PayloadAuditRow to a
// *service.PayloadAuditEvent (field-for-field copy).
func repoRowToServiceEvent(row *repository.PayloadAuditRow) *service.PayloadAuditEvent {
	e := row.PayloadAuditEvent // embed — copy all fields at once
	svc := &service.PayloadAuditEvent{
		ID:                  row.ID,
		RequestID:           e.RequestID,
		UserID:              e.UserID,
		APIKeyID:            e.APIKeyID,
		GroupID:             e.GroupID,
		UserEmail:           e.UserEmail,
		APIKeyName:          e.APIKeyName,
		GroupName:           e.GroupName,
		ClientIP:            e.ClientIP,
		Endpoint:            e.Endpoint,
		Provider:            e.Provider,
		Model:               e.Model,
		UpstreamModel:       e.UpstreamModel,
		Stream:              e.Stream,
		StatusCode:          e.StatusCode,
		DurationMs:          e.DurationMs,
		InputExcerpt:        e.InputExcerpt,
		OutputExcerpt:       e.OutputExcerpt,
		InputBody:           e.InputBody,
		OutputBody:          e.OutputBody,
		InputFormat:         e.InputFormat,
		OutputFormat:        e.OutputFormat,
		InputBytes:          e.InputBytes,
		OutputBytes:         e.OutputBytes,
		InputTruncated:      e.InputTruncated,
		OutputTruncated:     e.OutputTruncated,
		OutputOmitted:       e.OutputOmitted,
		InputOffloaded:      e.InputOffloaded,
		ConversationKey:     e.ConversationKey,
		ResponseID:          e.ResponseID,
		PreviousResponseID:  e.PreviousResponseID,
		ErrorMessage:        e.ErrorMessage,
		CreatedAt:           e.CreatedAt,
	}
	return svc
}

func repoRowsToServiceEvents(rows []*repository.PayloadAuditRow) []*service.PayloadAuditEvent {
	out := make([]*service.PayloadAuditEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, repoRowToServiceEvent(r))
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/audit/exports/payloads/:id/conversation
// ─────────────────────────────────────────────────────────────────────────────

// GetConversation exports the full conversation a payload record belongs to as
// a self-contained HTML page.
//
// Query params:
//   - created_at: unix-ms integer OR RFC3339 (required for efficient partition hit)
//   - format:     only "html" accepted; else 400
func (h *AuditConversationHandler) GetConversation(c *gin.Context) {
	// --- parse id ---
	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid id")
		return
	}

	// --- parse format ---
	format := strings.TrimSpace(strings.ToLower(c.DefaultQuery("format", "html")))
	if format != "html" {
		response.BadRequest(c, "Invalid format: only 'html' is supported")
		return
	}

	// --- parse created_at ---
	var createdAt time.Time
	if v := strings.TrimSpace(c.Query("created_at")); v != "" {
		// Try unix-ms integer first.
		if ms, parseErr := strconv.ParseInt(v, 10, 64); parseErr == nil {
			createdAt = time.UnixMilli(ms).UTC()
		} else {
			t, parseErr := parseExportTime(v)
			if parseErr != nil {
				response.BadRequest(c, "Invalid created_at: "+parseErr.Error())
				return
			}
			createdAt = t
		}
	}

	if createdAt.IsZero() {
		response.BadRequest(c, "created_at query parameter is required")
		return
	}

	ctx := c.Request.Context()

	// --- fetch hit row ---
	row, err := h.repo.Get(ctx, id, createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.Error(c, http.StatusNotFound, "Payload not found")
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	if row == nil {
		response.Error(c, http.StatusNotFound, "Payload not found")
		return
	}

	resolver := h.svc.Resolver()

	var events []*service.PayloadAuditEvent
	var historicalKey string // set when historical prompt_cache_key fallback is used
	singleTurn := false

	convKey := row.ConversationKey
	if convKey == "" {
		// Historical fallback: column empty (row predates conversation_key). Recover
		// the conversation by matching prompt_cache_key parsed from the body.
		anchor := createdAt
		if pck, _ := service.ExtractRequestConvIDs(row.Endpoint, []byte(row.InputBody)); pck != "" {
			needle := `prompt_cache_key":"` + pck
			sib, ferr := h.repo.ListByCacheKeyNeedle(ctx, row.UserID, needle, anchor.Add(-7*24*time.Hour), anchor.Add(7*24*time.Hour), 500)
			if ferr == nil && len(sib) > 1 {
				events = repoRowsToServiceEvents(sib)
				historicalKey = pck
			}
		}
		if events == nil {
			// Single-turn fallback: no conversation key and no recoverable multi-turn history.
			events = []*service.PayloadAuditEvent{repoRowToServiceEvent(row)}
			singleTurn = true
		}
	} else {
		// Fetch ±7 days around createdAt (or around row.CreatedAt if createdAt is zero).
		anchor := createdAt
		if anchor.IsZero() {
			anchor = row.CreatedAt
		}
		from := anchor.Add(-7 * 24 * time.Hour)
		to := anchor.Add(7 * 24 * time.Hour)

		convRows, listErr := h.repo.ListConversation(ctx, convKey, from, to, 500)
		if listErr != nil {
			response.ErrorFrom(c, listErr)
			return
		}
		if len(convRows) == 0 {
			// Fallback to hit row only.
			convRows = []*repository.PayloadAuditRow{row}
		}
		events = repoRowsToServiceEvents(convRows)
	}

	transcript := service.AssembleTranscript(ctx, events, resolver)

	// Annotate the manifest depending on which path was taken.
	if historicalKey != "" {
		// Historical fallback recovered a multi-turn conversation via prompt_cache_key body match.
		if transcript.Manifest.ConversationKey == "" {
			transcript.Manifest.ConversationKey = historicalKey
		}
		transcript.Manifest.Gaps = append(transcript.Manifest.Gaps, "历史会话：按 prompt_cache_key 回溯分组（conversation_key 列为空）")
	} else if singleTurn {
		// No conversation key and no recoverable history — truly single-turn.
		transcript.Manifest.Gaps = append(transcript.Manifest.Gaps, "单轮副本（无会话键）")
	}

	html, err := service.RenderTranscriptHTML(transcript)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'")
	c.Header("Referrer-Policy", "no-referrer")
	c.Data(http.StatusOK, "text/html; charset=utf-8", html)
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/audit/exports/payloads/blobs/:sha
// ─────────────────────────────────────────────────────────────────────────────

// GetBlob proxies a stored blob by its SHA-256 hex digest via the BlobResolver.
// The resolver must be configured (offload enabled); otherwise 404.
func (h *AuditConversationHandler) GetBlob(c *gin.Context) {
	sha := strings.TrimSpace(c.Param("sha"))
	// Strict hex validation: the sha builds an object-store key, so anything
	// other than a real 64-hex sha256 (e.g. "../") must be rejected outright.
	if !service.IsHexSHA256(sha) {
		response.BadRequest(c, "Invalid sha: must be 64 lowercase hex digits")
		return
	}

	resolver := h.svc.Resolver()
	if resolver == nil {
		response.Error(c, http.StatusNotFound, "Blob store not configured")
		return
	}

	data, mime, err := resolver.FetchBlob(c.Request.Context(), sha)
	if err != nil {
		response.Error(c, http.StatusNotFound, "Blob not found")
		return
	}

	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Content-Disposition", "attachment")
	c.Data(http.StatusOK, mime, data)
}
