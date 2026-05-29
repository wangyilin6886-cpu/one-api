package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/model"
)

// WebhookRepo wraps the webhook_events table.
type WebhookRepo struct {
	db *gorm.DB
}

func NewWebhookRepo(db *gorm.DB) *WebhookRepo { return &WebhookRepo{db: db} }

// ErrDuplicateWebhook is returned by Insert when (event_type, provider_resource_id)
// already exists - meaning this is a retransmission from the provider.
var ErrDuplicateWebhook = errors.New("duplicate webhook event")

// Insert stores a fresh webhook arrival. Returns ErrDuplicateWebhook on a
// composite-key collision, which the caller should treat as a successful
// idempotent replay (HTTP 200 OK back to the provider).
func (r *WebhookRepo) Insert(ctx context.Context, e *model.WebhookEvent) error {
	if err := r.db.WithContext(ctx).Create(e).Error; err != nil {
		if IsDuplicateKeyError(err) {
			return ErrDuplicateWebhook
		}
		return err
	}
	return nil
}

// MarkProcessed sets processed_at and process_result on the row.
func (r *WebhookRepo) MarkProcessed(
	ctx context.Context, id int64, result model.WebhookEventProcessResult, errMsg string,
) error {
	now := time.Now().UTC()
	patch := map[string]any{
		"processed_at":   now,
		"process_result": result,
	}
	if errMsg != "" {
		patch["error_msg"] = errMsg
	}
	return r.db.WithContext(ctx).
		Model(&model.WebhookEvent{}).
		Where("id = ?", id).
		Updates(patch).Error
}

// GetByResource fetches a webhook by (event_type, provider_resource_id).
// Returns gorm.ErrRecordNotFound when missing.
func (r *WebhookRepo) GetByResource(
	ctx context.Context, eventType, resourceId string,
) (*model.WebhookEvent, error) {
	var e model.WebhookEvent
	result := r.db.WithContext(ctx).
		Where("event_type = ? AND provider_resource_id = ?", eventType, resourceId).
		Limit(1).Find(&e)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &e, nil
}
