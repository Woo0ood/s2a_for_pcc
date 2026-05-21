package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// --- Test limiter mocks ---

type alwaysAllowLimiter struct{}

func (l *alwaysAllowLimiter) Allow(_ context.Context, _ string, _ int) (bool, int) {
	return true, 0
}

type alwaysDenyLimiter struct{ retryAfter int }

func (l *alwaysDenyLimiter) Allow(_ context.Context, _ string, _ int) (bool, int) {
	return false, l.retryAfter
}

type alwaysErrorLimiter struct{}

func (l *alwaysErrorLimiter) Allow(_ context.Context, _ string, _ int) (bool, int) {
	panic("redis connection failed")
}

// --- Helpers ---

func init() {
	gin.SetMode(gin.TestMode)
}

type ginCtxOption func(*http.Request)

func withAuth(value string) ginCtxOption {
	return func(r *http.Request) {
		r.Header.Set("Authorization", value)
	}
}

func newGinCtx(opts ...ginCtxOption) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	for _, o := range opts {
		o(r)
	}
	c, _ := gin.CreateTestContext(w)
	c.Request = r
	return c, w
}

// setupSvcWithKey creates a PayloadAuditService with one export key and returns
// the service and the plaintext token.
func setupSvcWithKey(t *testing.T, name string, ratePerMin int) (*service.PayloadAuditService, string) {
	t.Helper()
	repo := newMockPAASettingsRepo()
	svc, err := service.ProvidePayloadAuditService(repo, nil, 0, nil)
	require.NoError(t, err)

	// Enable audit so snapshot is active.
	_, err = svc.UpdateConfig(context.Background(), true, service.PayloadAuditConfig{})
	require.NoError(t, err)

	plain, _, err := svc.CreateExportKey(context.Background(), name, ratePerMin)
	require.NoError(t, err)
	return svc, plain
}

// --- In-memory mock settings repo (mirrors service test helper) ---

type mockPAASettingsRepo struct {
	data map[string]string
}

func newMockPAASettingsRepo() *mockPAASettingsRepo {
	return &mockPAASettingsRepo{data: make(map[string]string)}
}

func (m *mockPAASettingsRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	v, ok := m.data[key]
	if !ok {
		return nil, service.ErrSettingNotFound
	}
	return &service.Setting{Key: key, Value: v, UpdatedAt: time.Now()}, nil
}

func (m *mockPAASettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	v, ok := m.data[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return v, nil
}

func (m *mockPAASettingsRepo) Set(_ context.Context, key, value string) error {
	m.data[key] = value
	return nil
}

func (m *mockPAASettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := m.data[k]; ok {
			result[k] = v
		}
	}
	return result, nil
}

func (m *mockPAASettingsRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	for k, v := range settings {
		m.data[k] = v
	}
	return nil
}

func (m *mockPAASettingsRepo) GetAll(_ context.Context) (map[string]string, error) {
	result := make(map[string]string, len(m.data))
	for k, v := range m.data {
		result[k] = v
	}
	return result, nil
}

func (m *mockPAASettingsRepo) Delete(_ context.Context, key string) error {
	delete(m.data, key)
	return nil
}

// --- Tests ---

func TestAuditExportAuth_NoAuthorizationHeader_401(t *testing.T) {
	svc, _ := setupSvcWithKey(t, "test-key", 60)
	mw := AuditExportAuthMiddleware(svc, &alwaysAllowLimiter{})
	c, w := newGinCtx()
	mw(c)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.True(t, c.IsAborted())
}

func TestAuditExportAuth_BadTokenFormat_BasicScheme_401(t *testing.T) {
	svc, _ := setupSvcWithKey(t, "test-key", 60)
	mw := AuditExportAuthMiddleware(svc, &alwaysAllowLimiter{})
	c, w := newGinCtx(withAuth("Basic dXNlcjpwYXNz"))
	mw(c)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.True(t, c.IsAborted())
}

func TestAuditExportAuth_BadTokenFormat_EmptyBearer_401(t *testing.T) {
	svc, _ := setupSvcWithKey(t, "test-key", 60)
	mw := AuditExportAuthMiddleware(svc, &alwaysAllowLimiter{})
	c, w := newGinCtx(withAuth("Bearer "))
	mw(c)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.True(t, c.IsAborted())
}

