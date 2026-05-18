package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/service"
)

// WebhookHandler ingests Xendit webhook deliveries. The middleware chain
// upstream of these handlers has already enforced IP whitelist + token
// auth, so by the time the handler runs we trust the source.
//
// Each product (QRIS, VA, Invoice) has its own parser - the wire formats
// differ - then all converge on WebhookService.Process.
type WebhookHandler struct {
	WebhookService *service.WebhookService
}

func NewWebhookHandler(s *service.WebhookService) *WebhookHandler {
	return &WebhookHandler{WebhookService: s}
}

// QRIS handles POST /webhooks/xendit/qris.
//
// Expected payload (Xendit QR Code "qr.payment" webhook):
//
//	{
//	  "id": "qrpy_xxx",               // resource id of the payment event
//	  "qr_id": "qr_xxx",              // id of the QR object
//	  "currency": "IDR",
//	  "amount": 50000,
//	  "status": "SUCCEEDED",
//	  "created": "...",
//	  "reference_id": "IDR2024..."    // OUR order_no
//	}
func (h *WebhookHandler) QRIS(c *gin.Context) {
	rawBody := mustRead(c)
	var p struct {
		Id          string `json:"id"`
		QRId        string `json:"qr_id"`
		Currency    string `json:"currency"`
		Amount      int64  `json:"amount"`
		Status      string `json:"status"`
		ReferenceId string `json:"reference_id"`
	}
	if err := json.Unmarshal(rawBody, &p); err != nil {
		writeWebhookReply(c, http.StatusBadRequest, "bad_payload", err.Error())
		return
	}
	if p.Id == "" {
		writeWebhookReply(c, http.StatusBadRequest, "bad_payload", "missing payment id")
		return
	}

	out := h.WebhookService.Process(c.Request.Context(), service.NormalizedWebhook{
		EventType:        "qr.payment",
		XenditResourceId: p.Id,
		XenditPaymentId:  p.Id,
		OrderNo:          p.ReferenceId,
		Status:           strings.ToUpper(p.Status),
		PaidAmount:       p.Amount,
		Currency:         p.Currency,
		RawPayload:       rawBody,
		Signature:        c.GetHeader("X-Callback-Token"),
		SourceIP:         clientSourceIP(c),
	})
	replyFromOutcome(c, out)
}

// VA handles POST /webhooks/xendit/va.
//
// Expected payload (Xendit Virtual Account "virtual_account.payment" webhook):
//
//	{
//	  "id": "5f3xxx",                 // payment id (this delivery)
//	  "external_id": "IDR2024...",    // OUR order_no
//	  "amount": 50000,
//	  "currency": "IDR",
//	  "callback_virtual_account_id": "va_xxx",
//	  "bank_code": "BCA",
//	  "status": "COMPLETED",          // or "PAID"
//	  "transaction_timestamp": "..."
//	}
func (h *WebhookHandler) VA(c *gin.Context) {
	rawBody := mustRead(c)
	var p struct {
		Id          string `json:"id"`
		ExternalId  string `json:"external_id"`
		Amount      int64  `json:"amount"`
		Currency    string `json:"currency"`
		VAId        string `json:"callback_virtual_account_id"`
		BankCode    string `json:"bank_code"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal(rawBody, &p); err != nil {
		writeWebhookReply(c, http.StatusBadRequest, "bad_payload", err.Error())
		return
	}
	if p.Id == "" {
		writeWebhookReply(c, http.StatusBadRequest, "bad_payload", "missing payment id")
		return
	}
	// VA payment notifications may not include `status` at all - Xendit's
	// older endpoints just send the event because it happened. Treat the
	// absence of a status as "paid".
	status := strings.ToUpper(p.Status)
	if status == "" {
		status = "PAID"
	}

	out := h.WebhookService.Process(c.Request.Context(), service.NormalizedWebhook{
		EventType:        "virtual_account.payment",
		XenditResourceId: p.Id,
		XenditPaymentId:  p.Id,
		OrderNo:          p.ExternalId,
		Status:           status,
		PaidAmount:       p.Amount,
		Currency:         p.Currency,
		RawPayload:       rawBody,
		Signature:        c.GetHeader("X-Callback-Token"),
		SourceIP:         clientSourceIP(c),
	})
	replyFromOutcome(c, out)
}

// Invoice handles POST /webhooks/xendit/invoice.
//
// PR #2 does not create Invoice resources directly, but Xendit emits this
// event for some payment products and we should at least record/dedupe it.
func (h *WebhookHandler) Invoice(c *gin.Context) {
	rawBody := mustRead(c)
	var p struct {
		Id         string `json:"id"`
		ExternalId string `json:"external_id"`
		Amount     int64  `json:"amount"`
		Currency   string `json:"currency"`
		Status     string `json:"status"`
		PaidAt     string `json:"paid_at"`
	}
	if err := json.Unmarshal(rawBody, &p); err != nil {
		writeWebhookReply(c, http.StatusBadRequest, "bad_payload", err.Error())
		return
	}
	if p.Id == "" {
		writeWebhookReply(c, http.StatusBadRequest, "bad_payload", "missing invoice id")
		return
	}
	out := h.WebhookService.Process(c.Request.Context(), service.NormalizedWebhook{
		EventType:        "invoice.paid",
		XenditResourceId: p.Id,
		XenditPaymentId:  p.Id,
		OrderNo:          p.ExternalId,
		Status:           strings.ToUpper(p.Status),
		PaidAmount:       p.Amount,
		Currency:         p.Currency,
		RawPayload:       rawBody,
		Signature:        c.GetHeader("X-Callback-Token"),
		SourceIP:         clientSourceIP(c),
	})
	replyFromOutcome(c, out)
}

// mustRead drains the body. Returns nil on read failure (caller logs).
func mustRead(c *gin.Context) []byte {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.L(c.Request.Context()).Error("webhook body read failed", zap.Error(err))
		return nil
	}
	return body
}

// clientSourceIP prefers the value stamped by xendit_ip_whitelist; falls
// back to gin's resolution.
func clientSourceIP(c *gin.Context) string {
	if v, ok := c.Get("payment_source_ip"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return c.ClientIP()
}

// writeWebhookReply formats a Xendit-friendly response. We deliberately
// 200-OK most outcomes so Xendit doesn't redeliver an event we've already
// recorded. Genuine 5xx-causing situations still 500 so Xendit retries.
func writeWebhookReply(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": status < 400, "code": code, "message": msg})
}

func replyFromOutcome(c *gin.Context, out service.ProcessOutcome) {
	status := http.StatusOK
	switch out.Result {
	case model.WebhookResultError:
		// 500 -> Xendit retries. Use this for transient errors only
		// (DB write failed). Validation errors should be 400.
		status = http.StatusInternalServerError
	}
	writeWebhookReply(c, status, out.Code, out.Message)
}
