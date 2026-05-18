package middleware

import (
	"crypto/subtle"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/songquanpeng/one-api/payment/internal/errors"
	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/repository"
)

// XenditTokenAuth verifies the X-Callback-Token header against the
// payment_config row for THIS webhook URL. Each webhook URL ships with its
// own token (correction 9 - tokens live in DB, not env vars):
//
//	configKey   webhook URL
//	-----------------------------------------------------
//	xendit_webhook_token_qris       /webhooks/xendit/qris
//	xendit_webhook_token_va         /webhooks/xendit/va
//	xendit_webhook_token_invoice    /webhooks/xendit/invoice
//
// `configKey` is the parameter that pins this middleware to one URL.
//
// crypto/subtle.ConstantTimeCompare prevents the trivial timing oracle on
// the token.
func XenditTokenAuth(configRepo *repository.PaymentConfigRepo, configKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		expected, err := configRepo.Get(ctx, configKey)
		if err != nil {
			respondError(c, errors.ServiceUnavailable, "webhook config unavailable")
			return
		}
		if expected == "" {
			logger.L(ctx).Warn("webhook token not configured",
				zap.String("config_key", configKey),
				zap.String("path", c.Request.URL.Path))
			respondError(c, errors.ConfigError,
				"webhook token not set in payment_config: "+configKey)
			return
		}
		got := c.GetHeader("X-Callback-Token")
		if got == "" {
			respondError(c, errors.WebhookBadToken, "X-Callback-Token header missing")
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			logger.L(ctx).Warn("webhook token mismatch",
				zap.String("config_key", configKey),
				zap.String("path", c.Request.URL.Path))
			respondError(c, errors.WebhookBadToken, "X-Callback-Token does not match")
			return
		}
		c.Next()
	}
}
