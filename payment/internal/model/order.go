package model

import (
	"errors"
	"time"
)

// OrderStatus enumerates every state an order can be in. The state machine
// transition table lives at the bottom of this file.
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

// OrderType separates one-shot top-ups from subscription-cycle credits.
// PR-A introduces the field so the schema is stable for PR-C subscriptions.
type OrderType string

const (
	OrderTypeTopup             OrderType = "topup"              // user-initiated one-shot purchase
	OrderTypeSubscriptionCycle OrderType = "subscription_cycle" // automatic monthly credit (PR-C)
)

// Order is the canonical record of one purchase attempt (whether one-shot
// top-up or one cycle of a subscription).
//
// Money is stored as int64 USD cents to avoid floating-point pain. The
// `currency` column always carries the explicit currency code so a future
// rollout to EUR/GBP/JPY needs only a column-value change, not a schema
// migration.
//
// Once an order is created its amount_usd_cents / quota_to_credit are
// immutable. Only state, provider references, and timestamps may change.
type Order struct {
	Id      int64 `gorm:"primaryKey;autoIncrement" json:"id"`
	OrderNo string `gorm:"type:varchar(32);not null;uniqueIndex:uk_orders_order_no" json:"order_no"`
	UserId  int    `gorm:"not null;index:idx_orders_user_created,priority:1" json:"user_id"`

	// Type and (optional) subscription pointer.
	OrderType      OrderType `gorm:"type:varchar(24);not null;default:'topup'" json:"order_type"`
	SubscriptionId string    `gorm:"type:varchar(64);index:idx_orders_subscription" json:"subscription_id,omitempty"`

	// Money. Single-currency v1 ("USD").
	AmountUSDCents int64  `gorm:"not null" json:"amount_usd_cents"`
	Currency       string `gorm:"type:varchar(8);not null;default:'USD'" json:"currency"`
	QuotaToCredit  int64  `gorm:"not null" json:"quota_to_credit"`
	QuotaCredited  int64  `gorm:"default:0" json:"quota_credited"`

	// Provider linkage. `provider` is "polar" today; will be the discriminator
	// when we ever add a second MoR. Other ids let the support team trace one
	// of our order rows back to a Polar dashboard entry in two clicks.
	Provider           string `gorm:"type:varchar(32);not null" json:"provider"`
	ProviderCheckoutId string `gorm:"type:varchar(128);index:idx_orders_provider_checkout" json:"provider_checkout_id,omitempty"`
	ProviderPaymentId  string `gorm:"type:varchar(128);index:idx_orders_provider_payment" json:"provider_payment_id,omitempty"`
	CheckoutURL        string `gorm:"type:text" json:"checkout_url,omitempty"`

	// Lifecycle.
	Status        OrderStatus `gorm:"type:varchar(24);not null;index:idx_orders_status_expires,priority:1" json:"status"`
	FailureReason string      `gorm:"type:varchar(255)" json:"failure_reason,omitempty"`
	CreatedAt     time.Time   `gorm:"type:datetime;not null;index:idx_orders_user_created,priority:2,sort:desc" json:"created_at"`
	ExpiresAt     time.Time   `gorm:"type:datetime;not null;index:idx_orders_status_expires,priority:2" json:"expires_at"`
	PaidAt        *time.Time  `gorm:"type:datetime" json:"paid_at,omitempty"`
	CreditedAt    *time.Time  `gorm:"type:datetime" json:"credited_at,omitempty"`

	// Audit.
	ClientIP     string    `gorm:"type:varchar(45)" json:"client_ip,omitempty"`
	UserAgent    string    `gorm:"type:varchar(255)" json:"user_agent,omitempty"`
	MetadataJSON string    `gorm:"type:text" json:"metadata_json,omitempty"`
	UpdatedAt    time.Time `gorm:"type:datetime;not null" json:"updated_at"`
}

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
// MUST NOT auto-credit on a late webhook.
//
// Rationale: an `expired`/`canceled`/`failed` order has already been
// declared dead by the system. If money still shows up after that, we have
// either a race, a refund-then-recharge edge case, or a fraud signal. In
// every case, escalate to needs_manual_review and never auto-credit.
func (o *Order) IsPostPayTerminal() bool {
	switch o.Status {
	case StatusExpired, StatusCanceled, StatusFailed,
		StatusNeedsManualReview, StatusRefunding, StatusRefunded:
		return true
	}
	return false
}

// CanTransition checks the order state machine. Returns nil if the
// transition is legal, an explanatory error otherwise.
//
// Allowed edges:
//
//	pending  -> paid, expired, canceled, failed, needs_manual_review
//	paid     -> credited, needs_manual_review
//	credited -> refunding                                (PR-D)
//	expired  -> needs_manual_review                      (late webhook)
//	canceled -> needs_manual_review                      (late webhook)
//	failed   -> needs_manual_review                      (late webhook)
//	refunding-> refunded, needs_manual_review            (PR-D)
//	needs_manual_review -> credited, refunded            (admin manual action, PR-D)
func (o *Order) CanTransition(to OrderStatus) error {
	for _, candidate := range transitionTable[o.Status] {
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
		StatusRefunding, // PR-D
	},
	StatusExpired:           {StatusNeedsManualReview},
	StatusCanceled:          {StatusNeedsManualReview},
	StatusFailed:            {StatusNeedsManualReview},
	StatusRefunding:         {StatusRefunded, StatusNeedsManualReview}, // PR-D
	StatusRefunded:          {},
	StatusNeedsManualReview: {StatusCredited, StatusRefunded}, // admin actions (PR-D)
}
