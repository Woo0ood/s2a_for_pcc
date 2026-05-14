package handler

import (
	"context"
	"errors"

	"github.com/Woo0ood/s2a_for_pcc/internal/pkg/logger"
	"github.com/Woo0ood/s2a_for_pcc/internal/service"
)

// applyUserRateLimitFallback inspects the result of BillingCacheService.CheckBillingEligibility.
// When the error is a user-level 5h/7d rate-limit and a platform-level fallback model is
// configured (reusing the existing EnableModelFallback + FallbackModel{Platform} settings),
// it rewrites the request body's `model` field to the fallback model and clears the error,
// letting the request proceed via the normal upstream-routing path.
//
// Returns:
//   - newBody:        rewritten body when fallback engaged; original body otherwise.
//   - effectiveModel: fallback model name when engaged; original reqModel otherwise.
//   - finalErr:       nil when fallback engaged or original billingErr was nil;
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
) (newBody []byte, effectiveModel string, finalErr error) {
	if billingErr == nil {
		return body, reqModel, nil
	}
	if settingService == nil {
		return body, reqModel, billingErr
	}
	if !errors.Is(billingErr, service.ErrUserRateLimit5hExceeded) && !errors.Is(billingErr, service.ErrUserRateLimit7dExceeded) {
		return body, reqModel, billingErr
	}
	if !settingService.IsModelFallbackEnabled(ctx) {
		return body, reqModel, billingErr
	}
	fb := settingService.GetFallbackModel(ctx, platform)
	if fb == "" || fb == reqModel {
		return body, reqModel, billingErr
	}

	rewritten := service.ReplaceModelInBody(body, fb)
	reason := "user_5h_exceeded"
	if errors.Is(billingErr, service.ErrUserRateLimit7dExceeded) {
		reason = "user_7d_exceeded"
	}
	logger.LegacyPrintf(
		"handler.billing_fallback",
		"user_rate_limit_fallback engaged: user_id=%d platform=%s original_model=%s fallback_model=%s reason=%s",
		userID, platform, reqModel, fb, reason,
	)
	return rewritten, fb, nil
}
