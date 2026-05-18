package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/repository"
)

// WebhookService is the orchestrator for incoming Xendit webhooks.
//
// FIVE-LAYER IDEMPOTENCY DEFENSE (corrections 5 & 6 of the spec):
//   1. IP whitelist                              -> middleware/xendit_ip_whitelist.go
//   2. Shared-token verification                 -> middleware/xendit_token.go
//   3. UNIQUE(event_type, xendit_resource_id)    -> here (webhook_events table)
//   4. Order state machine                       -> here (Lock + IsPostPayTerminal)
//   5. UNIQUE(order_no, action_type) on topup_callbacks + one-api UNIQUE(order_no, action) -> here + PR #1
//
// On a late paid-webhook against an expired / canceled / failed order:
//   - DO NOT credit. (Corrections 1, 2.)
//   - Move order -> needs_manual_review.
//   - Mark webhook process_result = ignored_expired.
//   - Append a metadata snippet to the order.
//   - Page humans via Alerter.
type WebhookService struct {
	db            *gorm.DB
	orderRepo     *repository.OrderRepo
	webhookRepo   *repository.WebhookRepo
	topupCBRepo   *repository.TopupCallbackRepo
	topupClient   *TopupClient
	alerter       Alerter
	now           func() time.Time
}

func NewWebhookService(
	db *gorm.DB,
	orderRepo *repository.OrderRepo,
	webhookRepo *repository.WebhookRepo,
	topupCBRepo *repository.TopupCallbackRepo,
	topupClient *TopupClient,
	alerter Alerter,
) *WebhookService {
	return &WebhookService{
		db:          db,
		orderRepo:   orderRepo,
		webhookRepo: webhookRepo,
		topupCBRepo: topupCBRepo,
		topupClient: topupClient,
		alerter:     alerter,
		now:         func() time.Time { return time.Now().UTC() },
	}
}

// NormalizedWebhook is the format every product-specific webhook handler
// converts its payload into before calling Process. Keeps the service
// product-agnostic.
type NormalizedWebhook struct {
	EventType        string // "qr.payment", "virtual_account.payment", "invoice.paid"
	XenditResourceId string // the unique id of THIS webhook event
	XenditPaymentId  string // id of the payment / invoice object that was paid
	OrderNo          string // from reference_id / external_id
	Status           string // SUCCEEDED / PAID / FAILED / EXPIRED ...
	PaidAmount       int64  // IDR amount actually paid; we cross-check against order.amount_idr
	Currency         string // expect "IDR"
	RawPayload       []byte // full JSON, persisted for forensic review
	Signature        string // copy of the auth header for audit
	SourceIP         string
}

// ProcessOutcome carries the result of Process so the handler can reply to
// Xendit. Xendit treats any non-2xx as "redeliver", so we always 200 OK
// unless we want a retry.
type ProcessOutcome struct {
	Result  model.WebhookEventProcessResult
	Code    string // for the response body's "code" field; "ok" / "duplicate" / etc
	Message string // human-readable; English only (audience is Xendit log)
}

// Process is the entry point. It implements the five-layer defense for one
// normalized webhook delivery.
func (s *WebhookService) Process(ctx context.Context, w NormalizedWebhook) ProcessOutcome {
	ctx = logger.WithFields(ctx,
		zap.String("event_type", w.EventType),
		zap.String("resource_id", w.XenditResourceId),
		zap.String("order_no", w.OrderNo),
	)

	// Layer 3: DB-level dedupe.
	event := &model.WebhookEvent{
		EventType:        w.EventType,
		XenditResourceId: w.XenditResourceId,
		OrderNo:          w.OrderNo,
		RawPayload:       string(w.RawPayload),
		Signature:        w.Signature,
		SourceIP:         w.SourceIP,
		ReceivedAt:       s.now(),
	}
	if err := s.webhookRepo.Insert(ctx, event); err != nil {
		if errors.Is(err, repository.ErrDuplicateWebhook) {
			logger.L(ctx).Info("webhook duplicate (already processed)")
			return ProcessOutcome{
				Result: model.WebhookResultSkipDuplicate, Code: "duplicate",
				Message: "webhook already processed",
			}
		}
		logger.L(ctx).Error("webhook insert failed", zap.Error(err))
		return ProcessOutcome{
			Result: model.WebhookResultError, Code: "db_error",
			Message: "failed to record webhook event",
		}
	}

	outcome := s.processInsertedEvent(ctx, event, w)
	if err := s.webhookRepo.MarkProcessed(ctx, event.Id, outcome.Result, outcome.Message); err != nil {
		logger.L(ctx).Error("webhook MarkProcessed failed", zap.Error(err))
	}
	return outcome
}

