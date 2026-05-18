package model

import (
	"errors"

	"gorm.io/gorm"
)

// InternalTopupRecord stores one row per (order_no, action) processed by the
// internal payment endpoint. The UNIQUE(order_no, action) index gives the
// one-api side its own line of defense against duplicate webhook deliveries
// from the payment-service, even if the payment-service's own idempotency
// store is briefly inconsistent (e.g. after a crash between INSERT and the
// outgoing HTTP call).
type InternalTopupRecord struct {
	Id         int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	OrderNo    string `json:"order_no" gorm:"type:varchar(64);not null;uniqueIndex:idx_internal_topup_order_action,priority:1"`
	Action     string `json:"action" gorm:"type:varchar(24);not null;uniqueIndex:idx_internal_topup_order_action,priority:2"`
	UserId     int    `json:"user_id" gorm:"index;not null"`
	Quota      int64  `json:"quota" gorm:"bigint;not null"`
	NewBalance int64  `json:"new_balance" gorm:"bigint;not null"`
	Remark     string `json:"remark" gorm:"type:varchar(255);default:''"`
	CreatedAt  int64  `json:"created_at" gorm:"bigint;not null;index"`
}

const (
	InternalTopupActionTopup  = "topup"
	InternalTopupActionRefund = "refund"
)

// GetInternalTopupRecord returns the existing record for an (order_no, action)
// pair, or (nil, gorm.ErrRecordNotFound) if none exists.
//
// Implementation note: uses Limit(1).Find rather than First, because First
// logs "record not found" via gorm's default logger and the not-found case
// is the COMMON case here (most internal_topup requests are fresh).
func GetInternalTopupRecord(orderNo, action string) (*InternalTopupRecord, error) {
	if orderNo == "" || action == "" {
		return nil, errors.New("order_no and action are required")
	}
	var rec InternalTopupRecord
	result := DB.Where("order_no = ? AND action = ?", orderNo, action).Limit(1).Find(&rec)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &rec, nil
}

// IsRecordNotFound is a small wrapper so callers don't have to import gorm.
func IsRecordNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
