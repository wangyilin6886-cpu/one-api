package model

import "time"

// TopupCallbackAction labels a single business intent we want to invoke on
// one-api. The (order_no, action_type) pair is the idempotency key.
type TopupCallbackAction string

const (
	TopupCallbackActionTopup      TopupCallbackAction = "topup"
	TopupCallbackActionRefund     TopupCallbackAction = "refund"     // PR #3
	TopupCallbackActionCompensate TopupCallbackAction = "compensate" // reserved
)

// TopupCallback tracks each outgoing call to one-api's /api/internal/topup.
// It is inserted BEFORE the HTTP call so that a payment-service crash + retry
// can detect "I already (tried to) do this" via the UNIQUE constraint and
// then check the row's succeeded_at to decide whether to re-try.
//
// Combined with one-api PR #1's own internal_topup_records table, this gives
// us two layers of defense against double crediting.
type TopupCallback struct {
	Id           int64               `gorm:"primaryKey;autoIncrement" json:"id"`
	OrderNo      string              `gorm:"type:varchar(32);not null;uniqueIndex:uk_topup_cb_order_action,priority:1" json:"order_no"`
	ActionType   TopupCallbackAction `gorm:"type:varchar(24);not null;uniqueIndex:uk_topup_cb_order_action,priority:2" json:"action_type"`
	UserId       int                 `gorm:"not null" json:"user_id"`
	Quota        int64               `gorm:"not null" json:"quota"`
	RequestBody  string              `gorm:"type:text" json:"request_body"`
	ResponseBody string              `gorm:"type:text" json:"response_body"`
	HTTPStatus   int                 `gorm:"column:http_status" json:"http_status"`
	Attempt      int                 `gorm:"default:1" json:"attempt"`
	SucceededAt  *time.Time          `gorm:"type:datetime" json:"succeeded_at,omitempty"`
	CreatedAt    time.Time           `gorm:"type:datetime;not null" json:"created_at"`
}

func (TopupCallback) TableName() string { return "topup_callbacks" }

// IsSuccess returns true iff the row has been marked as succeeded.
func (t *TopupCallback) IsSuccess() bool { return t.SucceededAt != nil }
