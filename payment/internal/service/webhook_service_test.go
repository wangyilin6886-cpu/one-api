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

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/repository"
)

func setupWebhookEnv(t *testing.T, oneAPIHandler http.HandlerFunc) (
	*WebhookService, *gorm.DB, *repository.OrderRepo, *httptest.Server,
) {
	db := setupTestDB(t)
	orderRepo := repository.NewOrderRepo(db)
	webhookRepo := repository.NewWebhookRepo(db)
	topupCBRepo := repository.NewTopupCallbackRepo(db)

	oneAPI := httptest.NewServer(oneAPIHandler)
	t.Cleanup(oneAPI.Close)

	tc := NewTopupClient(oneAPI.URL, "padding-padding-padding-padding-padding!", 5*time.Second)
	svc := NewWebhookService(db, orderRepo, webhookRepo, topupCBRepo, tc, NewLogAlerter())
	return svc, db, orderRepo, oneAPI
}

func seedOrder(t *testing.T, db *gorm.DB, status model.OrderStatus, qcredit int64) *model.Order {
	now := time.Now().UTC()
	o := &model.Order{
		OrderNo:       fmt.Sprintf("IDR20260518TEST%013d", time.Now().UnixNano()%1e13),
		UserId:        42,
		AmountIDR:     50_000,
		ExchangeRate:  decimal.NewFromInt(16500),
		QuotaToCredit: qcredit,
		PaymentMethod: model.MethodQRIS,
		Status:        status,
		CreatedAt:     now.Add(-time.Hour),
		ExpiresAt:     now.Add(time.Hour),
		UpdatedAt:     now,
	}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	return o
}

// happyOneAPIHandler simulates a successful one-api response.
func happyOneAPIHandler(callCount *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if callCount != nil {
			atomic.AddInt32(callCount, 1)
		}
		body, _ := io.ReadAll(r.Body)
		_ = body
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true,"data":{"order_no":"X","action":"topup","user_id":42,"quota":1515151,"new_balance":1515151,"idempotent_replay":false}}`))
	}
}

func TestWebhookService_PendingPaidCredited(t *testing.T) {
	var calls int32
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls))
	o := seedOrder(t, db, model.StatusPending, 1_515_151)

	out := svc.Process(context.Background(), NormalizedWebhook{
		EventType:        "qr.payment",
		XenditResourceId: "qrpy_001",
		XenditPaymentId:  "qrpy_001",
		OrderNo:          o.OrderNo,
		Status:           "SUCCEEDED",
		PaidAmount:       50_000,
		Currency:         "IDR",
		RawPayload:       []byte(`{"id":"qrpy_001"}`),
	})

	if out.Result != model.WebhookResultOK {
		t.Fatalf("result: %s msg=%s", out.Result, out.Message)
	}
	if calls != 1 {
		t.Fatalf("one-api calls: %d, want 1", calls)
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
	if got.QuotaCredited != 1_515_151 {
		t.Fatalf("quota_credited: %d", got.QuotaCredited)
	}
}

// Layer 3 dedupe: second delivery of the same (event_type, resource_id) must
// be a no-op, even if the order would otherwise re-credit.
func TestWebhookService_DuplicateDelivery(t *testing.T) {
	var calls int32
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls))
	o := seedOrder(t, db, model.StatusPending, 100)

	w := NormalizedWebhook{
		EventType:        "qr.payment",
		XenditResourceId: "qrpy_dup",
		XenditPaymentId:  "qrpy_dup",
		OrderNo:          o.OrderNo,
		Status:           "SUCCEEDED",
		PaidAmount:       50_000,
		RawPayload:       []byte(`{}`),
	}

	out1 := svc.Process(context.Background(), w)
	if out1.Result != model.WebhookResultOK {
		t.Fatalf("first: %s", out1.Result)
	}
	out2 := svc.Process(context.Background(), w)
	if out2.Result != model.WebhookResultSkipDuplicate {
		t.Fatalf("second: %s msg=%s", out2.Result, out2.Message)
	}
	if calls != 1 {
		t.Fatalf("one-api calls: %d - duplicate should NOT re-credit", calls)
	}
}

// Correction 1: late callback against an expired order MUST NOT credit;
// order must move to needs_manual_review.
func TestWebhookService_LateCallback_Expired(t *testing.T) {
	var calls int32
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls))
	o := seedOrder(t, db, model.StatusExpired, 100)

	out := svc.Process(context.Background(), NormalizedWebhook{
		EventType:        "qr.payment",
		XenditResourceId: "qrpy_late1",
		XenditPaymentId:  "qrpy_late1",
		OrderNo:          o.OrderNo,
		Status:           "SUCCEEDED",
		PaidAmount:       50_000,
		RawPayload:       []byte(`{}`),
	})

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

// Correction 2: late callback against a canceled order: same behavior.
func TestWebhookService_LateCallback_Canceled(t *testing.T) {
	var calls int32
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls))
	o := seedOrder(t, db, model.StatusCanceled, 100)

	out := svc.Process(context.Background(), NormalizedWebhook{
		EventType: "qr.payment", XenditResourceId: "qrpy_late2",
		XenditPaymentId: "qrpy_late2", OrderNo: o.OrderNo,
		Status: "SUCCEEDED", PaidAmount: 50_000, RawPayload: []byte(`{}`),
	})
	if out.Result != model.WebhookResultIgnoredExpired {
		t.Fatalf("result: %s", out.Result)
	}
	if calls != 0 {
		t.Fatalf("MUST NOT credit on canceled-order late callback")
	}
	var got model.Order
	_ = db.First(&got, "order_no = ?", o.OrderNo).Error
	if got.Status != model.StatusNeedsManualReview {
		t.Fatalf("status: %s", got.Status)
	}
}

func TestWebhookService_AlreadyCredited(t *testing.T) {
	var calls int32
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls))
	o := seedOrder(t, db, model.StatusCredited, 100)

	out := svc.Process(context.Background(), NormalizedWebhook{
		EventType: "qr.payment", XenditResourceId: "qrpy_already",
		XenditPaymentId: "qrpy_already", OrderNo: o.OrderNo,
		Status: "SUCCEEDED", PaidAmount: 50_000, RawPayload: []byte(`{}`),
	})
	if out.Result != model.WebhookResultSkipDuplicate {
		t.Fatalf("result: %s msg=%s", out.Result, out.Message)
	}
	if calls != 0 {
		t.Fatalf("must not re-call one-api for already-credited order")
	}
}

func TestWebhookService_UnknownOrder(t *testing.T) {
	svc, _, _, _ := setupWebhookEnv(t, happyOneAPIHandler(nil))
	out := svc.Process(context.Background(), NormalizedWebhook{
		EventType: "qr.payment", XenditResourceId: "qrpy_orphan",
		XenditPaymentId: "qrpy_orphan", OrderNo: "IDR_DOES_NOT_EXIST",
		Status: "SUCCEEDED", PaidAmount: 1, RawPayload: []byte(`{}`),
	})
	if out.Result != model.WebhookResultIgnoredNoOrder {
		t.Fatalf("result: %s", out.Result)
	}
}

func TestWebhookService_NonTerminalStatus(t *testing.T) {
	svc, db, _, _ := setupWebhookEnv(t, happyOneAPIHandler(nil))
	o := seedOrder(t, db, model.StatusPending, 100)
	out := svc.Process(context.Background(), NormalizedWebhook{
		EventType: "qr.payment", XenditResourceId: "qrpy_pending",
		XenditPaymentId: "qrpy_pending", OrderNo: o.OrderNo,
		Status: "PENDING", PaidAmount: 0, RawPayload: []byte(`{}`),
	})
	if out.Result != model.WebhookResultIgnoredNonFinal {
		t.Fatalf("result: %s", out.Result)
	}
}

// If one-api refuses (e.g. transient error), order should remain in 'paid'
// so the retry cron picks it up. We must NOT mark it credited.
func TestWebhookService_OneAPIRefusal_LeavesPaidForRetry(t *testing.T) {
	failHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"success":false,"message":"injected failure"}`))
	})
	svc, db, _, _ := setupWebhookEnv(t, failHandler)
	o := seedOrder(t, db, model.StatusPending, 100)

	out := svc.Process(context.Background(), NormalizedWebhook{
		EventType: "qr.payment", XenditResourceId: "qrpy_retry",
		XenditPaymentId: "qrpy_retry", OrderNo: o.OrderNo,
		Status: "SUCCEEDED", PaidAmount: 50_000, RawPayload: []byte(`{}`),
	})
	if out.Result != model.WebhookResultError {
		t.Fatalf("result: %s msg=%s", out.Result, out.Message)
	}
	var got model.Order
	_ = db.First(&got, "order_no = ?", o.OrderNo).Error
	if got.Status != model.StatusPaid {
		t.Fatalf("status: %s (want paid so retry cron picks it up)", got.Status)
	}
}

