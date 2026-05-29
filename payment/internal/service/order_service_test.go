package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/provider"
	"github.com/songquanpeng/one-api/payment/internal/repository"
)

// fakeProvider implements provider.PaymentProvider for tests. The two
// methods can be overridden per-test via the *Fn fields.
type fakeProvider struct {
	name             string
	createCheckoutFn func(ctx context.Context, p provider.CheckoutParams) (*provider.CheckoutResult, error)
	verifyWebhookFn  func(ctx context.Context, body []byte, headers map[string]string) (*provider.NormalizedEvent, error)
	createCalls      int32
}

func (f *fakeProvider) Name() string {
	if f.name == "" {
		return "fake"
	}
	return f.name
}
func (f *fakeProvider) CreateCheckout(ctx context.Context, p provider.CheckoutParams) (*provider.CheckoutResult, error) {
	atomic.AddInt32(&f.createCalls, 1)
	if f.createCheckoutFn != nil {
		return f.createCheckoutFn(ctx, p)
	}
	return &provider.CheckoutResult{
		CheckoutURL:        "https://polar.test/checkout/stub",
		ProviderCheckoutId: "chk_stub_" + p.OrderNo,
		ExpiresAt:          time.Now().Add(time.Hour),
	}, nil
}
func (f *fakeProvider) VerifyWebhook(ctx context.Context, body []byte, headers map[string]string) (*provider.NormalizedEvent, error) {
	if f.verifyWebhookFn != nil {
		return f.verifyWebhookFn(ctx, body, headers)
	}
	return nil, errors.New("not configured")
}

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:?_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&model.Order{}, &model.WebhookEvent{},
		&model.TopupCallback{}, &model.Refund{}, &model.PaymentConfig{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newOrderServiceForTest(t *testing.T, db *gorm.DB) (*OrderService, *repository.PaymentConfigRepo, *fakeProvider) {
	t.Helper()
	configRepo := repository.NewPaymentConfigRepo(db, 100*time.Millisecond)
	if err := configRepo.SeedIfMissing(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	orderRepo := repository.NewOrderRepo(db)
	fp := &fakeProvider{name: "polar"}
	return NewOrderService(db, orderRepo, configRepo, fp, NewLogAlerter()), configRepo, fp
}

func TestQuotaFromCents(t *testing.T) {
	cases := []struct {
		cents int64
		want  int64
	}{
		{500, 2_500_000},      // $5 -> 2.5M quota
		{1900, 9_500_000},     // $19 (Pro tier)
		{9900, 49_500_000},    // $99 (Max tier)
		{100, 500_000},        // exact $1
		{99, 495_000},         // 99 cents
		{1, 5000},             // 1 cent
	}
	for _, c := range cases {
		got := QuotaFromCents(c.cents)
		if got != c.want {
			t.Errorf("QuotaFromCents(%d) = %d, want %d", c.cents, got, c.want)
		}
	}
}

func TestOrderService_CreateOrder_HappyPath(t *testing.T) {
	db := setupTestDB(t)
	svc, _, fp := newOrderServiceForTest(t, db)

	o, err := svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountUSDCents: 1000, CustomerEmail: "u@example.com", ClientIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if o.Status != model.StatusPending {
		t.Fatalf("status: %s", o.Status)
	}
	if !strings.HasPrefix(o.OrderNo, "ORD") || len(o.OrderNo) != 27 {
		t.Fatalf("order_no format: %s", o.OrderNo)
	}
	// $10 -> 1000 cents * 500_000 / 100 = 5_000_000 quota
	if o.QuotaToCredit != 5_000_000 {
		t.Fatalf("quota: got %d want %d", o.QuotaToCredit, 5_000_000)
	}
	if o.Currency != "USD" {
		t.Fatalf("currency: %s", o.Currency)
	}
	if o.Provider != "polar" {
		t.Fatalf("provider: %s", o.Provider)
	}
	if o.CheckoutURL == "" {
		t.Fatalf("checkout url not attached")
	}
	if !strings.HasPrefix(o.ProviderCheckoutId, "chk_stub_") {
		t.Fatalf("provider checkout id: %s", o.ProviderCheckoutId)
	}
	if fp.createCalls != 1 {
		t.Fatalf("expected 1 provider call, got %d", fp.createCalls)
	}
}

func TestOrderService_CreateOrder_AmountBounds(t *testing.T) {
	db := setupTestDB(t)
	svc, _, _ := newOrderServiceForTest(t, db)

	_, err := svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountUSDCents: 100, // $1, below default min $5
	})
	if err != ErrAmountTooLow {
		t.Fatalf("want ErrAmountTooLow, got %v", err)
	}

	_, err = svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountUSDCents: 1_000_000, // $10_000, above default max $2000
	})
	if err != ErrAmountTooHigh {
		t.Fatalf("want ErrAmountTooHigh, got %v", err)
	}
}

func TestOrderService_CreateOrder_UnsupportedCurrency(t *testing.T) {
	db := setupTestDB(t)
	svc, _, _ := newOrderServiceForTest(t, db)

	_, err := svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountUSDCents: 1000, Currency: "EUR",
	})
	if err != ErrUnsupportedCurrency {
		t.Fatalf("want ErrUnsupportedCurrency, got %v", err)
	}
}

func TestOrderService_CreateOrder_ProviderFails_OrderMarkedFailed(t *testing.T) {
	db := setupTestDB(t)
	svc, _, fp := newOrderServiceForTest(t, db)
	fp.createCheckoutFn = func(ctx context.Context, p provider.CheckoutParams) (*provider.CheckoutResult, error) {
		return nil, errors.New("provider down")
	}

	o, err := svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountUSDCents: 1000,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "provider down") {
		t.Fatalf("unexpected err: %v", err)
	}
	// The order row should still exist with status=failed - vital for support.
	r := repository.NewOrderRepo(db)
	got, gerr := r.GetByOrderNo(context.Background(), o.OrderNo)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if got.Status != model.StatusFailed {
		t.Fatalf("expected failed, got %s", got.Status)
	}
	if got.FailureReason == "" {
		t.Fatal("expected failure_reason to be set")
	}
}

func TestGenerateOrderNo(t *testing.T) {
	seen := map[string]bool{}
	now := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		got, err := generateOrderNo(now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 27 {
			t.Fatalf("len: %d (%s)", len(got), got)
		}
		if !strings.HasPrefix(got, "ORD20260518") {
			t.Fatalf("prefix: %s", got)
		}
		if seen[got] {
			t.Fatalf("dup after %d tries: %s", i, got)
		}
		seen[got] = true
	}
}
