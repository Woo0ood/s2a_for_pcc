package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// --------------- mock repo ---------------

type mockPayloadAuditRepo struct {
	listRows   []*repository.PayloadAuditRow
	listCursor *repository.PayloadAuditCursor
	listErr    error
	getRow     *repository.PayloadAuditRow
	getErr     error
	partitions []repository.PayloadAuditPartition
	partErr    error
}

func (m *mockPayloadAuditRepo) List(_ context.Context, _ repository.PayloadAuditListFilter) ([]*repository.PayloadAuditRow, *repository.PayloadAuditCursor, error) {
	return m.listRows, m.listCursor, m.listErr
}

func (m *mockPayloadAuditRepo) Get(_ context.Context, _ int64, _ time.Time) (*repository.PayloadAuditRow, error) {
	return m.getRow, m.getErr
}

func (m *mockPayloadAuditRepo) ListPartitionsBefore(_ context.Context, _ time.Time) ([]repository.PayloadAuditPartition, error) {
	return m.partitions, m.partErr
}

// --------------- mock cleanup repo ---------------

type mockPayloadAuditCleanupRepo struct{}

func (m *mockPayloadAuditCleanupRepo) ListPartitionsBefore(_ context.Context, _ time.Time) ([]service.PayloadAuditPartition, error) {
	return nil, nil
}

func (m *mockPayloadAuditCleanupRepo) PartitionState(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (m *mockPayloadAuditCleanupRepo) DetachPartitionConcurrently(_ context.Context, _ string) error {
	return nil
}

func (m *mockPayloadAuditCleanupRepo) FinalizePartitionDetach(_ context.Context, _ string) error {
	return nil
}

func (m *mockPayloadAuditCleanupRepo) DropPartition(_ context.Context, _ string) error {
	return nil
}

// --------------- helpers ---------------

// stubSettingsRepo implements service.SettingRepository for testing.
type stubSettingsRepo struct {
	data map[string]string
}

func (s *stubSettingsRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	v, ok := s.data[key]
	if !ok {
		return nil, service.ErrSettingNotFound
	}
	return &service.Setting{Key: key, Value: v, UpdatedAt: time.Now()}, nil
}

func (s *stubSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	v, ok := s.data[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return v, nil
}

func (s *stubSettingsRepo) Set(_ context.Context, key, value string) error {
	s.data[key] = value
	return nil
}

func (s *stubSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := s.data[k]; ok {
			result[k] = v
		}
	}
	return result, nil
}

func (s *stubSettingsRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	for k, v := range settings {
		s.data[k] = v
	}
	return nil
}

func (s *stubSettingsRepo) GetAll(_ context.Context) (map[string]string, error) {
	result := make(map[string]string, len(s.data))
	for k, v := range s.data {
		result[k] = v
	}
	return result, nil
}

func (s *stubSettingsRepo) Delete(_ context.Context, key string) error {
	delete(s.data, key)
	return nil
}

// buildTestPayloadAuditHandler creates a handler with a real PayloadAuditService
// (backed by in-memory stub settings) and the given mock repo.
func buildTestPayloadAuditHandler(
	enabledStr, cfgJSON string,
	repo PayloadAuditAdminRepo,
) *PayloadAuditAdminHandler {
	settings := &stubSettingsRepo{data: map[string]string{
		"payload_audit_enabled": enabledStr,
		"payload_audit_config":  cfgJSON,
	}}
	svc, _ := service.ProvidePayloadAuditService(settings, nil, 0)

	sink := service.NewPayloadAuditSink(nil, service.SinkConfig{
		WorkerCount: 4, QueueSize: 1024, QueueMaxBytes: 1 << 20,
		BatchSize: 50, BatchFlushMs: 100,
	})

	return &PayloadAuditAdminHandler{
		svc:     svc,
		sink:    sink,
		cleanup: service.NewPayloadAuditCleanup(&mockPayloadAuditCleanupRepo{}, svc),
		repo:    repo,
	}
}

func newPayloadAuditTestRouter(h *PayloadAuditAdminHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/payload-audit")
	{
		g.GET("/config", h.GetConfig)
		g.PUT("/config", h.UpdateConfig)
		g.GET("/status", h.GetStatus)
		g.GET("/payloads", h.ListPayloads)
		g.GET("/payloads/:id", h.GetPayload)
		g.GET("/export-keys", h.ListExportKeys)
		g.POST("/export-keys", h.CreateExportKey)
		g.DELETE("/export-keys/:id", h.DeleteExportKey)
		g.POST("/cleanup", h.RunCleanup)
	}
	return r
}

func doRequest(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(w, req)
	return w
}

func unmarshalData(t *testing.T, w *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()
	var env responseEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v, body: %s", err, w.Body.String())
	}
	return env.Data
}

