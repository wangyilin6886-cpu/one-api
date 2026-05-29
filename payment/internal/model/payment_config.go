package model

import "time"

// PaymentConfig is the runtime-tunable key/value store. The admin UI mutates
// these rows; the service caches a snapshot in memory for 5 minutes (see
// repository.PaymentConfigRepo).
type PaymentConfig struct {
	ConfigKey   string    `gorm:"primaryKey;type:varchar(64)" json:"config_key"`
	ConfigValue string    `gorm:"type:text" json:"config_value"`
	Description string    `gorm:"type:varchar(255)" json:"description"`
	UpdatedAt   time.Time `gorm:"type:datetime" json:"updated_at"`
	UpdatedBy   int       `gorm:"default:0" json:"updated_by"`
}

func (PaymentConfig) TableName() string { return "payment_config" }

// Stable config keys. Use these constants instead of raw strings so a typo
// fails to compile.
//
// All money values are in USD cents (int64-friendly, no rounding pain).
// Defaults assume a global LLM top-up service.
const (
	// CfgMinTopupCents is the minimum allowed top-up in USD cents. Lower
	// values get eaten by Polar's flat fee ($0.50). Default: 500 = $5.
	CfgMinTopupCents = "min_topup_usd_cents"

	// CfgMaxTopupCents prevents single-shot card abuse. Default: 200_000 = $2000.
	CfgMaxTopupCents = "max_topup_usd_cents"

	// CfgOrderExpiryMinutes is how long an unpaid order lingers before the
	// cron sweeper marks it expired. Default 30. Polar checkout links
	// expire on their own ~24h, but our table needs its own clock too.
	CfgOrderExpiryMinutes = "order_expiry_minutes"

	// CfgPolarWebhookSecret is the HMAC secret Polar prints once when you
	// register the webhook endpoint. PR-B's signature verifier reads it.
	CfgPolarWebhookSecret = "polar_webhook_secret"

	// CfgPolarOrgId / CfgPolarTopupProductId let admins swap which Polar
	// product is mapped to "one-shot top-up" without redeploying.
	CfgPolarOrgId          = "polar_organization_id"
	CfgPolarTopupProductId = "polar_topup_product_id"

	// CfgCheckoutSuccessURL / CfgCheckoutCancelURL are the public URLs
	// Polar redirects the customer to after they close the checkout page.
	CfgCheckoutSuccessURL = "checkout_success_url"
	CfgCheckoutCancelURL  = "checkout_cancel_url"

	// CfgAlertEmail receives needs_manual_review notifications.
	CfgAlertEmail = "alert_email"
)

// SeedRows returns the initial seed for the payment_config table.
// SeedIfMissing inserts only rows that don't yet exist; admins editing
// values are never overwritten.
//
// Sensitive values (secrets) are deliberately left blank - the operator
// must fill them in before the corresponding feature works. This is
// captured in the row's `description` so they're discoverable.
func SeedRows(now time.Time) []PaymentConfig {
	return []PaymentConfig{
		{ConfigKey: CfgMinTopupCents, ConfigValue: "500",
			Description: "Minimum top-up in USD cents. Default 500 = $5.00.", UpdatedAt: now},
		{ConfigKey: CfgMaxTopupCents, ConfigValue: "200000",
			Description: "Maximum top-up in USD cents. Default 200000 = $2000.00.", UpdatedAt: now},
		{ConfigKey: CfgOrderExpiryMinutes, ConfigValue: "30",
			Description: "Unpaid orders auto-expire after this many minutes.", UpdatedAt: now},

		{ConfigKey: CfgPolarWebhookSecret, ConfigValue: "",
			Description: "Polar webhook HMAC secret (shown once at endpoint creation). REQUIRED for signature verification.", UpdatedAt: now},
		{ConfigKey: CfgPolarOrgId, ConfigValue: "",
			Description: "Polar organization id (slug or UUID).", UpdatedAt: now},
		{ConfigKey: CfgPolarTopupProductId, ConfigValue: "",
			Description: "Polar product id mapped to the one-shot top-up SKU.", UpdatedAt: now},

		{ConfigKey: CfgCheckoutSuccessURL, ConfigValue: "",
			Description: "Public URL Polar redirects the user to after success. Empty = let Polar use its default.", UpdatedAt: now},
		{ConfigKey: CfgCheckoutCancelURL, ConfigValue: "",
			Description: "Public URL Polar redirects the user to on cancel.", UpdatedAt: now},

		{ConfigKey: CfgAlertEmail, ConfigValue: "",
			Description: "Email address for needs_manual_review and credit-failure alerts.", UpdatedAt: now},
	}
}
