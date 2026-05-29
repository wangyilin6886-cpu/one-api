// Package provider defines the abstract payment-provider interface and the
// canonical normalized types every concrete implementation produces.
//
// One concrete provider per file under provider/: polar.go for Polar,
// paddle.go later if we ever switch, etc. The rest of the service speaks
// PROVIDER-NEUTRAL types - only the adapter knows about Polar's REST shape.
package provider

import (
	"context"
	"errors"
	"time"
)

// EventType enumerates the normalized webhook events we react to. Each
// concrete provider maps its native event names into one of these.
type EventType string

const (
	// EventCheckoutCompleted: a one-shot top-up has been paid for. The
	// service will credit the user's quota.
	EventCheckoutCompleted EventType = "checkout.completed"

	// EventCheckoutFailed: a checkout was abandoned, declined, or refunded
	// before fulfillment. Mark the order failed.
	EventCheckoutFailed EventType = "checkout.failed"

	// EventSubscriptionCreated: a new recurring subscription is now active.
	// PR-C territory; included in the type so the framework is ready.
	EventSubscriptionCreated EventType = "subscription.created"

	// EventSubscriptionRenewed: a billing cycle was paid. Credit the
	// monthly quota allotment. PR-C.
	EventSubscriptionRenewed EventType = "subscription.renewed"

	// EventSubscriptionCanceled: customer canceled; downgrade group at
	// current_period_end. PR-C.
	EventSubscriptionCanceled EventType = "subscription.canceled"

	// EventSubscriptionPastDue: payment failed, provider is retrying. PR-C.
	EventSubscriptionPastDue EventType = "subscription.past_due"

	// EventRefundIssued: a refund has been issued by the provider (either
	// from our admin action or from the customer's dispute). PR-D.
	EventRefundIssued EventType = "refund.issued"

	// EventIgnored: we recognize the shape but do not react. Used for
	// audit-only events Polar sends.
	EventIgnored EventType = "ignored"
)

// CheckoutParams is the input for Provider.CreateCheckout.
type CheckoutParams struct {
	// OrderNo is OUR local order id; the provider should echo it back in
	// webhooks so we can join the rows.
	OrderNo string

	// UserId from one-api; persisted in metadata so the webhook side can
	// route the credit without a DB lookup.
	UserId int

	// AmountUSDCents is the unit price in US cents. Single-currency for v1.
	AmountUSDCents int64

	// Currency is always "USD" in v1; the field exists so a future
	// multi-currency rollout doesn't change the signature.
	Currency string

	// CustomerEmail is forwarded so the provider can show it on the
	// checkout page and pre-fill the receipt.
	CustomerEmail string

	// SuccessURL / CancelURL are where the provider redirects after the
	// hosted checkout closes.
	SuccessURL string
	CancelURL  string

	// IdempotencyKey: if non-empty, MUST be forwarded in the provider's
	// idempotency header. Same key for retries; new key for retries with
	// different intent.
	IdempotencyKey string
}

// CheckoutResult is the output of Provider.CreateCheckout.
type CheckoutResult struct {
	// CheckoutURL is the hosted-checkout URL the user is redirected to.
	CheckoutURL string

	// ProviderCheckoutId is the provider's own id for this checkout
	// session (Polar: "checkout.id"; Paddle: "txn_..."). Stored on the
	// order row for support and reconciliation.
	ProviderCheckoutId string

	// ExpiresAt: when the hosted checkout link stops being valid.
	ExpiresAt time.Time
}

// NormalizedEvent is the result of Provider.VerifyWebhook. The webhook
// service only ever sees this; it never touches the provider's raw JSON.
type NormalizedEvent struct {
	Type EventType

	// ProviderResourceId is THE unique id for THIS delivery. Used as one
	// half of the (event_type, provider_resource_id) UNIQUE for dedupe.
	// Different events for the same checkout will have different ids.
	ProviderResourceId string

	// OrderNo is OUR order id, pulled from the provider's metadata field.
	// Empty when the event has no associated local order (rare; logged).
	OrderNo string

	// UserId may be present in metadata; 0 when unknown.
	UserId int

	// AmountUSDCents echoes the paid amount. Cross-checked against the
	// order's expected amount in the webhook service.
	AmountUSDCents int64

	// Currency is "USD" in v1; non-USD currencies trigger needs_manual_review.
	Currency string

	// PaidAt is when the provider reports the payment settled. Used as
	// orders.paid_at.
	PaidAt time.Time

	// ProviderPaymentId is the provider's own id for the underlying
	// payment object (the transaction, not the webhook delivery).
	ProviderPaymentId string

	// SubscriptionId is set for subscription events; empty for one-shot.
	SubscriptionId string
}

// PaymentProvider is what order_service / webhook_service depend on. The
// only knowledge they need about Polar (or any future provider) lives here.
type PaymentProvider interface {
	// Name returns a stable short identifier, e.g. "polar". Used in logs
	// and stored on order rows so multi-provider support later is trivial.
	Name() string

	// CreateCheckout creates a hosted checkout session.
	CreateCheckout(ctx context.Context, p CheckoutParams) (*CheckoutResult, error)

	// VerifyWebhook verifies the signature in `headers` against `body` and
	// returns the normalized event. Returns ErrInvalidSignature when the
	// signature does not match; returns ErrUnrecognizedEvent when the
	// payload is well-formed but not a type we handle.
	VerifyWebhook(ctx context.Context, body []byte, headers map[string]string) (*NormalizedEvent, error)
}

// Errors common to all providers. The service layer pattern-matches on these.
var (
	ErrInvalidSignature   = errors.New("provider: invalid webhook signature")
	ErrUnrecognizedEvent  = errors.New("provider: unrecognized event type")
	ErrMissingOrderNo     = errors.New("provider: event has no order_no in metadata")
	ErrProviderUnavailable = errors.New("provider: upstream unavailable")
)
