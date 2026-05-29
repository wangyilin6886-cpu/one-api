package model

import "time"

// RefundType enumerates the three refund flavours.
// PR-A only declares the table; the actual refund flow lands in PR-D.
type RefundType string

const (
	RefundTypeFull    RefundType = "full_refund"
	RefundTypePartial RefundType = "partial_refund"
	RefundTypeService RefundType = "service_refund" // customer-service comp; doesn't deduct quota
)

// RefundStatus enumerates the lifecycle of a refund operation.
type RefundStatus string

const (
	RefundStatusPending          RefundStatus = "pending"
	RefundStatusQuotaDeducted    RefundStatus = "quota_deducted"
	RefundStatusProviderSubmitted RefundStatus = "provider_submitted"
	RefundStatusSucceeded        RefundStatus = "succeeded"
	RefundStatusFailed           RefundStatus = "failed"
	RefundStatusFailedProvider   RefundStatus = "failed_provider" // quota deducted, provider call failed
)

// Refund is the audit record for a single refund operation. PR-A migrates
// this table; the code that writes it lives in PR-D.
type Refund struct {
	Id               int64        `gorm:"primaryKey;autoIncrement" json:"id"`
	RefundNo         string       `gorm:"type:varchar(32);uniqueIndex:uk_refunds_refund_no" json:"refund_no"`
	OrderNo          string       `gorm:"type:varchar(32);not null;index:idx_refunds_order_no" json:"order_no"`
	RefundType       RefundType   `gorm:"type:varchar(24);not null" json:"refund_type"`
	AmountUSDCents   int64        `gorm:"not null" json:"amount_usd_cents"`
	Currency         string       `gorm:"type:varchar(8);not null;default:'USD'" json:"currency"`
	QuotaToDeduct    int64        `gorm:"not null" json:"quota_to_deduct"`
	Reason           string       `gorm:"type:varchar(255)" json:"reason"`
	OperatorId       int          `gorm:"not null" json:"operator_id"`
	Status           RefundStatus `gorm:"type:varchar(24);not null" json:"status"`
	ProviderRefundId string       `gorm:"type:varchar(128)" json:"provider_refund_id"`
	CreatedAt        time.Time    `gorm:"type:datetime;not null" json:"created_at"`
	CompletedAt      *time.Time   `gorm:"type:datetime" json:"completed_at,omitempty"`
}

func (Refund) TableName() string { return "refunds" }
