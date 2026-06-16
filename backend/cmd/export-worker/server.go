package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const conversationCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:"

// Server holds all dependencies for the export-worker HTTP server.
type Server struct {
	cfg         *Config
	repo        *repository.PayloadAuditCHRepo
	resolver    *service.BlobResolver
	resultStore *resultS3Store
	jobs        *JobManager
	mux         *http.ServeMux
}

// NewServer wires the HTTP mux and returns a ready Server.
func NewServer(
	cfg *Config,
	repo *repository.PayloadAuditCHRepo,
	resolver *service.BlobResolver,
	resultStore *resultS3Store,
	jobs *JobManager,
) *Server {
	s := &Server{
		cfg:         cfg,
		repo:        repo,
		resolver:    resolver,
		resultStore: resultStore,
		jobs:        jobs,
		mux:         http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// registerRoutes registers all HTTP routes using Go 1.22 method+pattern syntax.
func (s *Server) registerRoutes() {
	// Public health check — no auth.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Authenticated routes.
	s.mux.HandleFunc("POST /v1/export", s.auth(s.handleStartExport))
	s.mux.HandleFunc("GET /v1/export/{job_id}", s.auth(s.handleGetExport))
	s.mux.HandleFunc("GET /v1/export/{job_id}/result", s.auth(s.handleGetExportResult))
}

// auth wraps a handler with Bearer token authentication.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) != 1 {
			jsonError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// ─── handlers ───────────────────────────────────────────────────────────────

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

// startExportRequest is the POST /v1/export request body.
type startExportRequest struct {
	ID        json.RawMessage `json:"id"`
	CreatedAt string          `json:"created_at"`
}

func (s *Server) handleStartExport(w http.ResponseWriter, r *http.Request) {
	var req startExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Parse id — accept int64 or quoted string.
	id, err := parseID(req.ID)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id: "+err.Error())
		return
	}

	// Parse created_at — accept unix-ms integer string or RFC3339.
	createdAt, err := parseExportTime(req.CreatedAt)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid created_at: "+err.Error())
		return
	}

	jobID, err := s.jobs.Create()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "create job: "+err.Error())
		return
	}

	// Capture for goroutine — detached from request lifecycle.
	capturedID := id
	capturedCreatedAt := createdAt
	capturedJobID := jobID
	go s.render(capturedJobID, capturedID, capturedCreatedAt)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

func (s *Server) handleGetExport(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job := s.jobs.Get(jobID)
	if job == nil {
		jsonError(w, http.StatusNotFound, "job not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":     job.Status,
		"error":      job.Err,
		"result_key": job.ResultKey,
	})
}

func (s *Server) handleGetExportResult(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job := s.jobs.Get(jobID)
	if job == nil {
		jsonError(w, http.StatusNotFound, "job not found")
		return
	}
	switch job.Status {
	case JobStatusRunning:
		jsonError(w, http.StatusConflict, "export still running")
		return
	case JobStatusFailed:
		jsonError(w, http.StatusInternalServerError, "export failed: "+job.Err)
		return
	}
	// Job is done — stream the S3 result.
	rc, err := s.resultStore.Download(r.Context(), job.ResultKey)
	if err != nil {
		slog.Error("export-worker: download result", "job_id", jobID, "key", job.ResultKey, "err", err)
		jsonError(w, http.StatusNotFound, "result not found")
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", conversationCSP)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// parseID accepts either a bare JSON number or a quoted string containing a decimal int64.
func parseID(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	// Reject JSON null explicitly.
	if string(raw) == "null" {
		return 0, fmt.Errorf("id must not be null")
	}
	// Try number first.
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	// Try quoted string.
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseExportTime parses a created_at value that may be:
//   - a unix-millisecond integer (as string or bare number)
//   - RFC3339 / RFC3339Nano
func parseExportTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	// Unix-ms integer.
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC(), nil
	}
	// RFC3339 variants.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, io.ErrUnexpectedEOF
}

// parseExportTimeErr wraps parseExportTime for use in context where an error
// should be returned when the parsed time is zero.
func parseExportTimeErr(v string) (time.Time, error) {
	t, err := parseExportTime(v)
	if err != nil {
		return time.Time{}, err
	}
	if t.IsZero() {
		return time.Time{}, &invalidTimeError{v}
	}
	return t, nil
}

type invalidTimeError struct{ v string }

func (e *invalidTimeError) Error() string {
	return "cannot parse time: " + e.v
}

// Ensure unused import doesn't block compilation — parseExportTimeErr is only
// referenced when called from tests or from handlers that need strict validation.
var _ = parseExportTimeErr