func TestAuditExportAuth_UnknownToken_401(t *testing.T) {
	svc, _ := setupSvcWithKey(t, "test-key", 60)
	mw := AuditExportAuthMiddleware(svc, &alwaysAllowLimiter{})
	c, w := newGinCtx(withAuth("Bearer sk-pa-INVALIDTOKENVALUE"))
	mw(c)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.True(t, c.IsAborted())
}

func TestAuditExportAuth_DisabledKey_401(t *testing.T) {
	repo := newMockPAASettingsRepo()
	svc, err := service.ProvidePayloadAuditService(repo, nil, 0, nil)
	require.NoError(t, err)

	_, err = svc.UpdateConfig(context.Background(), true, service.PayloadAuditConfig{})
	require.NoError(t, err)

	plain, key, err := svc.CreateExportKey(context.Background(), "will-disable", 60)
	require.NoError(t, err)

	// Disable the key by updating config.
	_, err = svc.UpdateConfig(context.Background(), true, service.PayloadAuditConfig{
		ExportAPIKeys: []service.PayloadAuditExportKey{
			{
				ID:              key.ID,
				Name:            key.Name,
				HashedToken:     key.HashedToken,
				RateLimitPerMin: key.RateLimitPerMin,
				CreatedAt:       key.CreatedAt,
				Disabled:        true,
			},
		},
	})
	require.NoError(t, err)

	mw := AuditExportAuthMiddleware(svc, &alwaysAllowLimiter{})
	c, w := newGinCtx(withAuth("Bearer " + plain))
	mw(c)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.True(t, c.IsAborted())
}

func TestAuditExportAuth_ValidToken_PassesAndAttachesContext(t *testing.T) {
	svc, plain := setupSvcWithKey(t, "test-key", 60)
	mw := AuditExportAuthMiddleware(svc, &alwaysAllowLimiter{})
	c, w := newGinCtx(withAuth("Bearer " + plain))
	mw(c)
	require.False(t, c.IsAborted())
	require.Equal(t, http.StatusOK, w.Code)

	keyID, exists := c.Get(AuditExportKeyIDCtxKey)
	require.True(t, exists)
	require.NotEmpty(t, keyID)

	keyName, exists := c.Get(AuditExportKeyNameCtxKey)
	require.True(t, exists)
	require.Equal(t, "test-key", keyName)
}

func TestAuditExportAuth_RateLimit_429(t *testing.T) {
	svc, plain := setupSvcWithKey(t, "test-key", 1)
	limiter := &alwaysDenyLimiter{retryAfter: 60}
	mw := AuditExportAuthMiddleware(svc, limiter)
	c, w := newGinCtx(withAuth("Bearer " + plain))
	mw(c)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Equal(t, "60", w.Header().Get("Retry-After"))
	require.True(t, c.IsAborted())
}

func TestAuditExportAuth_RateLimitFailure_FailsOpen(t *testing.T) {
	svc, plain := setupSvcWithKey(t, "test-key", 60)
	// alwaysErrorLimiter panics — wrapped with FailOpenRateLimiter.
	limiter := &FailOpenRateLimiter{Inner: &alwaysErrorLimiter{}}
	mw := AuditExportAuthMiddleware(svc, limiter)
	c, w := newGinCtx(withAuth("Bearer " + plain))
	mw(c)
	// fail-open: should not abort.
	require.False(t, c.IsAborted())
	require.Equal(t, http.StatusOK, w.Code)

	keyID, exists := c.Get(AuditExportKeyIDCtxKey)
	require.True(t, exists)
	require.NotEmpty(t, keyID)
}

func TestAuditExportAuth_NoScheme_401(t *testing.T) {
	svc, plain := setupSvcWithKey(t, "test-key", 60)
	mw := AuditExportAuthMiddleware(svc, &alwaysAllowLimiter{})
	// No "Bearer" prefix, just raw token.
	c, w := newGinCtx(withAuth(plain))
	mw(c)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.True(t, c.IsAborted())
}

func TestNewAuditExportRateLimiter_NilRedis_ReturnsNoop(t *testing.T) {
	limiter := NewAuditExportRateLimiter(nil)
	allowed, retry := limiter.Allow(context.Background(), "any", 10)
	require.True(t, allowed)
	require.Equal(t, 0, retry)
}

func TestFailOpenRateLimiter_NilInner(t *testing.T) {
	limiter := &FailOpenRateLimiter{Inner: nil}
	allowed, retry := limiter.Allow(context.Background(), "any", 10)
	require.True(t, allowed)
	require.Equal(t, 0, retry)
}