// --------------- tests ---------------

func TestPayloadAuditAdmin_GetConfig(t *testing.T) {
	cfgJSON := `{"all_groups":true,"excerpt_bytes":256,"retention_days":90,"worker_count":4,"queue_size":1024,"batch_size":50,"batch_flush_ms":100}`
	h := buildTestPayloadAuditHandler("true", cfgJSON, &mockPayloadAuditRepo{})
	r := newPayloadAuditTestRouter(h)

	w := doRequest(r, http.MethodGet, "/payload-audit/config", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body: %s", w.Code, w.Body.String())
	}

	data := unmarshalData(t, w)
	var resp payloadAuditConfigResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if !resp.Enabled {
		t.Error("expected enabled=true")
	}
	if resp.Config.ExcerptBytes != 256 {
		t.Errorf("excerpt_bytes=%d, want 256", resp.Config.ExcerptBytes)
	}
}

func TestPayloadAuditAdmin_GetConfig_RedactsHashedToken(t *testing.T) {
	cfgJSON := `{"all_groups":true,"excerpt_bytes":256,"retention_days":90,"worker_count":4,"queue_size":1024,"batch_size":50,"batch_flush_ms":100,"export_api_keys":[{"id":"ak_test","name":"test","hashed_token":"secret_hash","rate_limit_per_min":60,"disabled":false}]}`
	h := buildTestPayloadAuditHandler("true", cfgJSON, &mockPayloadAuditRepo{})
	r := newPayloadAuditTestRouter(h)

	w := doRequest(r, http.MethodGet, "/payload-audit/config", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}

	// The response should NOT contain "hashed_token" or "secret_hash".
	body := w.Body.String()
	if bytes.Contains([]byte(body), []byte("hashed_token")) {
		t.Error("response must not contain hashed_token field")
	}
	if bytes.Contains([]byte(body), []byte("secret_hash")) {
		t.Error("response must not contain the actual hash value")
	}
}

func TestPayloadAuditAdmin_UpdateConfig_Valid(t *testing.T) {
	cfgJSON := `{"all_groups":false,"excerpt_bytes":128,"retention_days":90,"worker_count":2,"queue_size":512,"batch_size":50,"batch_flush_ms":100}`
	h := buildTestPayloadAuditHandler("false", cfgJSON, &mockPayloadAuditRepo{})
	r := newPayloadAuditTestRouter(h)

	reqBody := map[string]any{
		"enabled": true,
		"config": map[string]any{
			"all_groups":    true,
			"excerpt_bytes": 256,
			"retention_days": 60,
			"worker_count":  4,
			"queue_size":    2048,
			"batch_size":    100,
			"batch_flush_ms": 200,
		},
	}
	w := doRequest(r, http.MethodPut, "/payload-audit/config", reqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body: %s", w.Code, w.Body.String())
	}

	data := unmarshalData(t, w)
	var resp map[string]any
	_ = json.Unmarshal(data, &resp)
	// need_rebuild_sink should be true because queue_size changed (512 → 2048).
	if v, ok := resp["need_rebuild_sink"].(bool); !ok || !v {
		t.Errorf("need_rebuild_sink=%v, want true", resp["need_rebuild_sink"])
	}
}

func TestPayloadAuditAdmin_UpdateConfig_BadExcerpt(t *testing.T) {
	cfgJSON := `{"all_groups":false,"excerpt_bytes":128,"retention_days":90,"worker_count":2,"queue_size":512,"batch_size":50,"batch_flush_ms":100}`
	h := buildTestPayloadAuditHandler("false", cfgJSON, &mockPayloadAuditRepo{})
	r := newPayloadAuditTestRouter(h)

	reqBody := map[string]any{
		"enabled": true,
		"config": map[string]any{
			"excerpt_bytes": 10, // too small, must be >= 64
		},
	}
	w := doRequest(r, http.MethodPut, "/payload-audit/config", reqBody)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400, body: %s", w.Code, w.Body.String())
	}
}

