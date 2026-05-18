package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/model"
)

// TopupCallbackRepo wraps the topup_callbacks table.
type TopupCallbackRepo struct {
	db *gorm.DB
}

func NewTopupCallbackRepo(db *gorm.DB) *TopupCallbackRepo { return &TopupCallbackRepo{db: db} }

// ErrDuplicateTopupCallback signals a (order_no, action_type) collision -
// the action has already been attempted (and may or may not have succeeded).
var ErrDuplicateTopupCallback = errors.New("duplicate topup callback")

// InsertPending creates a new topup_callbacks row in "in flight" state. The
// service layer calls this BEFORE the HTTP call to one-api so that a crash
// between INSERT and the HTTP call still leaves a record we can reconcile.
func (r *TopupCallbackRepo) InsertPending(ctx context.Context, c *model.TopupCallback) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	if c.Attempt == 0 {
		c.Attempt = 1
	}
	if err := r.db.WithContext(ctx).Create(c).Error; err != nil {
		if IsDuplicateKeyError(err) {
			return ErrDuplicateTopupCallback
		}
		return err
	}
	return nil
}

// GetByOrderAction returns the existing callback row for (order_no, action),
// or gorm.ErrRecordNotFound.
func (r *TopupCallbackRepo) GetByOrderAction(
	ctx context.Context, orderNo string, action model.TopupCallbackAction,
) (*model.TopupCallback, error) {
	var c model.TopupCallback
	result := r.db.WithContext(ctx).
		Where("order_no = ? AND action_type = ?", orderNo, action).
		Limit(1).Find(&c)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &c, nil
}

// MarkSuccess updates a callback row after a successful one-api response.
func (r *TopupCallbackRepo) MarkSuccess(
	ctx context.Context, id int64, httpStatus int, responseBody string,
) error {
	now := time.Now().UTC()
	return r.db.WithContext(ctx).
		Model(&model.TopupCallback{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"http_status":   httpStatus,
			"response_body": responseBody,
			"succeeded_at":  now,
		}).Error
}

// MarkFailure updates a callback row after a failed one-api response. Bumps
// attempt count so a retry can identify how many times we've tried.
func (r *TopupCallbackRepo) MarkFailure(
	ctx context.Context, id int64, httpStatus int, responseBody string,
) error {
	return r.db.WithContext(ctx).
		Model(&model.TopupCallback{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"http_status":   httpStatus,
			"response_body": responseBody,
			"attempt":       gorm.Expr("attempt + 1"),
		}).Error
}
