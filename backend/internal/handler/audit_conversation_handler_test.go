package handler

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock repo
// ─────────────────────────────────────────────────────────────────────────────

type mockConvRepo struct {
	getRow         *repository.PayloadAuditRow
	getErr         error
	listRows        []*repository.PayloadAuditRow
	listErr         error
	needleRows      []*repository.PayloadAuditRow
	needleErr       error
}

func (m *mockConvRepo) Get(_ context.Context, _ int64, _ time.Time) (*repository.PayloadAuditRow, error) {
	return m.getRow, m.getErr
}

func (m *mockConvRepo) ListConversation(_ context.Context, _ string, _, _ time.Time, _ int) ([]*repository.PayloadAuditRow, error) {
	return m.listRows, m.listErr
}

func (m *mockConvRepo) ListByCacheKeyNeedle(_ context.Context, _ *int64, _ string, _, _ time.Time, _ int) ([]*repository.PayloadAuditRow, error) {
	return m.needleRows, m.needleErr
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func setupConvRouter(h *AuditConversationHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1/audit", func(c *gin.Context) {
		c.Set(middleware.AuditExportKeyIDCtxKey, "test-key-id")
		c.Set(middleware.AuditExportKeyNameCtxKey, "test-key")
		c.Next()
	})
	// Register blob proxy before :id to avoid param capture.
	g.GET("/exports/payloads/blobs/:sha", h.GetBlob)
	g.GET("/exports/payloads/:id/conversation", h.GetConversation)
	return r
}

func makeConvRow(id int64, convKey string, createdAt time.Time) *repository.PayloadAuditRow {
	return &repository.PayloadAuditRow{
		ID: id,
		PayloadAuditEvent: repository.PayloadAuditEvent{
			RequestID:       fmt.Sprintf("req-%d", id),
			Endpoint:        "/v1/chat/completions",
			Provider:        "openai",
			Model:           "gpt-4o",
			StatusCode:      200,
			InputBody:       `{"messages":[{"role":"user","content":"hello"}]}`,
			OutputBody:      `{"choices":[{"message":{"role":"assistant","content":"Hi!"}}]}`,
			OutputFormat:    "json",
			ConversationKey: convKey,
			CreatedAt:       createdAt,
		},
	}
}

// newMinimalSvc returns a PayloadAuditService backed by an in-memory settings repo.
// The resolver will be nil (no offload configured) which is fine for unit tests.
func newMinimalSvc(t *testing.T) *service.PayloadAuditService {
	t.Helper()
	repo := newMockConvSettingsRepo()
	svc, err := service.ProvidePayloadAuditService(repo, nil, 0, nil, nil, nil)
	require.NoError(t, err)
	return svc
}

// ─────────────────────────────────────────────────────────────────────────────
// In-memory settings repo (mirrors audit_export_auth_test helper)
// ─────────────────────────────────────────────────────────────────────────────

type mockConvSettingsRepo struct {
	data map[string]string
}

func newMockConvSettingsRepo() *mockConvSettingsRepo {
	return &mockConvSettingsRepo{data: make(map[string]string)}
}

func (m *mockConvSettingsRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	v, ok := m.data[key]
	if !ok {
		return nil, service.ErrSettingNotFound
	}
	return &service.Setting{Key: key, Value: v, UpdatedAt: time.Now()}, nil
}

func (m *mockConvSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	v, ok := m.data[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return v, nil
}

func (m *mockConvSettingsRepo) Set(_ context.Context, key, value string) error {
	m.data[key] = value
	return nil
}

func (m *mockConvSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := m.data[k]; ok {
			result[k] = v
		}
	}
	return result, nil
}

func (m *mockConvSettingsRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	for k, v := range settings {
		m.data[k] = v
	}
	return nil
}

func (m *mockConvSettingsRepo) GetAll(_ context.Context) (map[string]string, error) {
	result := make(map[string]string, len(m.data))
	for k, v := range m.data {
		result[k] = v
	}
	return result, nil
}

