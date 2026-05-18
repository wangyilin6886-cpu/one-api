package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/model"
)

// OrderRepo is the data access object for the orders table. All write methods
// run inside a transaction passed in by the service layer; lookup methods use
// the base DB so the caller picks the transaction boundary explicitly.
type OrderRepo struct {
	db *gorm.DB
}

func NewOrderRepo(db *gorm.DB) *OrderRepo { return &OrderRepo{db: db} }

// Create inserts a fresh order. Returns the unique-key error wrapped as
// ErrDuplicateOrderNo if order_no collides.
func (r *OrderRepo) Create(ctx context.Context, tx *gorm.DB, o *model.Order) error {
	conn := r.dbOr(tx)
	if err := conn.WithContext(ctx).Create(o).Error; err != nil {
		if IsDuplicateKeyError(err) {
			return ErrDuplicateOrderNo
		}
		return err
	}
	return nil
}

// GetByOrderNo fetches an order by its business order_no. Returns
// gorm.ErrRecordNotFound when no row exists.
func (r *OrderRepo) GetByOrderNo(ctx context.Context, orderNo string) (*model.Order, error) {
	var o model.Order
	result := r.db.WithContext(ctx).Where("order_no = ?", orderNo).Limit(1).Find(&o)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &o, nil
}

// LockByOrderNo runs SELECT ... FOR UPDATE inside the given transaction. Use
// from order_service whenever an order's status / quota_credited changes.
func (r *OrderRepo) LockByOrderNo(ctx context.Context, tx *gorm.DB, orderNo string) (*model.Order, error) {
	if tx == nil {
		return nil, errors.New("LockByOrderNo requires an explicit transaction")
	}
	var o model.Order
	err := tx.WithContext(ctx).
		Set("gorm:query_option", "FOR UPDATE").
		Where("order_no = ?", orderNo).
		First(&o).Error
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// UpdateStatusAndFields persists a status transition together with whatever
// other columns the caller wants to update. Pass `nil` for `fields` to update
// status alone. UpdatedAt is set to now in both cases.
func (r *OrderRepo) UpdateStatusAndFields(
	ctx context.Context, tx *gorm.DB, orderNo string,
	newStatus model.OrderStatus, fields map[string]any,
) error {
	patch := map[string]any{
		"status":     newStatus,
		"updated_at": time.Now().UTC(),
	}
	for k, v := range fields {
		patch[k] = v
	}
	conn := r.dbOr(tx)
	res := conn.WithContext(ctx).
		Model(&model.Order{}).
		Where("order_no = ?", orderNo).
		Updates(patch)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ListByUser returns orders for a user, newest first.
func (r *OrderRepo) ListByUser(ctx context.Context, userId int, limit, offset int) ([]model.Order, error) {
	var rows []model.Order
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userId).
		Order("created_at DESC").
		Limit(limit).Offset(offset).
		Find(&rows).Error
	return rows, err
}

// FindExpiredPending returns up to `batchSize` orders with status='pending'
// and expires_at < now. Used by the cron sweeper.
func (r *OrderRepo) FindExpiredPending(ctx context.Context, now time.Time, batchSize int) ([]model.Order, error) {
	var rows []model.Order
	err := r.db.WithContext(ctx).
		Where("status = ? AND expires_at < ?", model.StatusPending, now).
		Order("expires_at ASC").
		Limit(batchSize).
		Find(&rows).Error
	return rows, err
}

// FindUncreditedPaid returns paid orders that the topup callback hasn't
// finished crediting. Used by the retry cron.
func (r *OrderRepo) FindUncreditedPaid(ctx context.Context, olderThan time.Time, batchSize int) ([]model.Order, error) {
	var rows []model.Order
	err := r.db.WithContext(ctx).
		Where("status = ? AND paid_at < ?", model.StatusPaid, olderThan).
		Order("paid_at ASC").
		Limit(batchSize).
		Find(&rows).Error
	return rows, err
}

// AppendMetadata writes a JSON-encoded snippet into the metadata_json
// column. Existing content is kept verbatim with a "; " separator. We do
// NOT pretend metadata_json is a JSON array - it's an audit trail string.
func (r *OrderRepo) AppendMetadata(ctx context.Context, tx *gorm.DB, orderNo, snippet string) error {
	conn := r.dbOr(tx)
	// Use COALESCE so the first append doesn't produce a leading "; "
	return conn.WithContext(ctx).Exec(
		"UPDATE orders SET metadata_json = CASE WHEN metadata_json IS NULL OR metadata_json = '' "+
			"THEN ? ELSE CONCAT(metadata_json, ?, ?) END, updated_at = ? WHERE order_no = ?",
		snippet, " | ", snippet, time.Now().UTC(), orderNo,
	).Error
}

func (r *OrderRepo) dbOr(tx *gorm.DB) *gorm.DB {
	if tx != nil {
		return tx
	}
	return r.db
}

// ErrDuplicateOrderNo is returned by Create when order_no already exists.
var ErrDuplicateOrderNo = errors.New("duplicate order_no")

// IsDuplicateKeyError detects unique-constraint violations across drivers.
// GORM 1.25 doesn't translate driver errors by default, so we string-match.
func IsDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate entry") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate key value")
}
