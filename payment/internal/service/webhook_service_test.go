package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/provider"
	"github.com/songquanpeng/one-api/payment/internal/repository"
)

// setupWebhookEnv builds a WebhookService whose provider returns a fixed
// NormalizedEvent (so each test focuses on the orchestration, not on
// signature parsing). `eventFn` is the per-test event generator.
func setupWebhookEnv(
	t *testing.T,
	oneAPIHandler http.HandlerFunc,
	eventFn func() (*provider.NormalizedEvent, error),
) (*WebhookService, *gorm.DB, *repository.OrderRepo, *httptest.Server) {
	db := setupTestDB(t)
	orderRepo := repository.NewOrderRepo(db)
	webhookRepo := repository.NewWebhookRepo(db)
	topupCBRepo := repository.NewTopupCallbackRepo(db)

	oneAPI := httptest.NewServer(oneAPIHandler)
	t.Cleanup(oneAPI.Close)

	tc := NewTopupClient(oneAPI.URL, "padding-padding-padding-padding-padding!", 5*time.Second)
	fp := &fakeProvider{
		name: "polar",
		verifyWebhookFn: func(ctx context.Context, body []byte, headers map[string]string) (*provider.NormalizedEvent, error) {
			return eventFn()
		},
	}
	svc := NewWebhookService(db, orderRepo, webhookRepo, topupCBRepo, tc, fp, NewLogAlerter())
	return svc, db, orderRepo, oneAPI
}

func seedOrder(t *testing.T, db *gorm.DB, status model.OrderStatus, qcredit int64) *model.Order {
	t.Helper()
	now := time.Now().UTC()
	o := &model.Order{
		OrderNo:        fmt.Sprintf("ORD20260518TEST%013d", time.Now().UnixNano()%1e13),
		UserId:         42,
		OrderType:      model.OrderTypeTopup,
		AmountUSDCents: 1000, // $10
		Currency:       "USD",
		QuotaToCredit:  qcredit,
		Provider:       "polar",
		Status:         status,
		CreatedAt:      now.Add(-time.Hour),
		ExpiresAt:      now.Add(time.Hour),
		UpdatedAt:      now,
	}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	return o
}

