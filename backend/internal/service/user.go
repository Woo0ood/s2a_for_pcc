package service

import (
	"time"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID             int64
	Email          string
	Username       string
	Notes          string
	AvatarURL      string
	AvatarSource   string
	AvatarMIME     string
	AvatarByteSize int
	AvatarSHA256   string
	PasswordHash   string
	Role           string
	Balance        float64
	Concurrency    int
	Status         string
	AllowedGroups  []int64
	TokenVersion   int64 // Incremented on password change to invalidate existing tokens
	// TokenVersionResolved indicates TokenVersion already contains the fingerprint-derived
	// value expected in JWT claims and refresh-token state.
	TokenVersionResolved bool
	SignupSource         string
	LastLoginAt          *time.Time
	LastActiveAt         *time.Time
	LastUsedAt           *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time

	// GroupRates 用户专属分组倍率配置
	// map[groupID]rateMultiplier
	GroupRates map[int64]float64

	// TOTP 双因素认证字段
	TotpSecretEncrypted *string    // AES-256-GCM 加密的 TOTP 密钥
	TotpEnabled         bool       // 是否启用 TOTP
	TotpEnabledAt       *time.Time // TOTP 启用时间

	// 余额不足通知
	BalanceNotifyEnabled       bool
	BalanceNotifyThresholdType string // "fixed" (default) | "percentage"
	BalanceNotifyThreshold     *float64
	BalanceNotifyExtraEmails   []NotifyEmailEntry
	TotalRecharged             float64

	// RPMLimit 用户级每分钟请求数上限（0 = 不限制）。仅在所用分组未设置 rpm_limit
	// 且该 (用户, 分组) 无 rpm_override 时作为全局兜底生效，计数键 rpm:u:{userID}:{min}。
	RPMLimit int

	// UserGroupRPMOverride 来自 auth cache snapshot 的 (user, group) RPM 覆盖值。
	// nil = 该 API Key 对应的 (user, group) 无 override；非 nil 时 checkRPM 直接使用，
	// 避免每请求查 DB。字段不持久化到数据库。
	UserGroupRPMOverride *int

	// 用户级 5h/7d USD 限额（跨所有 API Key 聚合；0 = 不限制）。
	// 与 APIKey 自身的 rate_limit_5h/7d 取最严格并行检查。
	RateLimit5h   float64
	RateLimit7d   float64
	Usage5h       float64
	Usage7d       float64
	Window5hStart *time.Time
	Window7dStart *time.Time

	APIKeys       []APIKey
	Subscriptions []UserSubscription
}

func (u *User) IsAdmin() bool {
	return u.Role == RoleAdmin
}

func (u *User) IsActive() bool {
	return u.Status == StatusActive
}

// CanBindGroup checks whether a user can bind to a given group.
// For standard groups:
// - Public groups (non-exclusive): all users can bind
// - Exclusive groups: only users with the group in AllowedGroups can bind
func (u *User) CanBindGroup(groupID int64, isExclusive bool) bool {
	// 公开分组（非专属）：所有用户都可以绑定
	if !isExclusive {
		return true
	}
	// 专属分组：需要在 AllowedGroups 中
	for _, id := range u.AllowedGroups {
		if id == groupID {
			return true
		}
	}
	return false
}

func (u *User) SetPassword(password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.PasswordHash = string(hash)
	return nil
}

func (u *User) CheckPassword(password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
}

// HasUserRateLimits 返回是否设置了任一用户级 USD 限额。
func (u *User) HasUserRateLimits() bool {
	return u.RateLimit5h > 0 || u.RateLimit7d > 0
}

// EffectiveUsage5h 返回当前 5h 窗口的有效用量；窗口过期返回 0。
func (u *User) EffectiveUsage5h() float64 {
	if IsWindowExpired(u.Window5hStart, RateLimitWindow5h) {
		return 0
	}
	return u.Usage5h
}

// EffectiveUsage7d 返回当前 7d 窗口的有效用量；窗口过期返回 0。
func (u *User) EffectiveUsage7d() float64 {
	if IsWindowExpired(u.Window7dStart, RateLimitWindow7d) {
		return 0
	}
	return u.Usage7d
}
