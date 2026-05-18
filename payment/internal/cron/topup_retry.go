package cron

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/repository"
	"github.com/songquanpeng/one-api/payment/internal/service"
)

// TopupRetry handles the case where a webhook arrived, we marked the order
// `paid`, but the subsequent one-api credit call failed (network blip,
// one-api restart, etc). The webhook side intentionally does NOT keep
// retrying inline - it leaves the order in `paid` and returns. This cron
// retries from outside the request lifecycle.
//
// The retry runs through WebhookService.CreditOrder, which is itself
// idempotent (topup_callbacks UNIQUE + one-api UNIQUE), so retries are safe.
//
// MaxAge caps how long we keep retrying. After that an operator must
// intervene via /admin/orders/.../manual-credit (PR #3).
type TopupRetry struct {
	orderRepo      *repository.OrderRepo
	webhookService *service.WebhookService
	interval       time.Duration
	maxAge         time.Duration
	batchSize      int
}

func NewTopupRetry(
	orderRepo *repository.OrderRepo,
	webhookService *service.WebhookService,
	interval, maxAge time.Duration,
) *TopupRetry {
	return &TopupRetry{
		orderRepo:      orderRepo,
		webhookService: webhookService,
		interval:       interval,
		maxAge:         maxAge,
		batchSize:      200,
	}
}

// Start runs the retry loop until ctx is canceled.
func (c *TopupRetry) Start(ctx context.Context) {
	logger.Sys().Info("cron: TopupRetry started",
		zap.Duration("interval", c.interval),
		zap.Duration("max_age", c.maxAge))
	t := time.NewTicker(c.interval)
	defer t.Stop()
	c.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			logger.Sys().Info("cron: TopupRetry stopping")
			return
		case <-t.C:
			c.sweepOnce(ctx)
		}
	}
}

func (c *TopupRetry) sweepOnce(ctx context.Context) {
	// Only look at orders that have been paid for at least 30 seconds (give
	// the inline credit a chance to finish first); cap retries at maxAge.
	now := time.Now().UTC()
	olderThan := now.Add(-30 * time.Second)

	rows, err := c.orderRepo.FindUncreditedPaid(ctx, olderThan, c.batchSize)
	if err != nil {
		logger.L(ctx).Error("TopupRetry find failed", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		return
	}

	logger.L(ctx).Info("TopupRetry sweep", zap.Int("found", len(rows)))
	for i := range rows {
		row := rows[i]
		if row.PaidAt != nil && now.Sub(*row.PaidAt) > c.maxAge {
			logger.L(ctx).Warn("TopupRetry max_age exceeded; manual intervention required",
				zap.String("order_no", row.OrderNo),
				zap.Duration("age", now.Sub(*row.PaidAt)))
			// Don't auto-escalate here; the metric / log line is the signal.
			continue
		}
		if err := c.webhookService.CreditOrder(ctx, &row); err != nil {
			logger.L(ctx).Warn("TopupRetry credit attempt failed",
				zap.String("order_no", row.OrderNo), zap.Error(err))
		}
	}
}
