// Package handler contains the Gin HTTP handlers for the payment-service.
// Handlers stay thin: they parse + validate input, call the service layer,
// and translate (service error -> typed error code -> HTTP response).
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	apierrors "github.com/songquanpeng/one-api/payment/internal/errors"
	"github.com/songquanpeng/one-api/payment/internal/api/middleware"
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
	AmountIDR     int64  `json:"amount_idr"`
	PaymentMethod string `json:"payment_method"`
	// PayerName is shown in BCA mobile when payment_method=va_bca. Falls
	// back to the authenticated user's display name.
	PayerName string `json:"payer_name,omitempty"`
}

// OrderResponse is the public DTO for an order (one field per spec column
// that the frontend cares about; we intentionally do NOT leak internal
// metadata_json or failure_reason).
type OrderResponse struct {
	OrderNo              string `json:"order_no"`
	UserId               int    `json:"user_id"`
	AmountIDR            int64  `json:"amount_idr"`
	QuotaToCredit        int64  `json:"quota_to_credit"`
	PaymentMethod        string `json:"payment_method"`
	Status               string `json:"status"`
	XenditPaymentId      string `json:"xendit_payment_id,omitempty"`
	XenditPaymentChannel string `json:"xendit_payment_channel,omitempty"`
	QRString             string `json:"qr_string,omitempty"`
	VANumber             string `json:"va_number,omitempty"`
	CheckoutURL          string `json:"checkout_url,omitempty"`
	CreatedAt            string `json:"created_at"`
	ExpiresAt            string `json:"expires_at"`
	PaidAt               string `json:"paid_at,omitempty"`
	CreditedAt           string `json:"credited_at,omitempty"`
}

func toOrderResponse(o *model.Order) OrderResponse {
	r := OrderResponse{
		OrderNo:              o.OrderNo,
		UserId:               o.UserId,
		AmountIDR:            o.AmountIDR,
		QuotaToCredit:        o.QuotaToCredit,
		PaymentMethod:        string(o.PaymentMethod),
		Status:               string(o.Status),
		XenditPaymentId:      o.XenditPaymentId,
		XenditPaymentChannel: o.XenditPaymentChannel,
		QRString:             o.QRString,
		VANumber:             o.VANumber,
		CheckoutURL:          o.CheckoutURL,
		CreatedAt:            o.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		ExpiresAt:            o.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
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
	method := model.PaymentMethod(req.PaymentMethod)
	if method != model.MethodQRIS && method != model.MethodVABCA {
		respondError(c, apierrors.UnsupportedMethod,
			"only 'qris' and 'va_bca' are supported in v1")
		return
	}
	if req.AmountIDR <= 0 {
		respondError(c, apierrors.InvalidParameter, "amount_idr must be > 0")
		return
	}

	payerName := req.PayerName
	if payerName == "" {
		payerName = middleware.UserDisplayName(c)
	}

	params := service.CreateOrderParams{
		UserId:        userId,
		AmountIDR:     req.AmountIDR,
		PaymentMethod: method,
		PayerName:     payerName,
		ClientIP:      c.ClientIP(),
		UserAgent:     c.Request.UserAgent(),
	}

	ctx := logger.WithFields(c.Request.Context(),
		zap.Int("user_id", userId),
		zap.String("payment_method", req.PaymentMethod),
		zap.Int64("amount_idr", req.AmountIDR),
	)

	order, err := h.OrderService.CreateOrder(ctx, params)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrAmountTooLow):
			respondError(c, apierrors.OrderAmountTooLow, "")
		case errors.Is(err, service.ErrAmountTooHigh):
			respondError(c, apierrors.OrderAmountTooHigh, "")
		case errors.Is(err, service.ErrUnsupportedMethod):
			respondError(c, apierrors.UnsupportedMethod, "")
		case errors.Is(err, service.ErrInvalidConfig):
			respondError(c, apierrors.ConfigError, err.Error())
		default:
			// Xendit / DB failures bubble up here. We always return the
			// order if the service produced one (so the user knows their
			// order_no for support) - even if it's in `failed` state.
			if order != nil {
				logger.L(ctx).Error("CreateOrder partial: order persisted but xendit failed",
					zap.String("order_no", order.OrderNo), zap.Error(err))
				c.JSON(http.StatusBadGateway, gin.H{
					"success": false,
					"code":    apierrors.XenditCallFailed,
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

// Get handles GET /orders/:order_no. Returns 404 if the order doesn't exist,
// 403 if it belongs to a different user (and the caller isn't admin).
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
		// We deliberately return ORDER_FORBIDDEN, not OBJECT_NOT_FOUND.
		// We trust the (authenticated) caller; revealing that the order
		// exists is fine. If unauth'd discovery becomes a concern, switch
		// both branches to ORDER_NOT_FOUND.
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