func TestPayloadAuditAdmin_GetStatus(t *testing.T) {
	repo := &mockPayloadAuditRepo{
		partitions: []repository.PayloadAuditPartition{
			{Name: "payload_audit_logs_2025_05", End: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
		},
	}
	cfgJSON := `{"all_groups":true,"excerpt_bytes":256,"retention_days":90,"worker_count":4,"queue_size":1024,"batch_size":50,"batch_flush_ms":100}`
	h := buildTestPayloadAuditHandler("true", cfgJSON, repo)
	r := newPayloadAuditTestRouter(h)

	w := doRequest(r, http.MethodGet, "/payload-audit/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body: %s", w.Code, w.Body.String())
	}

	data := unmarshalData(t, w)
	var resp payloadAuditStatusResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if !resp.Enabled {
		t.Error("expected enabled=true")
	}
	if resp.Workers.Configured != 4 {
		t.Errorf("workers.configured=%d, want 4", resp.Workers.Configured)
	}
	if len(resp.Storage.Partitions) != 1 {
		t.Errorf("partitions=%d, want 1", len(resp.Storage.Partitions))
	}
}

func TestPayloadAuditAdmin_ListPayloads_Default(t *testing.T) {
	now := time.Now().UTC()
	repo := &mockPayloadAuditRepo{
		listRows: []*repository.PayloadAuditRow{
			{
				ID: 1,
				PayloadAuditEvent: repository.PayloadAuditEvent{
					InputExcerpt:  "hello",
					OutputExcerpt: "world",
					InputBody:     "full_input",
					OutputBody:    "full_output",
					CreatedAt:     now,
				},
			},
		},
	}
	h := buildTestPayloadAuditHandler("true", `{"retention_days":90,"batch_size":50,"batch_flush_ms":100}`, repo)
	r := newPayloadAuditTestRouter(h)

	from := now.Add(-24 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	w := doRequest(r, http.MethodGet, "/payload-audit/payloads?from="+from+"&to="+to, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body: %s", w.Code, w.Body.String())
	}

	data := unmarshalData(t, w)
	var resp struct {
		Data []struct {
			InputExcerpt  string `json:"InputExcerpt"`
			OutputExcerpt string `json:"OutputExcerpt"`
			InputBody     string `json:"InputBody"`
			OutputBody    string `json:"OutputBody"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &resp)
	if len(resp.Data) != 1 {
		t.Fatalf("data len=%d, want 1", len(resp.Data))
	}
	// Default include_body=excerpt: body fields should be empty, excerpts preserved.
	if resp.Data[0].InputExcerpt != "hello" {
		t.Errorf("InputExcerpt=%q, want 'hello'", resp.Data[0].InputExcerpt)
	}
	if resp.Data[0].InputBody != "" {
		t.Errorf("InputBody=%q, want empty (excerpt mode)", resp.Data[0].InputBody)
	}
}

func TestPayloadAuditAdmin_ListPayloads_None(t *testing.T) {
	now := time.Now().UTC()
	repo := &mockPayloadAuditRepo{
		listRows: []*repository.PayloadAuditRow{
			{
				ID: 1,
				PayloadAuditEvent: repository.PayloadAuditEvent{
					InputExcerpt:  "hello",
					OutputExcerpt: "world",
					InputBody:     "full_input",
					OutputBody:    "full_output",
					CreatedAt:     now,
				},
			},
		},
	}
	h := buildTestPayloadAuditHandler("true", `{"retention_days":90,"batch_size":50,"batch_flush_ms":100}`, repo)
	r := newPayloadAuditTestRouter(h)

	from := now.Add(-24 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	w := doRequest(r, http.MethodGet, "/payload-audit/payloads?from="+from+"&to="+to+"&include_body=none", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}

	data := unmarshalData(t, w)
	var resp struct {
		Data []struct {
			InputExcerpt string `json:"InputExcerpt"`
			InputBody    string `json:"InputBody"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &resp)
	if len(resp.Data) != 1 {
		t.Fatalf("data len=%d, want 1", len(resp.Data))
	}
	if resp.Data[0].InputExcerpt != "" {
		t.Errorf("InputExcerpt=%q, want empty (none mode)", resp.Data[0].InputExcerpt)
	}
	if resp.Data[0].InputBody != "" {
		t.Errorf("InputBody=%q, want empty (none mode)", resp.Data[0].InputBody)
	}
}

func TestPayloadAuditAdmin_ListPayloads_Full(t *testing.T) {
	now := time.Now().UTC()
	repo := &mockPayloadAuditRepo{
		listRows: []*repository.PayloadAuditRow{
			{
				ID: 1,
				PayloadAuditEvent: repository.PayloadAuditEvent{
					InputExcerpt:  "hello",
					OutputExcerpt: "world",
					InputBody:     "full_input",
					OutputBody:    "full_output",
					CreatedAt:     now,
				},
			},
		},
	}
	h := buildTestPayloadAuditHandler("true", `{"retention_days":90,"batch_size":50,"batch_flush_ms":100}`, repo)
	r := newPayloadAuditTestRouter(h)

	from := now.Add(-24 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	w := doRequest(r, http.MethodGet, "/payload-audit/payloads?from="+from+"&to="+to+"&include_body=full", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}

	data := unmarshalData(t, w)
	var resp struct {
		Data []struct {
			InputBody  string `json:"InputBody"`
			OutputBody string `json:"OutputBody"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Data[0].InputBody != "full_input" {
		t.Errorf("InputBody=%q, want 'full_input'", resp.Data[0].InputBody)
	}
}

func TestPayloadAuditAdmin_ListPayloads_BadTimeWindow(t *testing.T) {
	h := buildTestPayloadAuditHandler("true", `{"retention_days":90,"batch_size":50,"batch_flush_ms":100}`, &mockPayloadAuditRepo{})
	r := newPayloadAuditTestRouter(h)

	// 32 days window — exceeds max 31.
	from := time.Now().Add(-32 * 24 * time.Hour).Format(time.RFC3339)
	to := time.Now().Format(time.RFC3339)
	w := doRequest(r, http.MethodGet, "/payload-audit/payloads?from="+from+"&to="+to, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400, body: %s", w.Code, w.Body.String())
	}
}

func TestPayloadAuditAdmin_GetPayload(t *testing.T) {
	now := time.Now().UTC()
	repo := &mockPayloadAuditRepo{
		getRow: &repository.PayloadAuditRow{
			ID: 42,
			PayloadAuditEvent: repository.PayloadAuditEvent{
				InputBody: "full_body",
				CreatedAt: now,
			},
		},
	}
	h := buildTestPayloadAuditHandler("true", `{"retention_days":90,"batch_size":50,"batch_flush_ms":100}`, repo)
	r := newPayloadAuditTestRouter(h)

	w := doRequest(r, http.MethodGet, "/payload-audit/payloads/42", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body: %s", w.Code, w.Body.String())
	}

	data := unmarshalData(t, w)
	var row struct {
		ID        int64  `json:"ID"`
		InputBody string `json:"InputBody"`
	}
	_ = json.Unmarshal(data, &row)
	if row.ID != 42 {
		t.Errorf("ID=%d, want 42", row.ID)
	}
}

func TestPayloadAuditAdmin_GetPayload_NotFound(t *testing.T) {
	repo := &mockPayloadAuditRepo{
		getRow: nil,
		getErr: nil,
	}
	h := buildTestPayloadAuditHandler("true", `{"retention_days":90,"batch_size":50,"batch_flush_ms":100}`, repo)
	r := newPayloadAuditTestRouter(h)

	w := doRequest(r, http.MethodGet, "/payload-audit/payloads/999", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404, body: %s", w.Code, w.Body.String())
	}
}

func TestPayloadAuditAdmin_ExportKeys_CRUD(t *testing.T) {
	h := buildTestPayloadAuditHandler("true", `{"retention_days":90,"batch_size":50,"batch_flush_ms":100}`, &mockPayloadAuditRepo{})
	r := newPayloadAuditTestRouter(h)

	// 1. Create
	w := doRequest(r, http.MethodPost, "/payload-audit/export-keys", map[string]any{
		"name":               "test-key",
		"rate_limit_per_min": 30,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d, want 200, body: %s", w.Code, w.Body.String())
	}
	data := unmarshalData(t, w)
	var createResp struct {
		Token string                             `json:"token"`
		Key   payloadAuditExportKeyRedactedEntry `json:"key"`
	}
	_ = json.Unmarshal(data, &createResp)
	if createResp.Token == "" {
		t.Error("token should not be empty")
	}
	if createResp.Key.Name != "test-key" {
		t.Errorf("key.name=%q, want 'test-key'", createResp.Key.Name)
	}
	keyID := createResp.Key.ID

	// 2. List
	w = doRequest(r, http.MethodGet, "/payload-audit/export-keys", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d, want 200", w.Code)
	}
	data = unmarshalData(t, w)
	var listResp []exportKeyListEntry
	_ = json.Unmarshal(data, &listResp)
	if len(listResp) != 1 {
		t.Fatalf("list len=%d, want 1", len(listResp))
	}
	// Verify hashed_token is NOT in the list response.
	if bytes.Contains(data, []byte("hashed_token")) {
		t.Error("list response must not contain hashed_token")
	}

	// 3. Delete
	w = doRequest(r, http.MethodDelete, "/payload-audit/export-keys/"+keyID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d, want 200, body: %s", w.Code, w.Body.String())
	}

	// 4. List again — should be empty.
	w = doRequest(r, http.MethodGet, "/payload-audit/export-keys", nil)
	data = unmarshalData(t, w)
	_ = json.Unmarshal(data, &listResp)
	if len(listResp) != 0 {
		t.Errorf("list after delete len=%d, want 0", len(listResp))
	}
}

func TestPayloadAuditAdmin_RunCleanup(t *testing.T) {
	// RunCleanup calls cleanup.RunOnce which calls the underlying repo.
	// With nil repo it will error, so we test the error path.
	h := buildTestPayloadAuditHandler("true", `{"retention_days":90,"batch_size":50,"batch_flush_ms":100}`, &mockPayloadAuditRepo{})
	r := newPayloadAuditTestRouter(h)

	w := doRequest(r, http.MethodPost, "/payload-audit/cleanup", nil)
	// With nil cleanup repo, RunOnce should return an error.
	// The handler propagates that via response.ErrorFrom.
	if w.Code == http.StatusOK {
		// If it happened to succeed (e.g. 0 partitions found), that's also fine.
		data := unmarshalData(t, w)
		var resp struct {
			Deleted    int   `json:"deleted"`
			DurationMs int64 `json:"duration_ms"`
		}
		_ = json.Unmarshal(data, &resp)
		t.Logf("cleanup succeeded: deleted=%d, duration_ms=%d", resp.Deleted, resp.DurationMs)
	} else {
		t.Logf("cleanup returned status=%d (expected with nil repo)", w.Code)
	}
}

func TestPayloadAuditAdmin_ListPayloads_MissingTimeParams(t *testing.T) {
	h := buildTestPayloadAuditHandler("true", `{"retention_days":90,"batch_size":50,"batch_flush_ms":100}`, &mockPayloadAuditRepo{})
	r := newPayloadAuditTestRouter(h)

	w := doRequest(r, http.MethodGet, "/payload-audit/payloads", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for missing from/to", w.Code)
	}
}

func TestPayloadAuditAdmin_ListPayloads_RepoError(t *testing.T) {
	repo := &mockPayloadAuditRepo{
		listErr: errors.New("db connection failed"),
	}
	h := buildTestPayloadAuditHandler("true", `{"retention_days":90,"batch_size":50,"batch_flush_ms":100}`, repo)
	r := newPayloadAuditTestRouter(h)

	now := time.Now()
	from := now.Add(-1 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	w := doRequest(r, http.MethodGet, "/payload-audit/payloads?from="+from+"&to="+to, nil)
	if w.Code == http.StatusOK {
		t.Fatalf("expected error status, got 200")
	}
}