// Layer 5 + retry: order is in 'paid' (previous credit failed). Replay of
// the webhook should successfully credit it without double-spending via
// the topup_callbacks UNIQUE.
func TestWebhookService_CreditOrder_IdempotentOnTopupCallback(t *testing.T) {
	var calls int32
	svc, db, orderRepo, _ := setupWebhookEnv(t, happyOneAPIHandler(&calls))
	o := seedOrder(t, db, model.StatusPaid, 100)

	if err := svc.CreditOrder(context.Background(), o); err != nil {
		t.Fatalf("credit 1: %v", err)
	}
	// Reload because credit changed status; we can't reuse the stale pointer.
	reload, _ := orderRepo.GetByOrderNo(context.Background(), o.OrderNo)
	if reload.Status != model.StatusCredited {
		t.Fatalf("status after credit: %s", reload.Status)
	}
	if calls != 1 {
		t.Fatalf("calls after first credit: %d", calls)
	}

	// Simulate retry cron calling CreditOrder again. It should be a no-op.
	if err := svc.CreditOrder(context.Background(), reload); err != nil {
		t.Fatalf("credit 2 (retry): %v", err)
	}
	if calls != 1 {
		t.Fatalf("retry must NOT re-call one-api. calls: %d", calls)
	}
}

func TestIsTerminalSuccessStatus(t *testing.T) {
	wantTrue := []string{"SUCCEEDED", "succeeded", "PAID", "Completed", "SETTLED"}
	wantFalse := []string{"PENDING", "ACTIVE", "INACTIVE", "EXPIRED", "FAILED", "", "FOO"}
	for _, s := range wantTrue {
		if !isTerminalSuccessStatus(s) {
			t.Errorf("%q: want true", s)
		}
	}
	for _, s := range wantFalse {
		if isTerminalSuccessStatus(s) {
			t.Errorf("%q: want false", s)
		}
	}
}
