package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ─── config tests ────────────────────────────────────────────────────────────

func TestParseConfig_MissingRequired(t *testing.T) {
	// Clear any existing values for the required vars.
	required := map[string]string{
		"EXPORT_WORKER_TOKEN":      "",
		"CH_DSN":                   "",
		"BLOB_S3_BUCKET":           "",
		"BLOB_S3_SECRET_ACCESS_KEY": "",
	}
	for k := range required {
		t.Setenv(k, "")
	}

	tests := []struct {
		name    string
		unset   []string // vars to leave empty
		wantErr bool
	}{
		{
			name:    "all missing",
			unset:   []string{"EXPORT_WORKER_TOKEN", "CH_DSN", "BLOB_S3_BUCKET", "BLOB_S3_SECRET_ACCESS_KEY"},
			wantErr: true,
		},
		{
			name:    "missing token",
			unset:   []string{"EXPORT_WORKER_TOKEN"},
			wantErr: true,
		},
		{
			name:    "missing CH_DSN",
			unset:   []string{"CH_DSN"},
			wantErr: true,
		},
		{
			name:    "missing BLOB_S3_BUCKET",
			unset:   []string{"BLOB_S3_BUCKET"},
			wantErr: true,
		},
		{
			name:    "missing BLOB_S3_SECRET_ACCESS_KEY",
			unset:   []string{"BLOB_S3_SECRET_ACCESS_KEY"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set all required to valid values, then blank out the ones in tt.unset.
			t.Setenv("EXPORT_WORKER_TOKEN", "tok")
			t.Setenv("CH_DSN", "clickhouse://u:p@host:9000/db")
			t.Setenv("BLOB_S3_BUCKET", "mybucket")
			t.Setenv("BLOB_S3_SECRET_ACCESS_KEY", "secret")
			for _, k := range tt.unset {
				t.Setenv(k, "")
			}
			_, err := parseConfig()
			if (err != nil) != tt.wantErr {
				t.Errorf("parseConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseConfig_Defaults(t *testing.T) {
	t.Setenv("EXPORT_WORKER_TOKEN", "tok")
	t.Setenv("CH_DSN", "clickhouse://u:p@host:9000/db")
	t.Setenv("BLOB_S3_BUCKET", "mybucket")
	t.Setenv("BLOB_S3_SECRET_ACCESS_KEY", "secret")
	// Remove optional vars.
	for _, k := range []string{
		"EXPORT_WORKER_LISTEN", "CH_TABLE", "BLOB_S3_PREFIX",
		"EXPORT_RESULT_PREFIX", "RENDER_TIMEOUT_MINUTES", "CONV_WINDOW_DAYS",
		"BLOB_S3_FORCE_PATH_STYLE",
	} {
		t.Setenv(k, "")
	}

	cfg, err := parseConfig()
	if err != nil {
		t.Fatalf("parseConfig() unexpected error: %v", err)
	}
	if cfg.Listen != ":8088" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":8088")
	}
	if cfg.CHTable != "payload_audit_logs" {
		t.Errorf("CHTable = %q, want %q", cfg.CHTable, "payload_audit_logs")
	}
	if cfg.BlobS3Prefix != "payload-audit/" {
		t.Errorf("BlobS3Prefix = %q, want %q", cfg.BlobS3Prefix, "payload-audit/")
	}
	if cfg.ExportResultPrefix != "payload-audit/exports/" {
		t.Errorf("ExportResultPrefix = %q, want %q", cfg.ExportResultPrefix, "payload-audit/exports/")
	}
	if cfg.RenderTimeoutMinutes != 10 {
		t.Errorf("RenderTimeoutMinutes = %d, want 10", cfg.RenderTimeoutMinutes)
	}
	if cfg.ConvWindowDays != 7 {
		t.Errorf("ConvWindowDays = %d, want 7", cfg.ConvWindowDays)
	}
	if cfg.BlobS3ForcePathStyle != false {
		t.Errorf("BlobS3ForcePathStyle = %v, want false", cfg.BlobS3ForcePathStyle)
	}
}

func TestParseConfig_BadInteger(t *testing.T) {
	t.Setenv("EXPORT_WORKER_TOKEN", "tok")
	t.Setenv("CH_DSN", "clickhouse://u:p@host:9000/db")
	t.Setenv("BLOB_S3_BUCKET", "mybucket")
	t.Setenv("BLOB_S3_SECRET_ACCESS_KEY", "secret")
	t.Setenv("RENDER_TIMEOUT_MINUTES", "notanint")

	_, err := parseConfig()
	if err == nil {
		t.Error("expected error for invalid RENDER_TIMEOUT_MINUTES, got nil")
	}
}

func TestParseConfig_BadBool(t *testing.T) {
	t.Setenv("EXPORT_WORKER_TOKEN", "tok")
	t.Setenv("CH_DSN", "clickhouse://u:p@host:9000/db")
	t.Setenv("BLOB_S3_BUCKET", "mybucket")
	t.Setenv("BLOB_S3_SECRET_ACCESS_KEY", "secret")
	t.Setenv("BLOB_S3_FORCE_PATH_STYLE", "notabool")

	_, err := parseConfig()
	if err == nil {
		t.Error("expected error for invalid BLOB_S3_FORCE_PATH_STYLE, got nil")
	}
}

// ─── token middleware tests ──────────────────────────────────────────────────

// minimalServer builds a Server with only the auth middleware and a trivial handler
// under an authenticated route — no CH / S3 connections needed.
func minimalServer(token string) *Server {
	cfg := &Config{Token: token, Listen: ":0"}
	jobs := NewJobManager(1 * time.Hour)
	s := &Server{
		cfg:  cfg,
		jobs: jobs,
		mux:  http.NewServeMux(),
	}
	// Register just the health check + a protected dummy route.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /v1/export/{job_id}", s.auth(s.handleGetExport))
	return s
}

func TestTokenMiddleware_NoAuth(t *testing.T) {
	srv := minimalServer("secret-token")
	req := httptest.NewRequest("GET", "/v1/export/abcdef1234", nil)
	rw := httptest.NewRecorder()
	srv.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rw.Code)
	}
}

func TestTokenMiddleware_WrongToken(t *testing.T) {
	srv := minimalServer("secret-token")
	req := httptest.NewRequest("GET", "/v1/export/abcdef1234", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rw := httptest.NewRecorder()
	srv.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rw.Code)
	}
}