func (m *mockConvSettingsRepo) Delete(_ context.Context, key string) error {
	delete(m.data, key)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests — GetConversation
// ─────────────────────────────────────────────────────────────────────────────

func TestGetConversation_WithConversationKey_200HTML(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	hitRow := makeConvRow(42, "conv-abc", now)
	row2 := makeConvRow(43, "conv-abc", now.Add(time.Minute))

	repo := &mockConvRepo{
		getRow:  hitRow,
		listRows: []*repository.PayloadAuditRow{hitRow, row2},
	}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	url := fmt.Sprintf("/api/v1/audit/exports/payloads/42/conversation?created_at=%d&format=html",
		now.UnixMilli())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	body := w.Body.String()
	// Should have 2 turns.
	assert.Contains(t, body, "Turn 1")
	assert.Contains(t, body, "Turn 2")
	// Manifest section present.
	assert.Contains(t, body, "Manifest")
	// Security headers.
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"))
	csp := w.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'none'")
	assert.Contains(t, csp, "data:", "CSP must allow data: URIs for inlined image blobs")
}

func TestGetConversation_EmptyConversationKey_SingleTurnFallback(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	hitRow := makeConvRow(10, "", now) // no conversation key, no prompt_cache_key in body

	repo := &mockConvRepo{getRow: hitRow}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	url := "/api/v1/audit/exports/payloads/10/conversation?format=html&created_at=" + strconv.FormatInt(now.UnixMilli(), 10)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// Single-turn manifest note must appear.
	assert.Contains(t, body, "单轮副本")
	// Still exactly one turn.
	assert.Contains(t, body, "Turn 1")
}

// makeResponsesRow creates a row for the /v1/responses endpoint with a prompt_cache_key in the body.
func makeResponsesRow(id int64, pck string, createdAt time.Time) *repository.PayloadAuditRow {
	uid := int64(555)
	return &repository.PayloadAuditRow{
		ID: id,
		PayloadAuditEvent: repository.PayloadAuditEvent{
			RequestID:       fmt.Sprintf("req-%d", id),
			Endpoint:        "/v1/responses",
			Provider:        "openai",
			Model:           "gpt-5.4",
			StatusCode:      200,
			UserID:          &uid,
			InputBody:       fmt.Sprintf(`{"model":"gpt-5.4","prompt_cache_key":%q,"input":[{"type":"text","text":"hello"}]}`, pck),
			OutputBody:      `{"id":"resp_001","output":[{"type":"message","content":[{"type":"output_text","text":"Hi"}]}]}`,
			OutputFormat:    "json",
			ConversationKey: "", // historical: column empty
			CreatedAt:       createdAt,
		},
	}
}

func TestGetConversation_HistoricalFallback_MultiTurn(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	pck := "hist-7"

	hitRow := makeResponsesRow(20, pck, now)
	sib1 := makeResponsesRow(21, pck, now.Add(time.Minute))

	// needleRows: 2 siblings (>1) triggers the historical fallback path.
	repo := &mockConvRepo{
		getRow:     hitRow,
		needleRows: []*repository.PayloadAuditRow{hitRow, sib1},
	}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	url := fmt.Sprintf("/api/v1/audit/exports/payloads/20/conversation?format=html&created_at=%d", now.UnixMilli())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()

	// Must have >1 turn.
	assert.Contains(t, body, "Turn 1")
	assert.Contains(t, body, "Turn 2")

	// Historical fallback manifest note must appear.
	assert.Contains(t, body, "历史会话")
	assert.Contains(t, body, "prompt_cache_key")

	// Single-turn note must NOT appear.
	assert.NotContains(t, body, "单轮副本")
}

func TestGetConversation_HistoricalFallback_OnlySingleSibling_FallsBackToSingleTurn(t *testing.T) {
	// When ListByCacheKeyNeedle returns only 1 row (<=1), we must degrade to single-turn.
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	pck := "hist-8"

	hitRow := makeResponsesRow(30, pck, now)

	repo := &mockConvRepo{
		getRow:     hitRow,
		needleRows: []*repository.PayloadAuditRow{hitRow}, // only 1 — not multi-turn
	}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	url := fmt.Sprintf("/api/v1/audit/exports/payloads/30/conversation?format=html&created_at=%d", now.UnixMilli())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()

	// Should degrade to single-turn.
	assert.Contains(t, body, "单轮副本")
	assert.Contains(t, body, "Turn 1")

	// Historical fallback manifest note must NOT appear.
	assert.NotContains(t, body, "历史会话")
}

func TestGetConversation_HistoricalFallback_NoPCKInBody_FallsBackToSingleTurn(t *testing.T) {
	// When the body has no prompt_cache_key (e.g. chat endpoint with empty convKey),
	// we must degrade straight to single-turn without calling ListByCacheKeyNeedle.
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	hitRow := makeConvRow(40, "", now) // /v1/chat/completions, no pck

	repo := &mockConvRepo{
		getRow: hitRow,
		// needleRows left nil — should not be called
	}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	url := fmt.Sprintf("/api/v1/audit/exports/payloads/40/conversation?format=html&created_at=%d", now.UnixMilli())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "单轮副本")
	assert.NotContains(t, body, "历史会话")
}

