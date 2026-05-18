package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/repository"
	"github.com/songquanpeng/one-api/payment/internal/xendit"
)

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

// stubXendit serves canned 200 OK responses so OrderService.CreateOrder
// gets past the network step in unit tests.
func stubXenditServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		switch {
		case strings.HasSuffix(r.URL.Path, "/qr_codes"):
			fmt.Fprintf(w, `{"id":"qr_stub_%d","reference_id":"x","qr_string":"00020101..."}`, time.Now().UnixNano())
		case strings.HasSuffix(r.URL.Path, "/callback_virtual_accounts"):
			fmt.Fprintf(w, `{"id":"va_stub_%d","external_id":"x","account_number":"8808123456","bank_code":"BCA"}`, time.Now().UnixNano())
		default:
			http.Error(w, "unexpected path "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newOrderServiceForTest(t *testing.T, db *gorm.DB) (*OrderService, *repository.PaymentConfigRepo) {
	configRepo := repository.NewPaymentConfigRepo(db, 100*time.Millisecond)
	if err := configRepo.SeedIfMissing(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	xs := stubXenditServer(t)
	xc := xendit.New(xendit.Options{BaseURL: xs.URL, SecretKey: "test", Timeout: time.Second, Retries: 1})
	orderRepo := repository.NewOrderRepo(db)
	return NewOrderService(db, orderRepo, configRepo, xc, NewLogAlerter()), configRepo
}

func TestOrderService_CreateOrder_QRIS_HappyPath(t *testing.T) {
	db := setupTestDB(t)
	svc, _ := newOrderServiceForTest(t, db)

	o, err := svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountIDR: 50_000, PaymentMethod: model.MethodQRIS, ClientIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if o.Status != model.StatusPending {
		t.Fatalf("status: %s", o.Status)
	}
	if !strings.HasPrefix(o.OrderNo, "IDR") || len(o.OrderNo) != 27 {
		t.Fatalf("order_no format: %s", o.OrderNo)
	}
	// 50000 IDR * 500000 / 16500 = 1_515_151.515...; floored = 1_515_151
	if o.QuotaToCredit != 1_515_151 {
		t.Fatalf("quota: got %d want %d", o.QuotaToCredit, 1_515_151)
	}
	if o.QRString == "" {
		t.Fatalf("qr_string not attached after Xendit call")
	}
	if !o.ExchangeRate.Equal(decimal.NewFromInt(16500)) {
		t.Fatalf("rate: %s", o.ExchangeRate.String())
	}
}

func TestOrderService_CreateOrder_VABCA_HappyPath(t *testing.T) {
	db := setupTestDB(t)
	svc, _ := newOrderServiceForTest(t, db)

	o, err := svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 7, AmountIDR: 100_000, PaymentMethod: model.MethodVABCA, PayerName: "Budi",
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if o.VANumber == "" {
		t.Fatalf("va_number not attached")
	}
	if o.XenditPaymentChannel != "BCA" {
		t.Fatalf("channel: %s", o.XenditPaymentChannel)
	}
}

func TestOrderService_CreateOrder_AmountBounds(t *testing.T) {
	db := setupTestDB(t)
	svc, _ := newOrderServiceForTest(t, db)

	_, err := svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountIDR: 5_000, PaymentMethod: model.MethodQRIS, // below min 10000
	})
	if err != ErrAmountTooLow {
		t.Fatalf("want ErrAmountTooLow, got %v", err)
	}

	_, err = svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountIDR: 20_000_000, PaymentMethod: model.MethodQRIS, // above max 10000000
	})
	if err != ErrAmountTooHigh {
		t.Fatalf("want ErrAmountTooHigh, got %v", err)
	}
}

func TestOrderService_CreateOrder_UnsupportedMethod(t *testing.T) {
	db := setupTestDB(t)
	svc, _ := newOrderServiceForTest(t, db)

	_, err := svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountIDR: 50_000, PaymentMethod: model.PaymentMethod("gopay"),
	})
	if err != ErrUnsupportedMethod {
		t.Fatalf("want ErrUnsupportedMethod, got %v", err)
	}
}

func TestOrderService_ExchangeRateLockdown(t *testing.T) {
	// Correction 3: changing the rate AFTER an order is created must NOT
	// affect that order's locked quota_to_credit.
	db := setupTestDB(t)
	svc, configRepo := newOrderServiceForTest(t, db)

	o1, err := svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountIDR: 100_000, PaymentMethod: model.MethodQRIS,
	})
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	originalRate := o1.ExchangeRate
	originalQuota := o1.QuotaToCredit

	// Operator changes the rate.
	if err := configRepo.Set(context.Background(), model.CfgExchangeRateIDRPerUSD, "15000", 0); err != nil {
		t.Fatalf("set rate: %v", err)
	}
	configRepo.InvalidateCache()

	// Reload o1 and confirm its rate/quota are STILL at the original.
	r := repository.NewOrderRepo(db)
	got, err := r.GetByOrderNo(context.Background(), o1.OrderNo)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.ExchangeRate.Equal(originalRate) {
		t.Errorf("o1.ExchangeRate drifted: %s -> %s", originalRate, got.ExchangeRate)
	}
	if got.QuotaToCredit != originalQuota {
		t.Errorf("o1.QuotaToCredit drifted: %d -> %d", originalQuota, got.QuotaToCredit)
	}

	// New order uses the NEW rate.
	o2, err := svc.CreateOrder(context.Background(), CreateOrderParams{
		UserId: 1, AmountIDR: 100_000, PaymentMethod: model.MethodQRIS,
	})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if !o2.ExchangeRate.Equal(decimal.NewFromInt(15000)) {
		t.Errorf("o2.ExchangeRate should be new rate 15000, got %s", o2.ExchangeRate.String())
	}
	if o2.QuotaToCredit <= got.QuotaToCredit {
		// At lower rate the same IDR amount buys MORE quota.
		t.Errorf("o2 should credit more quota than o1: %d vs %d", o2.QuotaToCredit, got.QuotaToCredit)
	}
}

func TestOrderService_QuotaFloor(t *testing.T) {
	// quota is always floored - user gets at most what they paid for.
	// At 100 IDR, rate 16500: quota = floor(100 * 500000 / 16500) = floor(3030.30...) = 3030.
	// Wait — 100 IDR is below the min, so we can't test directly via CreateOrder.
	// Test the math via a direct decimal calc using the same formula.
	amt := decimal.NewFromInt(100)
	rate := decimal.NewFromInt(16500)
	quota := amt.Mul(decimal.NewFromInt(QuotaPerUnit)).Div(rate).Floor().IntPart()
	if quota != 3030 {
		t.Errorf("quota floor math: got %d want 3030", quota)
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
		if !strings.HasPrefix(got, "IDR20260518") {
			t.Fatalf("prefix: %s", got)
		}
		if seen[got] {
			t.Fatalf("dup after %d tries: %s", i, got)
		}
		seen[got] = true
	}
}
