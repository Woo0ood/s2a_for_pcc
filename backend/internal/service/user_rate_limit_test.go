package service

import (
	"testing"
	"time"
)

func TestUser_EffectiveUsage(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name   string
		user   User
		want5h float64
		want7d float64
	}{
		{
			name: "all windows active",
			user: User{
				Usage5h:       5.0,
				Usage7d:       50.0,
				Window5hStart: userRateLimitTimePtr(now.Add(-1 * time.Hour)),
				Window7dStart: userRateLimitTimePtr(now.Add(-3 * 24 * time.Hour)),
			},
			want5h: 5.0,
			want7d: 50.0,
		},
		{
			name: "all windows expired",
			user: User{
				Usage5h:       5.0,
				Usage7d:       50.0,
				Window5hStart: userRateLimitTimePtr(now.Add(-6 * time.Hour)),
				Window7dStart: userRateLimitTimePtr(now.Add(-8 * 24 * time.Hour)),
			},
			want5h: 0,
			want7d: 0,
		},
		{
			name: "nil window starts return 0",
			user: User{
				Usage5h:       5.0,
				Usage7d:       50.0,
				Window5hStart: nil,
				Window7dStart: nil,
			},
			want5h: 0,
			want7d: 0,
		},
		{
			name: "mixed: 5h expired, 7d active",
			user: User{
				Usage5h:       5.0,
				Usage7d:       50.0,
				Window5hStart: userRateLimitTimePtr(now.Add(-6 * time.Hour)),
				Window7dStart: userRateLimitTimePtr(now.Add(-3 * 24 * time.Hour)),
			},
			want5h: 0,
			want7d: 50.0,
		},
		{
			name: "boundary: exactly 5h ago (treated as expired)",
			user: User{
				Usage5h:       5.0,
				Window5hStart: userRateLimitTimePtr(now.Add(-5 * time.Hour)),
			},
			want5h: 0,
			want7d: 0,
		},
		{
			name: "boundary: 7d window just under expiry (6d23h ago)",
			user: User{
				Usage7d:       50.0,
				Window7dStart: userRateLimitTimePtr(now.Add(-(7*24 - 1) * time.Hour)),
			},
			want5h: 0,
			want7d: 50.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.user.EffectiveUsage5h(); got != tt.want5h {
				t.Errorf("User.EffectiveUsage5h() = %v, want %v", got, tt.want5h)
			}
			if got := tt.user.EffectiveUsage7d(); got != tt.want7d {
				t.Errorf("User.EffectiveUsage7d() = %v, want %v", got, tt.want7d)
			}
		})
	}
}

func TestUser_HasUserRateLimits(t *testing.T) {
	tests := []struct {
		name string
		user User
		want bool
	}{
		{name: "no limits", user: User{}, want: false},
		{name: "only 5h set", user: User{RateLimit5h: 10}, want: true},
		{name: "only 7d set", user: User{RateLimit7d: 100}, want: true},
		{name: "both set", user: User{RateLimit5h: 10, RateLimit7d: 100}, want: true},
		{name: "both zero (treated as unlimited)", user: User{RateLimit5h: 0, RateLimit7d: 0}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.user.HasUserRateLimits(); got != tt.want {
				t.Errorf("HasUserRateLimits() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUserRateLimitData_EffectiveUsage(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name   string
		data   UserRateLimitData
		want5h float64
		want7d float64
	}{
		{
			name: "all windows active",
			data: UserRateLimitData{
				Usage5h:       3.0,
				Usage7d:       40.0,
				Window5hStart: userRateLimitTimePtr(now.Add(-2 * time.Hour)),
				Window7dStart: userRateLimitTimePtr(now.Add(-2 * 24 * time.Hour)),
			},
			want5h: 3.0,
			want7d: 40.0,
		},
		{
			name: "all windows expired",
			data: UserRateLimitData{
				Usage5h:       3.0,
				Usage7d:       40.0,
				Window5hStart: userRateLimitTimePtr(now.Add(-10 * time.Hour)),
				Window7dStart: userRateLimitTimePtr(now.Add(-10 * 24 * time.Hour)),
			},
			want5h: 0,
			want7d: 0,
		},
		{
			name: "nil window starts return 0",
			data: UserRateLimitData{
				Usage5h:       3.0,
				Usage7d:       40.0,
			},
			want5h: 0,
			want7d: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.data.EffectiveUsage5h(); got != tt.want5h {
				t.Errorf("UserRateLimitData.EffectiveUsage5h() = %v, want %v", got, tt.want5h)
			}
			if got := tt.data.EffectiveUsage7d(); got != tt.want7d {
				t.Errorf("UserRateLimitData.EffectiveUsage7d() = %v, want %v", got, tt.want7d)
			}
		})
	}
}

func userRateLimitTimePtr(t time.Time) *time.Time {
	return &t
}
