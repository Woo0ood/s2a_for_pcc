package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// render performs the full streaming render for one export job:
//  1. Fetch the anchor row from ClickHouse.
//  2. Stream conversation rows to a temp file via TranscriptStreamer.
//  3. Upload the temp file to the result S3 bucket.
//  4. Mark the job done (or failed on any error).
//
// It is designed to run as a goroutine — it never returns an error to the caller;
// all outcomes are communicated via the JobManager.
func (s *Server) render(jobID string, id int64, createdAt time.Time) {
	// Panic recovery — translate panics to job failures so the process stays up.
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("panic: %v", r)
			slog.Error("export-worker render panic", "job_id", jobID, "err", msg)
			s.jobs.MarkFailed(jobID, msg)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.cfg.RenderTimeoutMinutes)*time.Minute)
	defer cancel()

	// ── 1. Fetch the anchor row ──────────────────────────────────────────────
	row, err := s.repo.Get(ctx, id, createdAt)
	if err != nil {
		s.jobs.MarkFailed(jobID, "Get row: "+err.Error())
		return
	}
	if row == nil {
		s.jobs.MarkFailed(jobID, "row not found")
		return
	}

	// ── 2. Open a temp file to receive the streamed HTML ─────────────────────
	tmpFile, err := os.CreateTemp("", "export-worker-"+jobID+"-*.html")
	if err != nil {
		s.jobs.MarkFailed(jobID, "create temp file: "+err.Error())
		return
	}
	tmpPath := tmpFile.Name()
	// Always clean up the temp file, regardless of outcome.
	defer func() { _ = os.Remove(tmpPath) }()

	// ── 3. Determine the conversation key to use in the manifest ─────────────
	convKeyForManifest := row.ConversationKey

	// ── 4. Build the TranscriptStreamer ──────────────────────────────────────
	streamer, err := service.NewTranscriptStreamer(tmpFile, convKeyForManifest)
	if err != nil {
		_ = tmpFile.Close()
		s.jobs.MarkFailed(jobID, "create streamer: "+err.Error())
		return
	}

	// ── 5. Render the conversation ───────────────────────────────────────────
	window := time.Duration(s.cfg.ConvWindowDays) * 24 * time.Hour

	convKey := row.ConversationKey
	if convKey != "" {
		// ── Happy path: conversation_key is populated ──────────────────────
		anchor := createdAt
		if anchor.IsZero() {
			anchor = row.CreatedAt
		}
		from := anchor.Add(-window)
		to := anchor.Add(window)

		// Pre-build the responseID set for chain-gap detection.
		_, _, _, responseIDs, metaErr := s.repo.ConversationMeta(ctx, convKey, from, to)
		if metaErr != nil {
			_ = tmpFile.Close()
			s.jobs.MarkFailed(jobID, "ConversationMeta: "+metaErr.Error())
			return
		}

		asm := service.NewStreamingAssembler(s.resolver, responseIDs, true /* unlimited inline */)

		streamErr := s.repo.StreamConversation(ctx, convKey, from, to, 0 /* no limit */, func(r *repository.PayloadAuditRow) error {
			evt := repoRowToEvent(r)
			turn, gaps := asm.Next(ctx, evt)
			streamer.AddGaps(gaps...)
			return streamer.WriteTurn(turn)
		})
		if streamErr != nil {
			_ = tmpFile.Close()
			s.jobs.MarkFailed(jobID, "StreamConversation: "+streamErr.Error())
			return
		}
	} else {
		// ── Historical fallback: conversation_key is empty ────────────────
		// Attempt recovery via prompt_cache_key body scan (±2d, bounded).
		anchor := createdAt
		if anchor.IsZero() {
			anchor = row.CreatedAt
		}

		var rows []*repository.PayloadAuditRow
		var historicalKey string
		fallbackBounded := false

		if pck, _ := service.ExtractRequestConvIDs(row.Endpoint, []byte(row.InputBody)); pck != "" {
			needle := `prompt_cache_key":"` + pck
			sib, ferr := s.repo.ListByCacheKeyNeedle(ctx, row.UserID, needle,
				anchor.Add(-2*24*time.Hour), anchor.Add(2*24*time.Hour), 500)
			if ferr != nil {
				// Scan exceeded its bound: degrade to single-turn.
				fallbackBounded = true
			} else if len(sib) > 1 {
				rows = sib
				historicalKey = pck
			}
		}

		if rows == nil {
			rows = []*repository.PayloadAuditRow{row}
		}

		// Build responseID set from rows (for chain-gap detection).
		responseIDs := make(map[string]bool, len(rows))
		for _, r := range rows {
			if r.ResponseID != "" {
				responseIDs[r.ResponseID] = true
			}
		}

		asm := service.NewStreamingAssembler(s.resolver, responseIDs, true /* unlimited inline */)
		for _, r := range rows {
			evt := repoRowToEvent(r)
			turn, gaps := asm.Next(ctx, evt)
			streamer.AddGaps(gaps...)
			if wErr := streamer.WriteTurn(turn); wErr != nil {
				_ = tmpFile.Close()
				s.jobs.MarkFailed(jobID, "WriteTurn: "+wErr.Error())
				return
			}
		}

		// Add gap annotation.
		if historicalKey != "" {
			streamer.AddGaps("历史会话：按 prompt_cache_key 回溯分组（conversation_key 列为空）")
		} else if fallbackBounded {
			streamer.AddGaps("历史回溯扫描超时/受限，仅导出本轮（部署 conversation_key 写入或回填历史后可恢复多轮）")
		} else {
			streamer.AddGaps("单轮副本（无会话键）")
		}
	}

	// ── 6. Finish the HTML document ──────────────────────────────────────────
	if err := streamer.Finish(); err != nil {
		_ = tmpFile.Close()
		s.jobs.MarkFailed(jobID, "Finish: "+err.Error())
		return
	}

	// Flush and seek back to 0 so PutObject reads from the beginning.
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		s.jobs.MarkFailed(jobID, "sync temp file: "+err.Error())
		return
	}
	if _, err := tmpFile.Seek(0, 0); err != nil {
		_ = tmpFile.Close()
		s.jobs.MarkFailed(jobID, "seek temp file: "+err.Error())
		return
	}

	// ── 7. Upload to result S3 ───────────────────────────────────────────────
	resultKey := s.cfg.ExportResultPrefix + jobID + ".html"
	contentType := "text/html; charset=utf-8"

	if err := s.resultStore.Upload(ctx, resultKey, tmpFile, contentType); err != nil {
		_ = tmpFile.Close()
		s.jobs.MarkFailed(jobID, "S3 upload: "+err.Error())
		return
	}
	_ = tmpFile.Close()

	// ── 8. Mark done ─────────────────────────────────────────────────────────
	s.jobs.MarkDone(jobID, resultKey)
	slog.Info("export-worker: job done", "job_id", jobID, "result_key", resultKey)
}

// repoRowToEvent converts a *repository.PayloadAuditRow to a *service.PayloadAuditEvent.
// This is a field-for-field copy mirroring audit_conversation_handler.go's repoRowToServiceEvent.
func repoRowToEvent(row *repository.PayloadAuditRow) *service.PayloadAuditEvent {
	e := row.PayloadAuditEvent
	return &service.PayloadAuditEvent{
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
}
