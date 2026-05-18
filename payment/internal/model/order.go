package model

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// OrderStatus enumerates every state an order can be in. The state machine
// transition table lives in order_service.go.
type OrderStatus string

const (
	StatusPending           OrderStatus = "pending"
	StatusPaid              OrderStatus = "paid"
	StatusCredited          OrderStatus = "credited"
	StatusExpired           OrderStatus = "expired"
	StatusCanceled          OrderStatus = "canceled"
	StatusFailed            OrderStatus = "failed"
	StatusNeedsManualReview OrderStatus = "needs_manual_review"
	StatusRefunding         OrderStatus = "refunding"
	StatusRefunded          OrderStatus = "refunded"
)

// PaymentMethod enumerates Xendit payment products this service can create.
// Extension hook: add a new constant + add a handler in xendit/ to support
// more (GoPay, OVO, DANA, etc.).
type PaymentMethod string

const (
	MethodQRIS  PaymentMethod = "qris"
	MethodVABCA PaymentMethod = "va_bca"
)

// Order is the canonical record of a single top-up attempt. Once created its
// quota_to_credit / exchange_rate fields are immutable; only status, the
// Xendit references, timestamps, and metadata may change.
//
// The unique index on order_no makes order_no the global idempotency key.
type Order struct {
	Id                   int64           `gorm:"primaryKey;autoIncrement" json:"id"`
	OrderNo              string          `gorm:"type:varchar(32);not null;uniqueIndex:uk_orders_order_no" json:"order_no"`
	UserId               int             `gorm:"not null;index:idx_orders_user_created,priority:1" json:"user_id"`
	AmountIDR            int64           `gorm:"not null" json:"amount_idr"`
	FeeIDR               int64           `gorm:"default:0" json:"fee_idr"`
	NetIDR               int64           `gorm:"default:0" json:"net_idr"`
	ExchangeRate         decimal.Decimal `gorm:"type:decimal(20,8);not null" json:"exchange_rate"`
	QuotaToCredit        int64           `gorm:"not null" json:"quota_to_credit"`
	QuotaCredited        int64           `gorm:"default:0" json:"quota_credited"`
	PaymentMethod        PaymentMethod   `gorm:"type:varchar(32);not null" json:"payment_method"`
	XenditInvoiceId      string          `gorm:"type:varchar(64);index:idx_orders_xendit_invoice" json:"xendit_invoice_id"`
	XenditPaymentId      string          `gorm:"type:varchar(64);index:idx_orders_xendit_payment" json:"xendit_payment_id"`
	XenditPaymentChannel string          `gorm:"type:varchar(32)" json:"xendit_payment_channel"`
	CheckoutURL          string          `gorm:"type:text" json:"checkout_url"`
	QRString             string          `gorm:"type:text" json:"qr_string"`
	VANumber             string          `gorm:"type:varchar(32)" json:"va_number"`
	Status               OrderStatus     `gorm:"type:varchar(24);not null;index:idx_orders_status_expires,priority:1" json:"status"`
	FailureReason        string          `gorm:"type:varchar(128)" json:"failure_reason"`
	CreatedAt            time.Time       `gorm:"type:datetime;not null;index:idx_orders_user_created,priority:2,sort:desc" json:"created_at"`
	ExpiresAt            time.Time       `gorm:"type:datetime;not null;index:idx_orders_status_expires,priority:2" json:"expires_at"`
	PaidAt               *time.Time      `gorm:"type:datetime" json:"paid_at,omitempty"`
	CreditedAt           *time.Time      `gorm:"type:datetime" json:"credited_at,omitempty"`
	ClientIP             string          `gorm:"type:varchar(45)" json:"client_ip"`
	UserAgent            string          `gorm:"type:varchar(255)" json:"user_agent"`
	MetadataJSON         string          `gorm:"type:text" json:"metadata_json"`
	UpdatedAt            time.Time       `gorm:"type:datetime;not null" json:"updated_at"`
}

// TableName pins the table name (GORM otherwise infers "orders" too, but be
// explicit so a future rename here doesn't silently shift migrations).
func (Order) TableName() string { return "orders" }

// IsTerminal returns true if the order can no longer transition under the
// normal payment flow (refund flow may still move credited -> refunded).
func (o *Order) IsTerminal() bool {
	switch o.Status {
	case StatusCredited, StatusExpired, StatusCanceled, StatusFailed,
		StatusRefunded, StatusNeedsManualReview:
		return true
	}
	return false
}

// IsPostPayTerminal returns true if the order has finalized in a way that
// MUST NOT auto-credit on a late webhook (corrections 1 and 2 of the spec).
//
// These statuses mean either:
//   - the system already declared the order dead (expired / canceled), OR
//   - a human is in the loop (needs_manual_review / refunding / refunded), OR
//   - Xendit told us the payment is definitively bad (failed).
//
// On a late paid-webhook against any of these, we MUST escalate to manual
// review and NEVER credit.
func (o *Order) IsPostPayTerminal() bool {
	switch o.Status {
	case StatusExpired, StatusCanceled, StatusFailed,
		StatusNeedsManualReview, StatusRefunding, StatusRefunded:
		return true
	}
	return false
}

// CanTransition checks the order state machine. Returns nil if the transition
// is legal, an explanatory error otherwise.
//
// Allowed transitions (PR #2 scope):
//
//	pending  -> paid, expired, canceled, failed, needs_manual_review
//	paid     -> credited, needs_manual_review
//	credited -> refunding   (PR #3)
//	expired  -> needs_manual_review        (late paid webhook)
//	canceled -> needs_manual_review        (late paid webhook)
//	failed   -> needs_manual_review        (late paid webhook)
//	refunding-> refunded, needs_manual_review  (PR #3)
func (o *Order) CanTransition(to OrderStatus) error {
	allowed := transitionTable[o.Status]
	for _, candidate := range allowed {
		if candidate == to {
			return nil
		}
	}
	return errors.New("illegal transition: " + string(o.Status) + " -> " + string(to))
}

var transitionTable = map[OrderStatus][]OrderStatus{
	StatusPending: {
		StatusPaid, StatusExpired, StatusCanceled, StatusFailed, StatusNeedsManualReview,
	},
	StatusPaid: {
		StatusCredited, StatusNeedsManualReview,
	},
	StatusCredited: {
		StatusRefunding, // PR #3
	},
	StatusExpired: {
		StatusNeedsManualReview,
	},
	StatusCanceled: {
		StatusNeedsManualReview,
	},
	StatusFailed: {
		StatusNeedsManualReview,
	},
	StatusRefunding: {
		StatusRefunded, StatusNeedsManualReview,
	},
	StatusRefunded:          {},
	StatusNeedsManualReview: {StatusCredited, StatusRefunded}, // manual admin actions, PR #3
}