// processInsertedEvent runs steps 4 and 5 of the defense.
func (s *WebhookService) processInsertedEvent(
	ctx context.Context, event *model.WebhookEvent, w NormalizedWebhook,
) ProcessOutcome {

	if !isTerminalSuccessStatus(w.Status) {
		logger.L(ctx).Info("webhook ignored (non-terminal status)",
			zap.String("status", w.Status))
		return ProcessOutcome{
			Result: model.WebhookResultIgnoredNonFinal, Code: "ignored",
			Message: "non-terminal payment status; nothing to do",
		}
	}

	if w.OrderNo == "" {
		// Can't even find the order. Probably a payload format issue.
		s.alerter.NeedsManualReview(ctx, "(unknown)", "webhook has no order_no",
			zap.String("event_type", w.EventType),
			zap.String("resource_id", w.XenditResourceId))
		return ProcessOutcome{
			Result: model.WebhookResultIgnoredNoOrder, Code: "no_order",
			Message: "webhook payload missing order reference",
		}
	}

	// Layer 4: order state machine under SELECT FOR UPDATE. We use enum
	// flags rather than sentinel errors to communicate outcomes, because
	// returning an error from a GORM Transaction closure rolls back the
	// transaction - including the status / metadata updates we just made
	// in the late-callback branch.
	type txOutcome int
	const (
		outcomeCommit txOutcome = iota
		outcomeOrderNotFound
		outcomeEscalated
		outcomeAlreadyCredited
	)
	var outcome txOutcome
	var creditNeeded bool
	var order *model.Order

	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		order, err = s.orderRepo.LockByOrderNo(ctx, tx, w.OrderNo)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				outcome = outcomeOrderNotFound
				return nil // commit empty tx; we still inserted the webhook_event in outer scope
			}
			return err
		}

		// Correction 1 & 2: late callbacks against terminal-non-credited orders.
		// We MUST commit the tx so the metadata + status update land.
		if order.IsPostPayTerminal() {
			snippet := fmt.Sprintf(
				"late callback received at %s; webhook_event_id=%d; xendit_payment_id=%s; prior_status=%s",
				s.now().Format(time.RFC3339), event.Id, w.XenditPaymentId, order.Status,
			)
			if err := s.orderRepo.AppendMetadata(ctx, tx, order.OrderNo, snippet); err != nil {
				return fmt.Errorf("append metadata: %w", err)
			}
			if order.Status != model.StatusNeedsManualReview {
				if err := order.CanTransition(model.StatusNeedsManualReview); err != nil {
					return err
				}
				if err := s.orderRepo.UpdateStatusAndFields(ctx, tx, order.OrderNo,
					model.StatusNeedsManualReview, map[string]any{
						"xendit_payment_id": w.XenditPaymentId,
					}); err != nil {
					return err
				}
			}
			outcome = outcomeEscalated
			return nil
		}

		// pending -> paid (the normal case): record paid_at, persist payment id,
		// and flag that we should attempt to credit after the tx commits.
		if order.Status == model.StatusPending {
			now := s.now()
			if err := order.CanTransition(model.StatusPaid); err != nil {
				return err
			}
			if err := s.orderRepo.UpdateStatusAndFields(ctx, tx, order.OrderNo,
				model.StatusPaid, map[string]any{
					"paid_at":           now,
					"xendit_payment_id": w.XenditPaymentId,
				}); err != nil {
				return err
			}
			// Keep the in-memory pointer in sync so subsequent (post-tx)
			// state-machine checks see the new status.
			order.Status = model.StatusPaid
			order.PaidAt = &now
			creditNeeded = true
			return nil
		}

		// paid -> still paid: maybe a duplicate event or our previous credit
		// attempt failed. Re-try the credit, which is itself idempotent.
		if order.Status == model.StatusPaid {
			creditNeeded = true
			return nil
		}

		// credited -> credited: nothing to do, already done.
		if order.Status == model.StatusCredited {
			outcome = outcomeAlreadyCredited
			return nil
		}

		// Anything else (refunding etc) shouldn't reach here under the
		// current state machine, but be defensive.
		return fmt.Errorf("unexpected order status: %s", order.Status)
	})

	if txErr != nil {
		logger.L(ctx).Error("webhook processing tx failed", zap.Error(txErr))
		return ProcessOutcome{
			Result: model.WebhookResultError, Code: "db_error",
			Message: txErr.Error(),
		}
	}

	switch outcome {
	case outcomeOrderNotFound:
		s.alerter.NeedsManualReview(ctx, w.OrderNo,
			"webhook for unknown order_no",
			zap.String("event_type", w.EventType),
			zap.String("resource_id", w.XenditResourceId))
		return ProcessOutcome{
			Result: model.WebhookResultIgnoredNoOrder, Code: "no_order",
			Message: "order_no not found",
		}
	case outcomeEscalated:
		s.alerter.NeedsManualReview(ctx, w.OrderNo,
			"late paid webhook against terminal-non-credited order",
			zap.String("event_type", w.EventType),
			zap.String("resource_id", w.XenditResourceId))
		return ProcessOutcome{
			Result: model.WebhookResultIgnoredExpired, Code: "ignored_expired",
			Message: "order already terminal; escalated to needs_manual_review",
		}
	case outcomeAlreadyCredited:
		return ProcessOutcome{
			Result: model.WebhookResultSkipDuplicate, Code: "duplicate",
			Message: "order already credited",
		}
	}

	// Verification: paid amount should match order amount. A mismatch is a
	// serious anomaly - probably a user overpaid via VA, or Xendit billed
	// the wrong amount. Escalate.
	if w.PaidAmount > 0 && order != nil && w.PaidAmount != order.AmountIDR {
		s.alerter.NeedsManualReview(ctx, w.OrderNo,
			"paid amount does not match order amount",
			zap.Int64("paid", w.PaidAmount),
			zap.Int64("expected", order.AmountIDR))
		// We still credit at order.QuotaToCredit (the locked value) to
		// avoid awarding more than was promised. Operators handle the
		// delta out-of-band.
	}

	if !creditNeeded {
		return ProcessOutcome{Result: model.WebhookResultOK, Code: "ok", Message: "no credit needed"}
	}

	// Layer 5: topup_callbacks UNIQUE + one-api UNIQUE. CreditOrder handles
	// "already done" by reading the existing callback row.
	if err := s.CreditOrder(ctx, order); err != nil {
		logger.L(ctx).Error("credit-to-oneapi failed; will retry via cron", zap.Error(err))
		s.alerter.OneAPIError(ctx, order.OrderNo, "topup", err)
		// Order stays as 'paid'; topup_retry cron will pick it up.
		return ProcessOutcome{
			Result: model.WebhookResultError, Code: "credit_failed",
			Message: "credit deferred to retry: " + err.Error(),
		}
	}

	return ProcessOutcome{Result: model.WebhookResultOK, Code: "ok", Message: "credited"}
}

