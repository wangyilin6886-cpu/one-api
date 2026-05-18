package model

import "time"

// RefundType enumerates the three refund flavours the spec lays out
// (correction 8). PR #2 only declares the table; the actual refund flow is
// implemented in PR #3.
type RefundType string

const (
	RefundTypeFull    RefundType = "full_refund"
	RefundTypePartial RefundType = "partial_refund"
	RefundTypeService RefundType = "service_refund"
)

// RefundStatus enumerates the lifecycle of a refund operation.
type RefundStatus string

const (
	RefundStatusPending        RefundStatus = "pending"
	RefundStatusQuotaDeducted  RefundStatus = "quota_deducted"
	RefundStatusXenditSubmitted RefundStatus = "xendit_submitted"
	RefundStatusSucceeded      RefundStatus = "succeeded"
	RefundStatusFailed         RefundStatus = "failed"
	RefundStatusFailedXendit   RefundStatus = "failed_xendit" // quota already deducted, Xendit refund failed
)

// Refund is the audit record for a single refund operation. PR #2 only
// migrates this table - the actual code that writes it lives in PR #3.
type Refund struct {
	Id              int64        `gorm:"primaryKey;autoIncrement" json:"id"`
	RefundNo        string       `gorm:"type:varchar(32);uniqueIndex:uk_refunds_refund_no" json:"refund_no"`
	OrderNo         string       `gorm:"type:varchar(32);not null;index:idx_refunds_order_no" json:"order_no"`
	RefundType      RefundType   `gorm:"type:varchar(24);not null" json:"refund_type"`
	AmountIDR       int64        `gorm:"not null" json:"amount_idr"`
	QuotaToDeduct   int64        `gorm:"not null" json:"quota_to_deduct"`
	Reason          string       `gorm:"type:varchar(255)" json:"reason"`
	OperatorId      int          `gorm:"not null" json:"operator_id"`
	Status          RefundStatus `gorm:"type:varchar(24);not null" json:"status"`
	XenditRefundId  string       `gorm:"type:varchar(64)" json:"xendit_refund_id"`
	CreatedAt       time.Time    `gorm:"type:datetime;not null" json:"created_at"`
	CompletedAt     *time.Time   `gorm:"type:datetime" json:"completed_at,omitempty"`
}

func (Refund) TableName() string { return "refunds" }