func TestTokenMiddleware_CorrectToken_NotFound(t *testing.T) {
	srv := minimalServer("secret-token")
	req := httptest.NewRequest("GET", "/v1/export/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rw := httptest.NewRecorder()
	srv.ServeHTTP(rw, req)
	// Job doesn't exist → 404, but the middleware passed.
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404 (job not found), got %d", rw.Code)
	}
}

func TestHealthzNoAuth(t *testing.T) {
	srv := minimalServer("secret-token")
	req := httptest.NewRequest("GET", "/healthz", nil)
	rw := httptest.NewRecorder()
	srv.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rw.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rw.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
}

// ─── job manager tests ───────────────────────────────────────────────────────

func TestJobManager_CreateRunningMarkDone(t *testing.T) {
	m := NewJobManager(1 * time.Hour)

	id, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty id")
	}

	job := m.Get(id)
	if job == nil {
		t.Fatal("Get returned nil after Create")
	}
	if job.Status != JobStatusRunning {
		t.Errorf("status = %q, want %q", job.Status, JobStatusRunning)
	}
	if !job.Created.After(time.Time{}) {
		t.Error("Created is zero")
	}

	m.MarkDone(id, "payload-audit/exports/"+id+".html")
	job = m.Get(id)
	if job.Status != JobStatusDone {
		t.Errorf("status after MarkDone = %q, want %q", job.Status, JobStatusDone)
	}
	if !strings.Contains(job.ResultKey, id) {
		t.Errorf("ResultKey = %q, want to contain job id", job.ResultKey)
	}
}

func TestJobManager_MarkFailed(t *testing.T) {
	m := NewJobManager(1 * time.Hour)
	id, _ := m.Create()
	m.MarkFailed(id, "something went wrong")
	job := m.Get(id)
	if job.Status != JobStatusFailed {
		t.Errorf("status = %q, want %q", job.Status, JobStatusFailed)
	}
	if job.Err != "something went wrong" {
		t.Errorf("Err = %q, want %q", job.Err, "something went wrong")
	}
}

func TestJobManager_GetNonExistent(t *testing.T) {
	m := NewJobManager(1 * time.Hour)
	if job := m.Get("doesnotexist"); job != nil {
		t.Errorf("expected nil for unknown id, got %+v", job)
	}
}

