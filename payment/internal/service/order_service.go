package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/repository"
	"github.com/songquanpeng/one-api/payment/internal/xendit"
)

// QuotaPerUnit mirrors one-api's `config.QuotaPerUnit` constant. 1 USD = 500,000 quota.
// Hard-coded here because the payment-service is intentionally decoupled from
// one-api's internals - importing one-api's config would re-couple them.
const QuotaPerUnit = int64(500_000)

// OrderService orchestrates order creation, lookup, and state transitions.
// All state-changing operations run inside a DB transaction.
type OrderService struct {
	db            *gorm.DB
	orderRepo     *repository.OrderRepo
	configRepo    *repository.PaymentConfigRepo
	xendit        *xendit.Client
	alerter       Alerter
	now           func() time.Time // injectable clock for tests
}

func NewOrderService(
	db *gorm.DB,
	orderRepo *repository.OrderRepo,
	configRepo *repository.PaymentConfigRepo,
	xc *xendit.Client,
	alerter Alerter,
) *OrderService {
	return &OrderService{
		db:         db,
		orderRepo:  orderRepo,
		configRepo: configRepo,
		xendit:     xc,
		alerter:    alerter,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// CreateOrderParams is the validated, normalized input for CreateOrder.
type CreateOrderParams struct {
	UserId        int
	AmountIDR     int64
	PaymentMethod model.PaymentMethod
	PayerName     string // shown in BCA mobile for VA; ignored for QRIS
	ClientIP      string
	UserAgent     string
}

// ErrXxx is the catalog of errors CreateOrder returns. The handler maps them
// to error codes/HTTP status.
var (
	ErrAmountTooLow      = errors.New("amount below minimum")
	ErrAmountTooHigh     = errors.New("amount above maximum")
	ErrUnsupportedMethod = errors.New("payment method not supported in this version")
	ErrInvalidConfig     = errors.New("payment_config is invalid")
)

// CreateOrder runs the full creation flow:
//  1. Read runtime config (min/max amount, exchange rate, expiry).
//  2. Validate amount.
//  3. Compute locked quota_to_credit.
//  4. Generate order_no; INSERT pending order. (Retry on unique collision.)
//  5. Call Xendit to create the QR / VA resource.
//  6. On success: persist xendit ids + qr_string / va_number.
//     On failure: transition order to failed with failure_reason.
func (s *OrderService) CreateOrder(ctx context.Context, p CreateOrderParams) (*model.Order, error) {
	if p.PaymentMethod != model.MethodQRIS && p.PaymentMethod != model.MethodVABCA {
		return nil, ErrUnsupportedMethod
	}

	minAmt, maxAmt, expiryMin, rate, err := s.readRuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	if p.AmountIDR < minAmt {
		return nil, ErrAmountTooLow
	}
	if p.AmountIDR > maxAmt {
		return nil, ErrAmountTooHigh
	}

	// Correction 3: lock the exchange rate at creation time. Compute
	// quota_to_credit with floor (decimal division then floor) so users get
	// at most what they paid for - never more. Edits to the rate after
	// this point never affect this order.
	amountDec := decimal.NewFromInt(p.AmountIDR)
	quotaDec := amountDec.
		Mul(decimal.NewFromInt(QuotaPerUnit)).
		Div(rate)
	quota := quotaDec.Floor().IntPart()
	if quota <= 0 {
		return nil, fmt.Errorf("computed quota is non-positive (amount=%d rate=%s): %w",
			p.AmountIDR, rate.String(), ErrInvalidConfig)
	}

	now := s.now()
	expiresAt := now.Add(time.Duration(expiryMin) * time.Minute)

	// 5 attempts is overkill (we're using 16 random chars - collisions are
	// astronomically rare) but cheap; protects against a freak collision.
	const insertAttempts = 5
	var order *model.Order
	for i := 0; i < insertAttempts; i++ {
		orderNo, err := generateOrderNo(now)
		if err != nil {
			return nil, fmt.Errorf("generate order_no: %w", err)
		}
		candidate := &model.Order{
			OrderNo:       orderNo,
			UserId:        p.UserId,
			AmountIDR:     p.AmountIDR,
			ExchangeRate:  rate,
			QuotaToCredit: quota,
			PaymentMethod: p.PaymentMethod,
			Status:        model.StatusPending,
			CreatedAt:     now,
			ExpiresAt:     expiresAt,
			ClientIP:      p.ClientIP,
			UserAgent:     truncate(p.UserAgent, 255),
			UpdatedAt:     now,
		}
		err = s.orderRepo.Create(ctx, nil, candidate)
		if err == nil {
			order = candidate
			break
		}
		if !errors.Is(err, repository.ErrDuplicateOrderNo) {
			return nil, fmt.Errorf("insert order: %w", err)
		}
		logger.L(ctx).Warn("order_no collision, retrying",
			zap.String("order_no", orderNo), zap.Int("attempt", i+1))
	}
	if order == nil {
		return nil, errors.New("could not insert order after retries")
	}

	// Now call Xendit. Failures transition the order to "failed" - keeping
	// the row visible for audit, freeing the order_no for nothing in
	// particular (we don't reuse it; a new order is a new order_no).
	xenditCtx, cancel := context.WithTimeout(ctx, 30*time.Second) // covers retries
	defer cancel()

	switch p.PaymentMethod {
	case model.MethodQRIS:
		if err := s.attachQRIS(xenditCtx, order, expiresAt); err != nil {
			return order, s.failOrder(ctx, order, "qris_create_failed", err)
		}
	case model.MethodVABCA:
		if err := s.attachVABCA(xenditCtx, order, p.PayerName, expiresAt); err != nil {
			return order, s.failOrder(ctx, order, "va_create_failed", err)
		}
	}

	// Reload so caller sees the freshly-attached Xendit fields.
	return s.orderRepo.GetByOrderNo(ctx, order.OrderNo)
}

func (s *OrderService) attachQRIS(ctx context.Context, o *model.Order, expiresAt time.Time) error {
	resp, err := s.xendit.CreateQRCode(ctx, o.OrderNo, o.AmountIDR, expiresAt)
	if err != nil {
		s.alerter.XenditError(ctx, o.OrderNo, "create_qr", err)
		return err
	}
	return s.orderRepo.UpdateStatusAndFields(ctx, nil, o.OrderNo, model.StatusPending, map[string]any{
		"xendit_payment_id":      resp.Id,
		"xendit_payment_channel": "QRIS",
		"qr_string":              resp.QRString,
	})
}

func (s *OrderService) attachVABCA(ctx context.Context, o *model.Order, payerName string, expiresAt time.Time) error {
	if payerName == "" {
		payerName = "OneAPI User"
	}
	resp, err := s.xendit.CreateVirtualAccount(ctx, o.OrderNo, "BCA", payerName, o.AmountIDR, expiresAt)
	if err != nil {
		s.alerter.XenditError(ctx, o.OrderNo, "create_va", err)
		return err
	}
	return s.orderRepo.UpdateStatusAndFields(ctx, nil, o.OrderNo, model.StatusPending, map[string]any{
		"xendit_payment_id":      resp.Id,
		"xendit_payment_channel": "BCA",
		"va_number":              resp.AccountNumber,
	})
}

// failOrder transitions order -> failed and records the reason. Returns the
// original error so the caller can surface it to the handler.
func (s *OrderService) failOrder(ctx context.Context, o *model.Order, reason string, cause error) error {
	patch := map[string]any{
		"failure_reason": truncate(reason+": "+cause.Error(), 128),
	}
	if err := s.orderRepo.UpdateStatusAndFields(ctx, nil, o.OrderNo, model.StatusFailed, patch); err != nil {
		logger.L(ctx).Error("failed to mark order failed",
			zap.String("order_no", o.OrderNo), zap.Error(err))
	}
	return cause
}

// GetOrder returns an order by order_no, or gorm.ErrRecordNotFound.
func (s *OrderService) GetOrder(ctx context.Context, orderNo string) (*model.Order, error) {
	return s.orderRepo.GetByOrderNo(ctx, orderNo)
}

// readRuntimeConfig loads all the order-creation knobs from payment_config.
func (s *OrderService) readRuntimeConfig(ctx context.Context) (
	minAmt, maxAmt int64, expiryMin int, rate decimal.Decimal, err error,
) {
	all, err := s.configRepo.GetAll(ctx)
	if err != nil {
		return 0, 0, 0, decimal.Zero, fmt.Errorf("load runtime config: %w", err)
	}

	minAmt, err = readInt64(all, model.CfgMinTopupAmountIDR, 10_000)
	if err != nil {
		return
	}
	maxAmt, err = readInt64(all, model.CfgMaxTopupAmountIDR, 10_000_000)
	if err != nil {
		return
	}
	expiryMinI64, err := readInt64(all, model.CfgOrderExpiryMinutes, 30)
	if err != nil {
		return
	}
	expiryMin = int(expiryMinI64)

	rate, err = readDecimal(all, model.CfgExchangeRateIDRPerUSD, "16500")
	if err != nil {
		return
	}
	if rate.LessThanOrEqual(decimal.Zero) {
		err = fmt.Errorf("%w: exchange rate must be > 0", ErrInvalidConfig)
		return
	}
	return
}

func readInt64(m map[string]string, key string, def int64) (int64, error) {
	v, ok := m[key]
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s is not int64 (%q)", ErrInvalidConfig, key, v)
	}
	return n, nil
}

func readDecimal(m map[string]string, key, def string) (decimal.Decimal, error) {
	v, ok := m[key]
	if !ok || v == "" {
		v = def
	}
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%w: %s is not decimal (%q)", ErrInvalidConfig, key, v)
	}
	return d, nil
}

// generateOrderNo produces a 27-char order_no in the format
// "IDR" + YYYYMMDD + 16-char random alphanumeric (uppercase + digits).
func generateOrderNo(now time.Time) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const randLen = 16
	buf := make([]byte, randLen)
	for i := 0; i < randLen; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		buf[i] = alphabet[n.Int64()]
	}
	return "IDR" + now.UTC().Format("20060102") + string(buf), nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
