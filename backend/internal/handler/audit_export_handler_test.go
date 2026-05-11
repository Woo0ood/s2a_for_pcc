package handler

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Woo0ood/s2a_for_pcc/internal/repository" //nolint:depguard // export API uses repo filter/cursor types
	"github.com/Woo0ood/s2a_for_pcc/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

// --------------- mock repo ---------------

type mockExportRepo struct {
	listRows   []*repository.PayloadAuditRow
	listCursor *repository.PayloadAuditCursor
	listErr    error
	listCalls  int

	getRow *repository.PayloadAuditRow
	getErr error
}

func (m *mockExportRepo) List(_ context.Context, _ repository.PayloadAuditListFilter) ([]*repository.PayloadAuditRow, *repository.PayloadAuditCursor, error) {
	m.listCalls++
	return m.listRows, m.listCursor, m.listErr
}

func (m *mockExportRepo) Get(_ context.Context, _ int64, _ time.Time) (*repository.PayloadAuditRow, error) {
	return m.getRow, m.getErr
}

// --------------- noop ops logger ---------------

type noopOpsLogger struct{}

func (n *noopOpsLogger) Log(_, _ string, _ int, _ int64) {}

// --------------- helpers ---------------

func setupExportRouter(h *AuditExportHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Simulate auth middleware by setting context keys
	g := r.Group("/api/v1/audit", func(c *gin.Context) {
		c.Set(middleware.AuditExportKeyIDCtxKey, "test-key-id")
		c.Set(middleware.AuditExportKeyNameCtxKey, "test-key")
		c.Next()
	})
	g.POST("/auth/verify", h.VerifyAuth)
	g.GET("/exports/payloads", h.ListPayloads)
	g.GET("/exports/payloads/:id", h.GetPayload)
	g.GET("/exports/payloads.ndjson", h.StreamNDJSON)
	return r
}

func makeRows(n int) []*repository.PayloadAuditRow {
	rows := make([]*repository.PayloadAuditRow, n)
	base := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := range rows {
		rows[i] = &repository.PayloadAuditRow{
			ID: int64(i + 1),
			PayloadAuditEvent: repository.PayloadAuditEvent{
				RequestID:    fmt.Sprintf("req-%d", i+1),
				Endpoint:     "/v1/chat/completions",
				Provider:     "openai",
				Model:        "gpt-4",
				StatusCode:   200,
				InputExcerpt: fmt.Sprintf("input-excerpt-%d", i+1),
				OutputExcerpt: fmt.Sprintf("output-excerpt-%d", i+1),
				InputBody:    fmt.Sprintf("input-body-%d", i+1),
				OutputBody:   fmt.Sprintf("output-body-%d", i+1),
				CreatedAt:    base.Add(-time.Duration(i) * time.Minute),
			},
		}
	}
	return rows
}

// --------------- tests ---------------

func TestExport_VerifyAuth(t *testing.T) {
	repo := &mockExportRepo{}
	h := NewAuditExportHandler(repo, &noopOpsLogger{})
	r := setupExportRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/audit/auth/verify", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["key_name"] != "test-key" {
		t.Fatalf("expected key_name=test-key, got %v", body["key_name"])
	}
}

