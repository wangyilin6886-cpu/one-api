package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/provider"
	"github.com/songquanpeng/one-api/payment/internal/repository"
)

// QuotaPerUnit mirrors one-api's `config.QuotaPerUnit` constant (500_000
// quota per 1 USD). Hard-coded so the payment-service stays decoupled
// from one-api's internals; if one-api ever changes this you'd update
// the constant here too.
const QuotaPerUnit = int64(500_000)

// QuotaFromCents converts a USD-cents amount to quota with floor semantics.
// We always floor so the user gets at most what they paid for - never
// half a quota unit more.
//
//   $5  = 500   cents -> floor(500   * 500_000 / 100) = 2_500_000  quota
//   $19 = 1900  cents -> floor(1900  * 500_000 / 100) = 9_500_000  quota
//   $99 = 9900  cents -> floor(9900  * 500_000 / 100) = 49_500_000 quota
func QuotaFromCents(cents int64) int64 {
	return cents * QuotaPerUnit / 100
}

// OrderService orchestrates order creation, lookup, and state transitions.
// All state-changing operations run inside a DB transaction.
type OrderService struct {
	db         *gorm.DB
	orderRepo  *repository.OrderRepo
	configRepo *repository.PaymentConfigRepo
	provider   provider.PaymentProvider
	alerter    Alerter
	now        func() time.Time // injectable clock for tests
}