func happyOneAPIHandler(callCount *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if callCount != nil {
			atomic.AddInt32(callCount, 1)
		}
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true,"data":{"order_no":"X","action":"topup","user_id":42,"quota":5000000,"new_balance":5000000,"idempotent_replay":false}}`))
	}
}

// Helper: a one-shot event factory that always returns the same event for a
// given order. Caller decides the type/id to test routing.
func eventOf(typ provider.EventType, resourceId, orderNo string, amountCents int64) func() (*provider.NormalizedEvent, error) {
	return func() (*provider.NormalizedEvent, error) {
		return &provider.NormalizedEvent{
			Type:               typ,
			ProviderResourceId: resourceId,
			ProviderPaymentId:  resourceId,
			OrderNo:            orderNo,
			AmountUSDCents:     amountCents,
			Currency:           "USD",
			PaidAt:             time.Now().UTC(),
		}, nil
	}
}

func TestWebhookService_PendingPaidCredited(t *testing.T) {
	var calls int32
	o := (*model.Order)(nil)
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls), func() (*provider.NormalizedEvent, error) {
		return &provider.NormalizedEvent{
			Type:               provider.EventCheckoutCompleted,
			ProviderResourceId: "evt_001",
			ProviderPaymentId:  "pay_001",
			OrderNo:            o.OrderNo,
			AmountUSDCents:     1000,
			Currency:           "USD",
		}, nil
	})
	o = seedOrder(t, db, model.StatusPending, 5_000_000)

	out := svc.Process(context.Background(), IncomingWebhook{RawPayload: []byte(`{}`)})
	if out.Result != model.WebhookResultOK {
		t.Fatalf("result: %s msg=%s", out.Result, out.Message)
	}
	if calls != 1 {
		t.Fatalf("one-api calls: %d", calls)
	}
	var got model.Order
	if err := db.First(&got, "order_no = ?", o.OrderNo).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusCredited {
		t.Fatalf("status: %s", got.Status)
	}
	if got.CreditedAt == nil {
		t.Fatalf("credited_at not set")
	}
	if got.QuotaCredited != 5_000_000 {
		t.Fatalf("quota_credited: %d", got.QuotaCredited)
	}
	if got.ProviderPaymentId != "pay_001" {
		t.Fatalf("provider_payment_id: %s", got.ProviderPaymentId)
	}
}

func TestWebhookService_DuplicateDelivery(t *testing.T) {
	var calls int32
	o := (*model.Order)(nil)
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls), func() (*provider.NormalizedEvent, error) {
		return &provider.NormalizedEvent{
			Type:               provider.EventCheckoutCompleted,
			ProviderResourceId: "evt_dup",
			ProviderPaymentId:  "pay_dup",
			OrderNo:            o.OrderNo,
			AmountUSDCents:     1000,
			Currency:           "USD",
		}, nil
	})
	o = seedOrder(t, db, model.StatusPending, 100)

	out1 := svc.Process(context.Background(), IncomingWebhook{RawPayload: []byte(`{}`)})
	if out1.Result != model.WebhookResultOK {
		t.Fatalf("first: %s", out1.Result)
	}
	out2 := svc.Process(context.Background(), IncomingWebhook{RawPayload: []byte(`{}`)})
	if out2.Result != model.WebhookResultSkipDuplicate {
		t.Fatalf("second: %s msg=%s", out2.Result, out2.Message)
	}
	if calls != 1 {
		t.Fatalf("one-api calls: %d - duplicate must NOT re-credit", calls)
	}
}

func TestWebhookService_LateCallback_Expired(t *testing.T) {
	var calls int32
	o := (*model.Order)(nil)
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls), func() (*provider.NormalizedEvent, error) {
		return eventOf(provider.EventCheckoutCompleted, "evt_late_exp", o.OrderNo, 1000)()
	})
	o = seedOrder(t, db, model.StatusExpired, 100)

	out := svc.Process(context.Background(), IncomingWebhook{RawPayload: []byte(`{}`)})
	if out.Result != model.WebhookResultIgnoredExpired {
		t.Fatalf("result: %s", out.Result)
	}
	if calls != 0 {
		t.Fatalf("MUST NOT credit on late callback. one-api calls: %d", calls)
	}
	var got model.Order
	_ = db.First(&got, "order_no = ?", o.OrderNo).Error
	if got.Status != model.StatusNeedsManualReview {
		t.Fatalf("order status: %s (want needs_manual_review)", got.Status)
	}
	if got.MetadataJSON == "" {
		t.Fatalf("metadata_json should have late-callback snippet")
	}
}

func TestWebhookService_LateCallback_Canceled(t *testing.T) {
	var calls int32
	o := (*model.Order)(nil)
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls), func() (*provider.NormalizedEvent, error) {
		return eventOf(provider.EventCheckoutCompleted, "evt_late_can", o.OrderNo, 1000)()
	})
	o = seedOrder(t, db, model.StatusCanceled, 100)

	out := svc.Process(context.Background(), IncomingWebhook{RawPayload: []byte(`{}`)})
	if out.Result != model.WebhookResultIgnoredExpired {
		t.Fatalf("result: %s", out.Result)
	}
	if calls != 0 {
		t.Fatalf("MUST NOT credit canceled-order late callback")
	}
	var got model.Order
	_ = db.First(&got, "order_no = ?", o.OrderNo).Error
	if got.Status != model.StatusNeedsManualReview {
		t.Fatalf("status: %s", got.Status)
	}
}

func TestWebhookService_UnknownOrder(t *testing.T) {
	svc, _, _, _ := setupWebhookEnv(t, happyOneAPIHandler(nil), func() (*provider.NormalizedEvent, error) {
		return eventOf(provider.EventCheckoutCompleted, "evt_orphan", "ORD_NOPE_NOPE", 1000)()
	})
	out := svc.Process(context.Background(), IncomingWebhook{RawPayload: []byte(`{}`)})
	if out.Result != model.WebhookResultIgnoredNoOrder {
		t.Fatalf("result: %s", out.Result)
	}
}

func TestWebhookService_NonCreditTriggering(t *testing.T) {
	// subscription.canceled is recognized but does not trigger credit.
	o := (*model.Order)(nil)
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(nil), func() (*provider.NormalizedEvent, error) {
		return eventOf(provider.EventSubscriptionCanceled, "evt_subcan", o.OrderNo, 0)()
	})
	o = seedOrder(t, db, model.StatusPending, 100)
	out := svc.Process(context.Background(), IncomingWebhook{RawPayload: []byte(`{}`)})
	if out.Result != model.WebhookResultIgnoredNonFinal {
		t.Fatalf("result: %s", out.Result)
	}
}

func TestWebhookService_AmountMismatch_StillCredits_AndAlerts(t *testing.T) {
	// Order is for $10 (1000 cents); webhook says $5 (500 cents). We still
	// credit at the locked quota_to_credit and rely on the alert for the
	// human to reconcile.
	var calls int32
	o := (*model.Order)(nil)
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls), func() (*provider.NormalizedEvent, error) {
		return eventOf(provider.EventCheckoutCompleted, "evt_amt_mis", o.OrderNo, 500)()
	})
	o = seedOrder(t, db, model.StatusPending, 5_000_000)

	out := svc.Process(context.Background(), IncomingWebhook{RawPayload: []byte(`{}`)})
	if out.Result != model.WebhookResultOK {
		t.Fatalf("expected OK, got %s", out.Result)
	}
	if calls != 1 {
		t.Fatalf("expected 1 credit call, got %d", calls)
	}
}

func TestWebhookService_OneAPIRefusal_LeavesPaidForRetry(t *testing.T) {
	o := (*model.Order)(nil)
	failHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"success":false,"message":"injected"}`))
	})
	svc, db, _, _ := setupWebhookEnv(t, failHandler, func() (*provider.NormalizedEvent, error) {
		return eventOf(provider.EventCheckoutCompleted, "evt_retry", o.OrderNo, 1000)()
	})
	o = seedOrder(t, db, model.StatusPending, 100)

	out := svc.Process(context.Background(), IncomingWebhook{RawPayload: []byte(`{}`)})
	if out.Result != model.WebhookResultError {
		t.Fatalf("result: %s", out.Result)
	}
	var got model.Order
	_ = db.First(&got, "order_no = ?", o.OrderNo).Error
	if got.Status != model.StatusPaid {
		t.Fatalf("status: %s (want paid so retry cron picks it up)", got.Status)
	}
}

