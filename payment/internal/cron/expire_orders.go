// Package cron runs background sweepers. PR #2 ships two:
//
//   - ExpireOrders: every minute, mark expired pending orders as `expired`.
//   - TopupRetry:   every 30s, re-call one-api for paid-but-not-credited orders.
package cron

import (
	"context"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/repository"
)

// ExpireOrders periodically sweeps pending orders whose expires_at has
// passed and marks them expired. This is the correction-3 cron.
//
// The transition is "pending -> expired" through the model state machine,
// so a webhook that arrives during the same minute might race us. That
// race is the EXACT case correction 1/2 (late-callback) is built to
// handle: whoever wins the SELECT FOR UPDATE proceeds, the loser sees the
// new status and escalates correctly.
type ExpireOrders struct {
	db        *gorm.DB
	orderRepo *repository.OrderRepo
	interval  time.Duration
	batchSize int
}

func NewExpireOrders(db *gorm.DB, orderRepo *repository.OrderRepo, interval time.Duration) *ExpireOrders {
	return &ExpireOrders{
		db:        db,
		orderRepo: orderRepo,
		interval:  interval,
		batchSize: 500,
	}
}

// Start runs the sweep loop until ctx is canceled. Returns when ctx is done.
func (c *ExpireOrders) Start(ctx context.Context) {
	logger.Sys().Info("cron: ExpireOrders started",
		zap.Duration("interval", c.interval))
	t := time.NewTicker(c.interval)
	defer t.Stop()
	c.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			logger.Sys().Info("cron: ExpireOrders stopping")
			return
		case <-t.C:
			c.sweepOnce(ctx)
		}
	}
}

func (c *ExpireOrders) sweepOnce(ctx context.Context) {
	now := time.Now().UTC()
	rows, err := c.orderRepo.FindExpiredPending(ctx, now, c.batchSize)
	if err != nil {
		logger.L(ctx).Error("ExpireOrders find failed", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		return
	}
	logger.L(ctx).Info("ExpireOrders sweep", zap.Int("found", len(rows)))

	for _, row := range rows {
		c.expireOne(ctx, row.OrderNo)
	}
}

func (c *ExpireOrders) expireOne(ctx context.Context, orderNo string) {
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		o, err := c.orderRepo.LockByOrderNo(ctx, tx, orderNo)
		if err != nil {
			return err
		}
		// Re-check under the lock: another goroutine / webhook may have
		// already moved this order out of pending.
		if o.Status != model.StatusPending {
			return nil
		}
		if time.Now().UTC().Before(o.ExpiresAt) {
			return nil
		}
		if err := o.CanTransition(model.StatusExpired); err != nil {
			return err
		}
		return c.orderRepo.UpdateStatusAndFields(ctx, tx, orderNo, model.StatusExpired,
			map[string]any{"failure_reason": "auto-expired by cron"})
	})
	if err != nil {
		logger.L(ctx).Warn("ExpireOrders single expire failed",
			zap.String("order_no", orderNo), zap.Error(err))
	}
}