func NewOrderService(
	db *gorm.DB,
	orderRepo *repository.OrderRepo,
	configRepo *repository.PaymentConfigRepo,
	p provider.PaymentProvider,
	alerter Alerter,
) *OrderService {
	return &OrderService{
		db:         db,
		orderRepo:  orderRepo,
		configRepo: configRepo,
		provider:   p,
		alerter:    alerter,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// CreateOrderParams is the validated, normalized input for CreateOrder.
type CreateOrderParams struct {
	UserId         int
	AmountUSDCents int64
	Currency       string // "USD" only in v1
	CustomerEmail  string // forwarded to provider for the receipt
	ClientIP       string
	UserAgent      string
}

// Catalog of errors CreateOrder can return. Handlers translate to error codes.
var (
	ErrAmountTooLow      = errors.New("amount below minimum")
	ErrAmountTooHigh     = errors.New("amount above maximum")
	ErrUnsupportedCurrency = errors.New("currency not supported in v1 (USD only)")
	ErrInvalidConfig     = errors.New("payment_config is invalid")
	ErrProviderFailed    = errors.New("provider checkout creation failed")
)

// CreateOrder runs the full creation flow:
//  1. Read runtime config (min/max amount, expiry).
//  2. Validate amount + currency.
//  3. Lock quota_to_credit via QuotaFromCents (immutable on the row).
//  4. Generate order_no; INSERT pending order. (Retry on unique collision.)
//  5. Ask the provider to create a hosted checkout session.
//  6. On success: persist provider_checkout_id + checkout_url.
//     On failure: transition order to `failed` with failure_reason.
func (s *OrderService) CreateOrder(ctx context.Context, p CreateOrderParams) (*model.Order, error) {
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if p.Currency != "USD" {
		return nil, ErrUnsupportedCurrency
	}

	minC, maxC, expiryMin, err := s.readRuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	if p.AmountUSDCents < minC {
		return nil, ErrAmountTooLow
	}
	if p.AmountUSDCents > maxC {
		return nil, ErrAmountTooHigh
	}

	quota := QuotaFromCents(p.AmountUSDCents)
	if quota <= 0 {
		return nil, fmt.Errorf("computed quota is non-positive (cents=%d): %w",
			p.AmountUSDCents, ErrInvalidConfig)
	}

	now := s.now()
	expiresAt := now.Add(time.Duration(expiryMin) * time.Minute)

	// Insert a `pending` order first, BEFORE calling the provider. This
	// way, if the provider call succeeds but our network drops on the way
	// back, the order is still on file and a future reconciliation can
	// match it to the provider-side checkout.
	//
	// 5 attempts on order_no collision is overkill given a 16-char random
	// suffix, but cheap.
	const insertAttempts = 5
	var order *model.Order
	for i := 0; i < insertAttempts; i++ {
		orderNo, err := generateOrderNo(now)
		if err != nil {
			return nil, fmt.Errorf("generate order_no: %w", err)
		}
		candidate := &model.Order{
			OrderNo:        orderNo,
			UserId:         p.UserId,
			OrderType:      model.OrderTypeTopup,
			AmountUSDCents: p.AmountUSDCents,
			Currency:       p.Currency,
			QuotaToCredit:  quota,
			Provider:       s.provider.Name(),
			Status:         model.StatusPending,
			CreatedAt:      now,
			ExpiresAt:      expiresAt,
			ClientIP:       p.ClientIP,
			UserAgent:      truncate(p.UserAgent, 255),
			UpdatedAt:      now,
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

	// Call the provider. 30s overall budget covers internal retries.
	provCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	chk, err := s.provider.CreateCheckout(provCtx, provider.CheckoutParams{
		OrderNo:        order.OrderNo,
		UserId:         p.UserId,
		AmountUSDCents: p.AmountUSDCents,
		Currency:       p.Currency,
		CustomerEmail:  p.CustomerEmail,
		IdempotencyKey: order.OrderNo, // same key for retries of the same logical order
	})
	if err != nil {
		s.alerter.ProviderError(ctx, order.OrderNo, "create_checkout", err)
		return order, s.failOrder(ctx, order, "provider_checkout_failed", err)
	}

	// Persist checkout reference + URL.
	if err := s.orderRepo.UpdateStatusAndFields(ctx, nil, order.OrderNo, model.StatusPending, map[string]any{
		"provider_checkout_id": chk.ProviderCheckoutId,
		"checkout_url":         chk.CheckoutURL,
	}); err != nil {
		// We have a real checkout at the provider but lost the URL locally.
		// Escalate; an operator can fetch the URL from the provider dashboard
		// and patch the row.
		s.alerter.NeedsManualReview(ctx, order.OrderNo,
			"checkout created at provider but DB update failed",
			zap.String("provider_checkout_id", chk.ProviderCheckoutId),
			zap.Error(err))
		return order, fmt.Errorf("persist checkout url: %w", err)
	}

	// Reload so caller sees the freshly-attached fields.
	return s.orderRepo.GetByOrderNo(ctx, order.OrderNo)
}

// failOrder transitions order -> failed and records the reason. Returns the
// original error so the caller can surface it to the handler.
func (s *OrderService) failOrder(ctx context.Context, o *model.Order, reason string, cause error) error {
	patch := map[string]any{
		"failure_reason": truncate(reason+": "+cause.Error(), 255),
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

// readRuntimeConfig loads the order-creation knobs from payment_config.
func (s *OrderService) readRuntimeConfig(ctx context.Context) (
	minCents, maxCents int64, expiryMin int, err error,
) {
	all, err := s.configRepo.GetAll(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("load runtime config: %w", err)
	}
	minCents, err = readInt64(all, model.CfgMinTopupCents, 500)
	if err != nil {
		return
	}
	maxCents, err = readInt64(all, model.CfgMaxTopupCents, 200_000)
	if err != nil {
		return
	}
	expiryMinI64, err := readInt64(all, model.CfgOrderExpiryMinutes, 30)
	if err != nil {
		return
	}
	expiryMin = int(expiryMinI64)
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

// generateOrderNo produces a 27-char order_no in the format
// "ORD" + YYYYMMDD + 16-char random alphanumeric (uppercase + digits).
// Format chosen to stay <= 32 chars (matches DB column width) and to give
// support staff a date hint at a glance.
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
	return "ORD" + now.UTC().Format("20060102") + string(buf), nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