func TestGetConversation_BadID_400(t *testing.T) {
	repo := &mockConvRepo{}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/audit/exports/payloads/notanid/conversation", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetConversation_BadFormat_400(t *testing.T) {
	repo := &mockConvRepo{}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/audit/exports/payloads/42/conversation?format=json", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetConversation_BadCreatedAt_400(t *testing.T) {
	repo := &mockConvRepo{}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/audit/exports/payloads/42/conversation?created_at=notadate&format=html", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetConversation_RowNotFound_404(t *testing.T) {
	repo := &mockConvRepo{getErr: sql.ErrNoRows}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/audit/exports/payloads/99/conversation?format=html&created_at=1700000000000", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetConversation_CreatedAtRFC3339_200(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	hitRow := makeConvRow(7, "conv-xyz", now)

	repo := &mockConvRepo{
		getRow:  hitRow,
		listRows: []*repository.PayloadAuditRow{hitRow},
	}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	// RFC3339 format.
	url := "/api/v1/audit/exports/payloads/7/conversation?format=html&created_at=" + now.Format(time.RFC3339)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests — GetBlob
// ─────────────────────────────────────────────────────────────────────────────

func TestGetBlob_NoResolver_404(t *testing.T) {
	repo := &mockConvRepo{}
	svc := newMinimalSvc(t) // resolver is nil (no offload configured)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/audit/exports/payloads/blobs/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetBlob_MissingSha_400(t *testing.T) {
	// This shouldn't be reachable via the gin route (gin requires :sha to be non-empty)
	// but test the explicit empty-sha guard via a direct call.
	repo := &mockConvRepo{}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/blobs/", nil)
	// Gin param "sha" not set → TrimSpace returns ""
	// Manually invoke to test the guard.
	h.GetBlob(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetBlob_InvalidSha_400(t *testing.T) {
	repo := &mockConvRepo{}
	svc := newMinimalSvc(t)
	h := NewAuditConversationHandler(repo, svc)
	r := setupConvRouter(h)
	// Non-hex / wrong-length shas must be rejected before any object-store
	// lookup — guards against path-traversal / arbitrary-object reads.
	for _, bad := range []string{"abc123", "NOTHEXVALUE", "zzzz"} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/audit/exports/payloads/blobs/"+bad, nil)
		r.ServeHTTP(w, req)
		assert.Equalf(t, http.StatusBadRequest, w.Code, "sha %q must be 400", bad)
	}
}