func TestWebhookService_CreditOrder_IdempotentOnTopupCallback(t *testing.T) {
	var calls int32
	svc, db, orderRepo, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls), nil)
	o := seedOrder(t, db, model.StatusPaid, 100)

	if err := svc.CreditOrder(context.Background(), o); err != nil {
		t.Fatalf("credit 1: %v", err)
	}
	reload, _ := orderRepo.GetByOrderNo(context.Background(), o.OrderNo)
	if reload.Status != model.StatusCredited {
		t.Fatalf("status after credit: %s", reload.Status)
	}
	if calls != 1 {
		t.Fatalf("calls after first credit: %d", calls)
	}

	if err := svc.CreditOrder(context.Background(), reload); err != nil {
		t.Fatalf("credit 2 (retry): %v", err)
	}
	if calls != 1 {
		t.Fatalf("retry must NOT re-call one-api. calls: %d", calls)
	}
}

func TestIsCreditTriggering(t *testing.T) {
	wantTrue := []provider.EventType{
		provider.EventCheckoutCompleted,
		provider.EventSubscriptionCreated,
		provider.EventSubscriptionRenewed,
	}
	wantFalse := []provider.EventType{
		provider.EventCheckoutFailed,
		provider.EventSubscriptionCanceled,
		provider.EventSubscriptionPastDue,
		provider.EventRefundIssued,
		provider.EventIgnored,
	}
	for _, x := range wantTrue {
		if !isCreditTriggering(x) {
			t.Errorf("%s should trigger credit", x)
		}
	}
	for _, x := range wantFalse {
		if isCreditTriggering(x) {
			t.Errorf("%s should NOT trigger credit", x)
		}
	}
}
