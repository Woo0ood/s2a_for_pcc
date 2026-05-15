package handler

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// applyUserRateLimitFallback inspects the result of BillingCacheService.CheckBillingEligibility.
// When the error is a user-level 5h/7d rate-limit and a platform-level fallback model is
// configured (reusing the existing EnableModelFallback + FallbackModel{Platform} settings),
// it rewrites the request body's `model` field to the fallback model and clears the error,
// letting the request proceed via the normal upstream-routing path.
//
// IMPORTANT: only the body is rewritten; the caller's `reqModel` local variable is NOT
// changed. Routing (account selection, channel mapping) continues to use the user's
// originally-requested model, and usage_logs records:
//   - RequestedModel  = original reqModel       (what the client asked for)
//   - UpstreamModel   = result.UpstreamModel    (the fallback model the upstream echoed back)
// — so the admin call log naturally renders the same "original → fallback" two-line cell
// that the existing channel-side model-redirect mechanism produces.
//
// Returns:
//   - newBody:  rewritten body when fallback engaged; original body otherwise.
//   - engaged:  true when the body was rewritten to a fallback model; caller must
//     override channelMapping.BillingModelSource to service.BillingModelSourceRequested
//     before calling ToUsageFields so the user is billed at the original (typically
//     more expensive) requested-model price. This prevents users from gaming the
//     rate-limit cap by burning through their quota to fall back to a cheaper model
//     at cheaper prices.
//   - finalErr: nil when fallback engaged (request proceeds) or original billingErr was nil;
//     otherwise the original billingErr so the caller can return its normal 429/403/etc.
//
// The response body's `model` field is intentionally NOT rewritten back to the original
// requested model — clients see the actual fallback model. TODO: once client SDKs adopt
// fallback awareness, evaluate transparent rewriting + X-Model-Fallback header.
func applyUserRateLimitFallback(
	ctx context.Context,
	body []byte,
	reqModel string,
	userID int64,
	platform string,
	billingErr error,
	settingService *service.SettingService,
) (newBody []byte, engaged bool, finalErr error) {
	if billingErr == nil {
		return body, false, nil
	}
	if settingService == nil {
		return body, false, billingErr
	}
	if !errors.Is(billingErr, service.ErrUserRateLimit5hExceeded) && !errors.Is(billingErr, service.ErrUserRateLimit7dExceeded) {
		return body, false, billingErr
	}
	if !settingService.IsModelFallbackEnabled(ctx) {
		return body, false, billingErr
	}
	fb := settingService.GetFallbackModel(ctx, platform)
	if fb == "" || fb == reqModel {
		return body, false, billingErr
	}

	rewritten := service.ReplaceModelInBody(body, fb)
	reason := "user_5h_exceeded"
	if errors.Is(billingErr, service.ErrUserRateLimit7dExceeded) {
		reason = "user_7d_exceeded"
	}
	logger.LegacyPrintf(
		"handler.billing_fallback",
		"user_rate_limit_fallback engaged: user_id=%d platform=%s original_model=%s fallback_model=%s reason=%s billing=requested",
		userID, platform, reqModel, fb, reason,
	)
	return rewritten, true, nil
}