// CreditOrder is the public entry point for crediting a paid order. Used
// both by Process and by the topup_retry cron.
//
// Sequence:
//  1. INSERT topup_callbacks with status=pending (UNIQUE blocks duplicate).
//     If already exists and previously succeeded, treat as a no-op.
//     If already exists but failed, increment attempt and retry.
//  2. Call one-api /api/internal/topup. The endpoint is itself idempotent.
//  3. On success: mark callback success, transition order -> credited,
//     set credited_at + quota_credited.
//  4. On failure: mark callback failure; leave order in 'paid' so cron retries.
func (s *WebhookService) CreditOrder(ctx context.Context, order *model.Order) error {
	const action = model.TopupCallbackActionTopup

	cb := &model.TopupCallback{
		OrderNo:    order.OrderNo,
		ActionType: action,
		UserId:     order.UserId,
		Quota:      order.QuotaToCredit,
	}

	insertErr := s.topupCBRepo.InsertPending(ctx, cb)
	if insertErr != nil {
		if !errors.Is(insertErr, repository.ErrDuplicateTopupCallback) {
			return fmt.Errorf("topup_callbacks insert: %w", insertErr)
		}
		// Already attempted at least once. Reuse the existing row.
		existing, err := s.topupCBRepo.GetByOrderAction(ctx, order.OrderNo, action)
		if err != nil {
			return fmt.Errorf("topup_callbacks reload: %w", err)
		}
		cb = existing
		if cb.IsSuccess() {
			// Already credited on the one-api side. Make sure our order
			// reflects that. (Defensive: usually CreditOrder finalizes the
			// order in the same call that flips the callback.)
			return s.finalizeCreditedOrder(ctx, order, cb)
		}
	}

	req := TopupRequest{
		Action:  "topup",
		OrderNo: order.OrderNo,
		UserId:  order.UserId,
		Quota:   order.QuotaToCredit,
		Remark:  fmt.Sprintf("xendit %s %d IDR", order.PaymentMethod, order.AmountIDR),
	}
	reqJSON := mustJSON(req)
	_ = s.db.WithContext(ctx).
		Model(&model.TopupCallback{}).Where("id = ?", cb.Id).
		Update("request_body", reqJSON).Error

	result, err := s.topupClient.Call(ctx, req)
	if err != nil {
		_ = s.topupCBRepo.MarkFailure(ctx, cb.Id, 0, "transport: "+err.Error())
		return fmt.Errorf("topup_client call: %w", err)
	}
	if result.Parsed == nil || !result.Parsed.Success {
		body := result.RawBody
		_ = s.topupCBRepo.MarkFailure(ctx, cb.Id, result.HTTPStatus, body)
		return fmt.Errorf("one-api refused: http=%d body=%s", result.HTTPStatus, body)
	}
	if err := s.topupCBRepo.MarkSuccess(ctx, cb.Id, result.HTTPStatus, result.RawBody); err != nil {
		// We've actually credited - DB write to mark success failing is a
		// monitoring concern but the credit is done.
		logger.L(ctx).Error("topup_callbacks MarkSuccess failed",
			zap.String("order_no", order.OrderNo), zap.Error(err))
	}

	return s.finalizeCreditedOrder(ctx, order, cb)
}

func (s *WebhookService) finalizeCreditedOrder(
	ctx context.Context, order *model.Order, cb *model.TopupCallback,
) error {
	if order.Status == model.StatusCredited {
		return nil // already finalized
	}
	now := s.now()
	if err := order.CanTransition(model.StatusCredited); err != nil {
		return fmt.Errorf("order finalize: %w", err)
	}
	return s.orderRepo.UpdateStatusAndFields(ctx, nil, order.OrderNo,
		model.StatusCredited, map[string]any{
			"credited_at":    now,
			"quota_credited": cb.Quota,
		})
}

// isTerminalSuccessStatus tells if a Xendit status string means "the user
// has actually paid". Xendit uses different strings for different products.
func isTerminalSuccessStatus(status string) bool {
	switch strings.ToUpper(status) {
	case "SUCCEEDED", "PAID", "COMPLETED", "SETTLED":
		return true
	}
	return false
}

func mustJSON(v any) string {
	b, _ := jsonMarshal(v)
	return string(b)
}

// Indirect for tests; isolated so we can swap to a deterministic encoder
// later if needed.
var jsonMarshal = func(v any) ([]byte, error) {
	return jsonStdMarshal(v)
}

