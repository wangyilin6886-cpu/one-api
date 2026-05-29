package handler

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/service"
)

// WebhookHandler is the thin HTTP layer over WebhookService.Process. Unlike
// the previous Xendit version, there's only ONE inbound webhook URL now —
// Polar puts all event types on the same endpoint. The provider adapter
// distinguishes them at the normalizer step.
type WebhookHandler struct {
	WebhookService *service.WebhookService
}

func NewWebhookHandler(s *service.WebhookService) *WebhookHandler {
	return &WebhookHandler{WebhookService: s}
}

// Handle POST /webhooks/polar.
//
// All headers are forwarded to the provider adapter for signature
// verification (Polar uses the "webhook-signature" / "webhook-id" /
// "webhook-timestamp" headers per the Standard Webhooks spec).
func (h *WebhookHandler) Handle(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.L(c.Request.Context()).Error("webhook body read failed", zap.Error(err))
		writeWebhookReply(c, http.StatusBadRequest, "read_error", err.Error())
		return
	}

	// Copy all request headers (lowercase keys) so the provider verifier
	// gets exactly what Polar sent.
	headers := make(map[string]string, len(c.Request.Header))
	for k, vv := range c.Request.Header {
		if len(vv) > 0 {
			headers[lower(k)] = vv[0]
		}
	}

	out := h.WebhookService.Process(c.Request.Context(), service.IncomingWebhook{
		RawPayload: body,
		Headers:    headers,
		SourceIP:   c.ClientIP(),
	})
	replyFromOutcome(c, out)
}

func writeWebhookReply(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": status < 400, "code": code, "message": msg})
}

func replyFromOutcome(c *gin.Context, out service.ProcessOutcome) {
	status := http.StatusOK
	if out.Result == model.WebhookResultError && out.Code != "bad_signature" {
		// 5xx -> provider retries. Use for transient (DB) errors only;
		// signature errors stay 401 (provider should NOT retry).
		status = http.StatusInternalServerError
	}
	if out.Code == "bad_signature" {
		status = http.StatusUnauthorized
	}
	writeWebhookReply(c, status, out.Code, out.Message)
}

func lower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