func TestExport_ListWithCursor(t *testing.T) {
	cursor := &repository.PayloadAuditCursor{
		SchemaVer:   1,
		ToEffective: time.Date(2025, 5, 2, 0, 0, 0, 0, time.UTC),
		LastCreated: time.Date(2025, 5, 1, 11, 58, 0, 0, time.UTC),
		LastID:      3,
	}
	repo := &mockExportRepo{
		listRows:   makeRows(3),
		listCursor: cursor,
	}
	h := NewAuditExportHandler(repo, &noopOpsLogger{})
	r := setupExportRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/audit/exports/payloads?from=2025-05-01&to=2025-05-02", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Data       []json.RawMessage `json:"data"`
		NextCursor string            `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(body.Data))
	}
	if body.NextCursor == "" {
		t.Fatal("expected non-empty next_cursor")
	}
}

func TestExport_ListIncludeBodyModes(t *testing.T) {
	tests := []struct {
		mode          string
		expectExcerpt bool
		expectBody    bool
	}{
		{mode: "none", expectExcerpt: false, expectBody: false},
		{mode: "excerpt", expectExcerpt: true, expectBody: false},
		{mode: "full", expectExcerpt: true, expectBody: true},
	}

	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			repo := &mockExportRepo{listRows: makeRows(1)}
			h := NewAuditExportHandler(repo, &noopOpsLogger{})
			r := setupExportRouter(h)

			w := httptest.NewRecorder()
			url := fmt.Sprintf("/api/v1/audit/exports/payloads?from=2025-05-01&to=2025-05-02&include_body=%s", tt.mode)
			req, _ := http.NewRequest("GET", url, nil)
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}

			var body struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Data) != 1 {
				t.Fatalf("expected 1 row, got %d", len(body.Data))
			}

			row := body.Data[0]

			// Check excerpt fields
			excerpt, _ := row["InputExcerpt"].(string)
			if tt.expectExcerpt && excerpt == "" {
				t.Error("expected non-empty InputExcerpt")
			}
			if !tt.expectExcerpt && excerpt != "" {
				t.Error("expected empty InputExcerpt")
			}

			// Check body fields
			bodyField, _ := row["InputBody"].(string)
			if tt.expectBody && bodyField == "" {
				t.Error("expected non-empty InputBody")
			}
			if !tt.expectBody && bodyField != "" {
				t.Error("expected empty InputBody")
			}
		})
	}
}

func TestExport_RejectsTimeWindowOver31d(t *testing.T) {
	repo := &mockExportRepo{}
	h := NewAuditExportHandler(repo, &noopOpsLogger{})
	r := setupExportRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/audit/exports/payloads?from=2025-01-01&to=2025-03-01", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExport_RejectsKeywordWithLargeWindow(t *testing.T) {
	repo := &mockExportRepo{}
	h := NewAuditExportHandler(repo, &noopOpsLogger{})
	r := setupExportRouter(h)

	w := httptest.NewRecorder()
	// 10 days with keyword
	req, _ := http.NewRequest("GET", "/api/v1/audit/exports/payloads?from=2025-05-01&to=2025-05-11&keyword=test&user_id=1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExport_RequiresFilterForLongWindow(t *testing.T) {
	repo := &mockExportRepo{}
	h := NewAuditExportHandler(repo, &noopOpsLogger{})
	r := setupExportRouter(h)

	// 8 days without user_id or group_id → 400
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/audit/exports/payloads?from=2025-05-01&to=2025-05-09", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for >7d without filter, got %d: %s", w.Code, w.Body.String())
	}

	// 8 days with user_id → 200
	repo.listRows = makeRows(0)
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/v1/audit/exports/payloads?from=2025-05-01&to=2025-05-09&user_id=1", nil)
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 for >7d with user_id, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestExport_GetById(t *testing.T) {
	rows := makeRows(1)
	repo := &mockExportRepo{getRow: rows[0]}
	h := NewAuditExportHandler(repo, &noopOpsLogger{})
	r := setupExportRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/audit/exports/payloads/1?created_at=2025-05-01T12:00:00Z", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data["RequestID"] != "req-1" {
		t.Fatalf("expected RequestID=req-1, got %v", body.Data["RequestID"])
	}

	// Test 404
	repo.getRow = nil
	repo.getErr = sql.ErrNoRows
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/v1/audit/exports/payloads/999", nil)
	r.ServeHTTP(w2, req2)

	// sql.ErrNoRows should result in an error via ErrorFrom
	if w2.Code == http.StatusOK {
		t.Fatalf("expected non-200 for missing row, got %d", w2.Code)
	}
}

func TestExport_NDJSONStreaming(t *testing.T) {
	rows := makeRows(3)
	callCount := 0
	repo := &mockExportRepo{}
	// First call returns rows with a cursor, second call returns empty (no more pages)
	originalList := repo.List
	_ = originalList
	cursor := &repository.PayloadAuditCursor{
		SchemaVer:   1,
		ToEffective: time.Date(2025, 5, 2, 0, 0, 0, 0, time.UTC),
		LastCreated: rows[2].CreatedAt,
		LastID:      rows[2].ID,
	}

	// Create a custom repo that returns data on first call, empty on second
	multiCallRepo := &multiPageMockRepo{
		pages: []mockPage{
			{rows: rows, cursor: cursor},
			{rows: nil, cursor: nil},
		},
	}

	h := NewAuditExportHandler(multiCallRepo, &noopOpsLogger{})
	r := setupExportRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/audit/exports/payloads.ndjson?from=2025-05-01&to=2025-05-02", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify Content-Type
	ct := w.Header().Get("Content-Type")
	if ct != "application/x-ndjson" {
		t.Fatalf("expected Content-Type application/x-ndjson, got %s", ct)
	}

	// Parse NDJSON lines
	scanner := bufio.NewScanner(w.Body)
	lineCount := 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("failed to parse NDJSON line %d: %v", lineCount, err)
		}
		lineCount++
	}
	if lineCount != 3 {
		t.Fatalf("expected 3 NDJSON lines, got %d", lineCount)
	}
	_ = callCount
}

func TestExport_NDJSONRejectsLargeWindow(t *testing.T) {
	repo := &mockExportRepo{}
	h := NewAuditExportHandler(repo, &noopOpsLogger{})
	r := setupExportRouter(h)

	// 8 days with user_id → still fails for NDJSON (max 7 days)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/audit/exports/payloads.ndjson?from=2025-05-01&to=2025-05-09&user_id=1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for NDJSON >7d, got %d: %s", w.Code, w.Body.String())
	}
}

// --------------- multi-page mock repo ---------------

type mockPage struct {
	rows   []*repository.PayloadAuditRow
	cursor *repository.PayloadAuditCursor
}

type multiPageMockRepo struct {
	pages []mockPage
	call  int
}

func (m *multiPageMockRepo) List(_ context.Context, _ repository.PayloadAuditListFilter) ([]*repository.PayloadAuditRow, *repository.PayloadAuditCursor, error) {
	if m.call >= len(m.pages) {
		return nil, nil, nil
	}
	page := m.pages[m.call]
	m.call++
	return page.rows, page.cursor, nil
}

func (m *multiPageMockRepo) Get(_ context.Context, _ int64, _ time.Time) (*repository.PayloadAuditRow, error) {
	return nil, sql.ErrNoRows
}
