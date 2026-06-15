package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const (
	// AuditExportKeyIDCtxKey is the gin context key for the authenticated export key ID.
	AuditExportKeyIDCtxKey = "audit_export_key_id"
	// AuditExportKeyNameCtxKey is the gin context key for the authenticated export key name.
	AuditExportKeyNameCtxKey = "audit_export_key_name"
)

// AuditExportRateLimiter abstracts per-key rate limiting for the audit export API.
type AuditExportRateLimiter interface {
	// Allow checks whether the given keyID is within its rate limit.
	// Returns allowed=true if the request should proceed.
	// On deny, retryAfterSec hints when the client may retry.
	Allow(ctx context.Context, keyID string, ratePerMin int) (allowed bool, retryAfterSec int)
}

// --- Redis sliding-window implementation ---

var auditExportRateLimitScript = redis.NewScript(`
local current = redis.call('INCR', KEYS[1])
if current == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
local ttl = redis.call('TTL', KEYS[1])
if ttl == -1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return {current, ttl}
`)

// RedisAuditExportRateLimiter implements AuditExportRateLimiter using Redis fixed-window counters.
type RedisAuditExportRateLimiter struct {
	rdb *redis.Client
}

// NewRedisAuditExportRateLimiter creates a new Redis-backed rate limiter for audit export.
func NewRedisAuditExportRateLimiter(rdb *redis.Client) *RedisAuditExportRateLimiter {
	return &RedisAuditExportRateLimiter{rdb: rdb}
}

// Allow implements AuditExportRateLimiter.
// Returns (true, 0) on allow, (false, retryAfterSec) on deny.
// On Redis error or unexpected result, returns (false, 60) — fail-closed.
func (l *RedisAuditExportRateLimiter) Allow(ctx context.Context, keyID string, ratePerMin int) (bool, int) {
	key := fmt.Sprintf("payload_audit:export:rate:%s", keyID)
	windowSec := int64(60)

	result, err := auditExportRateLimitScript.Run(ctx, l.rdb, []string{key}, windowSec).Slice()
	if err != nil {
		slog.Warn("audit export rate limiter redis error, fail-closed", "key_id", keyID, "error", err)
		return false, 60
	}

	if len(result) < 2 {
		slog.Warn("audit export rate limiter unexpected result, fail-closed", "key_id", keyID)
		return false, 60
	}

	count, ok1 := toInt64(result[0])
	ttl, ok2 := toInt64(result[1])
	if !ok1 || !ok2 {
		slog.Warn("audit export rate limiter parse error, fail-closed", "key_id", keyID)
		return false, 60
	}

	if count > int64(ratePerMin) {
		retryAfter := int(ttl)
		if retryAfter < 1 {
			retryAfter = 60
		}
		return false, retryAfter
	}
	return true, 0
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}

// --- Middleware ---

// AuditExportAuthMiddleware validates Bearer token authentication and applies
// per-key rate limiting for payload audit export endpoints.
//
// Failure responses:
//   - Missing/malformed Authorization: 401, {"error": "unauthorized"}
//   - Unknown/disabled token: 401, {"error": "unauthorized"}
//   - Rate limited: 429, Retry-After header, {"error": "rate limit"}
//
// On success:
//   - Sets AuditExportKeyIDCtxKey and AuditExportKeyNameCtxKey in gin context
//   - Asynchronously calls svc.MarkExportKeyUsed
//   - Calls c.Next()
func AuditExportAuthMiddleware(svc *service.PayloadAuditService, limiter AuditExportRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		// --- Extract Bearer token ---
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		token := strings.TrimSpace(parts[1])
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		// --- Lookup token in snapshot ---
		snap := svc.Snapshot()
		key := snap.FindExportKey(token)
		if key == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		// --- Rate limit ---
		allowed, retryAfter := limiter.Allow(c.Request.Context(), key.ID, key.RateLimitPerMin)
		if !allowed {
			c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit"})
			return
		}

		// --- Success ---
		c.Set(AuditExportKeyIDCtxKey, key.ID)
		c.Set(AuditExportKeyNameCtxKey, key.Name)

		// Fire-and-forget last-used update.
		go svc.MarkExportKeyUsed(context.Background(), key.ID)

		c.Next()
	}
}

// --- Fail-open wrapper (used by RedisAuditExportRateLimiter internally) ---
// The Redis implementation already handles fail-open internally.
// For non-Redis limiters that may panic or error, wrap at the call site.

// NewAuditExportRateLimiter creates the appropriate rate limiter.
// If rdb is nil, returns a no-op limiter that always allows.
func NewAuditExportRateLimiter(rdb *redis.Client) AuditExportRateLimiter {
	if rdb == nil {
		return &noopAuditExportRateLimiter{}
	}
	return NewRedisAuditExportRateLimiter(rdb)
}

type noopAuditExportRateLimiter struct{}

func (n *noopAuditExportRateLimiter) Allow(_ context.Context, _ string, _ int) (bool, int) {
	return true, 0
}

// FailOpenRateLimiter wraps any AuditExportRateLimiter and recovers from panics,
// logging a warning and allowing the request through.
type FailOpenRateLimiter struct {
	Inner AuditExportRateLimiter
}

// Allow delegates to Inner; on panic or nil Inner, returns (true, 0).
func (f *FailOpenRateLimiter) Allow(ctx context.Context, keyID string, ratePerMin int) (allowed bool, retryAfterSec int) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("audit export rate limiter panic, fail-open", "key_id", keyID, "panic", r)
			allowed = true
			retryAfterSec = 0
		}
	}()
	if f.Inner == nil {
		return true, 0
	}
	return f.Inner.Allow(ctx, keyID, ratePerMin)
}

