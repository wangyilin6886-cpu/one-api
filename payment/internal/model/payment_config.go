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

// Stable key names. Use these constants instead of raw strings so a typo
// fails to compile.
const (
	CfgExchangeRateIDRPerUSD  = "exchange_rate_idr_per_usd"
	CfgExchangeRateUpdatedAt  = "exchange_rate_updated_at"
	CfgMinTopupAmountIDR      = "min_topup_amount_idr"
	CfgMaxTopupAmountIDR      = "max_topup_amount_idr"
	CfgOrderExpiryMinutes     = "order_expiry_minutes"
	CfgXenditWebhookTokenInv  = "xendit_webhook_token_invoice"
	CfgXenditWebhookTokenVA   = "xendit_webhook_token_va"
	CfgXenditWebhookTokenQRIS = "xendit_webhook_token_qris"
	CfgXenditIPWhitelistJSON  = "xendit_ip_whitelist_json"
	CfgAlertEmail             = "alert_email"
)

// SeedRows returns the initial seed for the payment_config table. The
// migrate step calls this and inserts each row if not present (idempotent).
// Sensitive values are deliberately left blank - operators must fill them
// in via SQL or the admin UI before the corresponding feature works.
func SeedRows(now time.Time) []PaymentConfig {
	return []PaymentConfig{
		{ConfigKey: CfgExchangeRateIDRPerUSD, ConfigValue: "16500", Description: "Locked at order creation. IDR per 1 USD. Edit affects new orders only.", UpdatedAt: now},
		{ConfigKey: CfgExchangeRateUpdatedAt, ConfigValue: now.UTC().Format(time.RFC3339), Description: "When exchange_rate_idr_per_usd was last edited.", UpdatedAt: now},
		{ConfigKey: CfgMinTopupAmountIDR, ConfigValue: "10000", Description: "Minimum top-up amount in IDR (rupiah).", UpdatedAt: now},
		{ConfigKey: CfgMaxTopupAmountIDR, ConfigValue: "10000000", Description: "Maximum top-up amount in IDR (rupiah).", UpdatedAt: now},
		{ConfigKey: CfgOrderExpiryMinutes, ConfigValue: "30", Description: "Unpaid orders expire after this many minutes.", UpdatedAt: now},
		{ConfigKey: CfgXenditWebhookTokenInv, ConfigValue: "", Description: "x-callback-token for the Invoice webhook URL. Fill from Xendit dashboard.", UpdatedAt: now},
		{ConfigKey: CfgXenditWebhookTokenVA, ConfigValue: "", Description: "x-callback-token for the Virtual Account webhook URL.", UpdatedAt: now},
		{ConfigKey: CfgXenditWebhookTokenQRIS, ConfigValue: "", Description: "x-callback-token for the QRIS webhook URL.", UpdatedAt: now},
		{ConfigKey: CfgXenditIPWhitelistJSON, ConfigValue: `["0.0.0.0/0"]`, Description: "JSON array of CIDRs allowed to call /webhooks/xendit/*. DEFAULT IS PERMISSIVE - lock down before prod.", UpdatedAt: now},
		{ConfigKey: CfgAlertEmail, ConfigValue: "", Description: "Email address for needs_manual_review alerts.", UpdatedAt: now},
	}
}
