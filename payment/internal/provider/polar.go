// Polar (https://polar.sh) adapter. Polar is the Merchant of Record that
// invoices the customer, charges the card, handles VAT/sales tax globally,
// and sends webhooks when money has moved.
//
// Webhook auth: Polar signs payloads with HMAC-SHA256 using a per-endpoint
// secret. The signature comes in the `webhook-signature` header (Standard
// Webhooks spec, https://standardwebhooks.com).
//
// This file is the PR-A scaffold: full PaymentProvider implementation lands
// in PR-B. Methods here return ErrNotImplemented so dependency wiring still
// compiles and tests can swap in a fake.
package provider

import (
	"context"
	"errors"
)

// ErrNotImplemented is returned by stub methods. Replaced in PR-B.
var ErrNotImplemented = errors.New("polar: not implemented (PR-B)")

// PolarConfig holds the bootstrap config Polar needs.
type PolarConfig struct {
	// AccessToken: server-side API token from the Polar dashboard.
	// Settings -> Developers -> Access Tokens. Format: polar_oat_*
	AccessToken string

	// OrganizationId: the org slug or UUID you're billing under.
	OrganizationId string

	// ProductTopupId: the Polar product id mapped to "one-shot top-up".
	// PR-C will add ProductSubscription{Pro,Max}Id.
	ProductTopupId string

	// WebhookSecret: the HMAC key for verifying inbound webhooks. Polar
	// shows it once when you register the endpoint.
	WebhookSecret string

	// BaseURL defaults to https://api.polar.sh; can be overridden for
	// tests against a recording proxy.
	BaseURL string

	// SandboxMode: when true, talk to https://sandbox-api.polar.sh.
	SandboxMode bool
}

// PolarProvider is the PaymentProvider implementation. PR-B fills in the
// methods; PR-A just registers the type so wiring can complete.
type PolarProvider struct {
	cfg PolarConfig
}

// NewPolar constructs the adapter. Validation of `cfg` happens here (rather
// than per-call) so a misconfigured deployment fails on startup, not on
// first checkout.
func NewPolar(cfg PolarConfig) (*PolarProvider, error) {
	if cfg.AccessToken == "" {
		return nil, errors.New("polar: AccessToken is required")
	}
	if cfg.OrganizationId == "" {
		return nil, errors.New("polar: OrganizationId is required")
	}
	if cfg.WebhookSecret == "" {
		return nil, errors.New("polar: WebhookSecret is required")
	}
	if cfg.BaseURL == "" {
		if cfg.SandboxMode {
			cfg.BaseURL = "https://sandbox-api.polar.sh"
		} else {
			cfg.BaseURL = "https://api.polar.sh"
		}
	}
	return &PolarProvider{cfg: cfg}, nil
}

// Name implements PaymentProvider.
func (p *PolarProvider) Name() string { return "polar" }

// CreateCheckout implements PaymentProvider. PR-B will implement the real
// POST /v1/checkouts call. For now we return ErrNotImplemented so any
// caller during PR-A integration sees a clear failure mode.
func (p *PolarProvider) CreateCheckout(ctx context.Context, params CheckoutParams) (*CheckoutResult, error) {
	return nil, ErrNotImplemented
}

// VerifyWebhook implements PaymentProvider. PR-B implements signature
// verification + event normalization. The stub returns ErrNotImplemented.
func (p *PolarProvider) VerifyWebhook(ctx context.Context, body []byte, headers map[string]string) (*NormalizedEvent, error) {
	return nil, ErrNotImplemented
}