func TestJobManager_SweepDropsOldJobs(t *testing.T) {
	// Use a very short TTL so we can verify sweep without waiting.
	m := &JobManager{
		jobs:     make(map[string]*Job),
		sweepTTL: 0, // zero TTL — all jobs immediately expire
	}
	id, _ := m.Create()
	// Manually backdate the Created time so it's expired.
	m.mu.Lock()
	m.jobs[id].Created = time.Now().Add(-2 * time.Hour)
	m.mu.Unlock()

	// Run the sweep logic directly (without the goroutine).
	cutoff := time.Now().Add(-m.sweepTTL)
	m.mu.Lock()
	for jid, j := range m.jobs {
		if j.Created.Before(cutoff) {
			delete(m.jobs, jid)
		}
	}
	m.mu.Unlock()

	if job := m.Get(id); job != nil {
		t.Errorf("expected job to be swept, but still present: %+v", job)
	}
}

func TestJobManager_UniqueIDs(t *testing.T) {
	m := NewJobManager(1 * time.Hour)
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := m.Create()
		if err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q at iteration %d", id, i)
		}
		seen[id] = true
	}
}

// ─── parseID / parseExportTime tests ─────────────────────────────────────────

func TestParseID(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{`12345`, 12345, false},
		{`"12345"`, 12345, false},
		{`"abc"`, 0, true},
		{`null`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseID(json.RawMessage(tt.input))
			if (err != nil) != tt.wantErr {
				t.Errorf("parseID(%s) err = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseID(%s) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseExportTime(t *testing.T) {
	tests := []struct {
		input    string
		wantZero bool
		wantErr  bool
	}{
		{`1700000000000`, false, false},     // unix-ms
		{`2023-11-14T22:00:00Z`, false, false}, // RFC3339
		{`2023-11-14T22:00:00.123456789Z`, false, false}, // RFC3339Nano
		{``, true, false},                   // empty → zero time, no error
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.input), func(t *testing.T) {
			got, err := parseExportTime(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseExportTime(%q) err = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && tt.wantZero != got.IsZero() {
				t.Errorf("parseExportTime(%q).IsZero() = %v, want %v", tt.input, got.IsZero(), tt.wantZero)
			}
		})
	}
}

// ─── POST /v1/export request parsing tests ──────────────────────────────────

// stubServer builds a minimal server with a stub render func that immediately
// marks the job done — avoids needing real CH/S3.
type stubRenderServer struct {
	*Server
}

func newStubServer(token string) *stubRenderServer {
	cfg := &Config{Token: token, Listen: ":0"}
	jobs := NewJobManager(1 * time.Hour)
	s := &Server{
		cfg:  cfg,
		jobs: jobs,
		mux:  http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	ss := &stubRenderServer{s}
	// Register start-export with a stub handler that calls the stub render.
	s.mux.HandleFunc("POST /v1/export", s.auth(ss.handleStartExportStub))
	s.mux.HandleFunc("GET /v1/export/{job_id}", s.auth(s.handleGetExport))
	return ss
}

// handleStartExportStub is identical to handleStartExport but spawns stubRender.
func (ss *stubRenderServer) handleStartExportStub(w http.ResponseWriter, r *http.Request) {
	var req startExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if _, err := parseID(req.ID); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id: "+err.Error())
		return
	}
	if _, err := parseExportTime(req.CreatedAt); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid created_at: "+err.Error())
		return
	}
	jobID, err := ss.jobs.Create()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "create job: "+err.Error())
		return
	}
	// Immediately mark done — no real render.
	go func() { ss.jobs.MarkDone(jobID, "payload-audit/exports/"+jobID+".html") }()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

func TestHandleStartExport_ValidRequest(t *testing.T) {
	ss := newStubServer("tok")
	body := `{"id": 12345, "created_at": "2023-11-14T22:00:00Z"}`
	req := httptest.NewRequest("POST", "/v1/export", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	ss.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rw.Code, rw.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["job_id"] == "" {
		t.Error("expected non-empty job_id in response")
	}
}

func TestHandleStartExport_BadJSON(t *testing.T) {
	ss := newStubServer("tok")
	req := httptest.NewRequest("POST", "/v1/export", bytes.NewBufferString(`not-json`))
	req.Header.Set("Authorization", "Bearer tok")
	rw := httptest.NewRecorder()
	ss.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rw.Code)
	}
}

// ─── env helper tests ────────────────────────────────────────────────────────

func TestEnvOrDefault(t *testing.T) {
	os.Unsetenv("TEST_MISSING_VAR_XYZ")
	if v := envOrDefault("TEST_MISSING_VAR_XYZ", "default"); v != "default" {
		t.Errorf("got %q, want %q", v, "default")
	}
	t.Setenv("TEST_MISSING_VAR_XYZ", "set")
	if v := envOrDefault("TEST_MISSING_VAR_XYZ", "default"); v != "set" {
		t.Errorf("got %q, want %q", v, "set")
	}
}
