package service

import (
	"context"

	"github.com/songquanpeng/one-api/payment/internal/logger"
	"go.uber.org/zap"
)

// Alerter sends operational alerts when something needs human attention.
// PR-A ships an in-process logger-based stub; future PRs will plug in
// Telegram / email / PagerDuty by replacing the implementation.
//
// Until then: every alert lands in stdout as a structured zap WARN log,
// tagged "ALERT", so existing log aggregators can fan it out.
type Alerter interface {
	NeedsManualReview(ctx context.Context, orderNo, reason string, fields ...zap.Field)
	ProviderError(ctx context.Context, orderNo, action string, err error)
	OneAPIError(ctx context.Context, orderNo, action string, err error)
}

type LogAlerter struct{}

func NewLogAlerter() *LogAlerter { return &LogAlerter{} }

func (a *LogAlerter) NeedsManualReview(ctx context.Context, orderNo, reason string, fields ...zap.Field) {
	all := append([]zap.Field{
		zap.String("alert", "needs_manual_review"),
		zap.String("order_no", orderNo),
		zap.String("reason", reason),
	}, fields...)
	logger.L(ctx).Warn("ALERT: order escalated to needs_manual_review", all...)
}

func (a *LogAlerter) ProviderError(ctx context.Context, orderNo, action string, err error) {
	logger.L(ctx).Error("ALERT: payment provider call failed",
		zap.String("alert", "provider_error"),
		zap.String("order_no", orderNo),
		zap.String("action", action),
		zap.Error(err),
	)
}

func (a *LogAlerter) OneAPIError(ctx context.Context, orderNo, action string, err error) {
	logger.L(ctx).Error("ALERT: one-api call failed",
		zap.String("alert", "one_api_error"),
		zap.String("order_no", orderNo),
		zap.String("action", action),
		zap.Error(err),
	)
}
