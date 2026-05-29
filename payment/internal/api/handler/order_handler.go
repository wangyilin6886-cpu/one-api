// Package handler contains the Gin HTTP handlers for the payment-service.
// Handlers stay thin: parse + validate input, call the service layer,
// translate (service error -> typed error code -> HTTP response).
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/api/middleware"
	apierrors "github.com/songquanpeng/one-api/payment/internal/errors"
	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/service"
)

type OrderHandler struct {
	OrderService *service.OrderService
}

func NewOrderHandler(s *service.OrderService) *OrderHandler {
	return &OrderHandler{OrderService: s}
}

// CreateOrderRequest is the body for POST /orders.
type CreateOrderRequest struct {
	// AmountUSDCents: must be within the configured min/max range.
	// Frontend should send 500 for $5.00, 1900 for $19.00, etc.
	AmountUSDCents int64 `json:"amount_usd_cents"`

	// Currency: optional; default "USD". Forward-looking for multi-currency.
	Currency string `json:"currency,omitempty"`

	// CustomerEmail: optional override; if empty the middleware fills it
	// from one-api's /api/user/self response.
	CustomerEmail string `json:"customer_email,omitempty"`
}

// OrderResponse is the public DTO for an order. Internal fields like
// metadata_json and failure_reason intentionally don't leak to clients.
type OrderResponse struct {
	OrderNo            string `json:"order_no"`
	UserId             int    `json:"user_id"`
	OrderType          string `json:"order_type"`
	AmountUSDCents     int64  `json:"amount_usd_cents"`
	Currency           string `json:"currency"`
	QuotaToCredit      int64  `json:"quota_to_credit"`
	Provider           string `json:"provider"`
	ProviderCheckoutId string `json:"provider_checkout_id,omitempty"`
	CheckoutURL        string `json:"checkout_url,omitempty"`
	Status             string `json:"status"`
	CreatedAt          string `json:"created_at"`
	ExpiresAt          string `json:"expires_at"`
	PaidAt             string `json:"paid_at,omitempty"`
	CreditedAt         string `json:"credited_at,omitempty"`
}

func toOrderResponse(o *model.Order) OrderResponse {
	r := OrderResponse{
		OrderNo:            o.OrderNo,
		UserId:             o.UserId,
		OrderType:          string(o.OrderType),
		AmountUSDCents:     o.AmountUSDCents,
		Currency:           o.Currency,
		QuotaToCredit:      o.QuotaToCredit,
		Provider:           o.Provider,
		ProviderCheckoutId: o.ProviderCheckoutId,
		CheckoutURL:        o.CheckoutURL,
		Status:             string(o.Status),
		CreatedAt:          o.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		ExpiresAt:          o.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if o.PaidAt != nil {
		r.PaidAt = o.PaidAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if o.CreditedAt != nil {
		r.CreditedAt = o.CreditedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return r
}

// Create handles POST /orders.
func (h *OrderHandler) Create(c *gin.Context) {
	userId := middleware.UserId(c)
	if userId <= 0 {
		respondError(c, apierrors.Unauthorized, "user not authenticated")
		return
	}

	var req CreateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, apierrors.InvalidRequestBody, err.Error())
		return
	}
	if req.AmountUSDCents <= 0 {
		respondError(c, apierrors.InvalidParameter, "amount_usd_cents must be > 0")
		return
	}
	currency := req.Currency
	if currency == "" {
		currency = "USD"
	}

	email := req.CustomerEmail
	// Future: pull email from one-api /api/user/self response (added in user_auth).

	params := service.CreateOrderParams{
		UserId:         userId,
		AmountUSDCents: req.AmountUSDCents,
		Currency:       currency,
		CustomerEmail:  email,
		ClientIP:       c.ClientIP(),
		UserAgent:      c.Request.UserAgent(),
	}

	ctx := logger.WithFields(c.Request.Context(),
		zap.Int("user_id", userId),
		zap.Int64("amount_cents", req.AmountUSDCents),
		zap.String("currency", currency),
	)

	order, err := h.OrderService.CreateOrder(ctx, params)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrAmountTooLow):
			respondError(c, apierrors.OrderAmountTooLow, "")
		case errors.Is(err, service.ErrAmountTooHigh):
			respondError(c, apierrors.OrderAmountTooHigh, "")
		case errors.Is(err, service.ErrUnsupportedCurrency):
			respondError(c, apierrors.UnsupportedCurrency, "")
		case errors.Is(err, service.ErrInvalidConfig):
			respondError(c, apierrors.ConfigError, err.Error())
		default:
			// Provider/DB failures: if the service produced an order row
			// before failing, return it so the user knows their order_no
			// for support, even if the row is in `failed` state.
			if order != nil {
				logger.L(ctx).Error("CreateOrder partial: order persisted but provider failed",
					zap.String("order_no", order.OrderNo), zap.Error(err))
				c.JSON(http.StatusBadGateway, gin.H{
					"success": false,
					"code":    apierrors.ProviderCallFailed,
					"message": "payment provider rejected the request",
					"detail":  err.Error(),
					"data":    toOrderResponse(order),
				})
				return
			}
			logger.L(ctx).Error("CreateOrder failed", zap.Error(err))
			respondError(c, apierrors.InternalError, err.Error())
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    toOrderResponse(order),
	})
}

// Get handles GET /orders/:order_no.
func (h *OrderHandler) Get(c *gin.Context) {
	userId := middleware.UserId(c)
	if userId <= 0 {
		respondError(c, apierrors.Unauthorized, "user not authenticated")
		return
	}
	orderNo := c.Param("order_no")
	if orderNo == "" {
		respondError(c, apierrors.InvalidParameter, "order_no required")
		return
	}
	o, err := h.OrderService.GetOrder(c.Request.Context(), orderNo)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respondError(c, apierrors.OrderNotFound, "")
			return
		}
		respondError(c, apierrors.DatabaseError, err.Error())
		return
	}
	if o.UserId != userId {
		respondError(c, apierrors.OrderForbidden, "")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    toOrderResponse(o),
	})
}

func respondError(c *gin.Context, code apierrors.Code, detail string) {
	lang := apierrors.PickLang(c.Request.Header.Get("Accept-Language"))
	c.AbortWithStatusJSON(code.HTTPStatus(), apierrors.Build(code, lang, detail))
}
