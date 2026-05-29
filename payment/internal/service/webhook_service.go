package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/provider"
	"github.com/songquanpeng/one-api/payment/internal/repository"
)

// WebhookService orchestrates inbound webhook events from the payment
// provider.
//
// FIVE-LAYER IDEMPOTENCY DEFENSE:
//
//   1. Provider signature verification          -> middleware/polar_signature.go (PR-B)
//   2. (reserved)                               -> we used to do IP whitelisting for Xendit;
//                                                  Polar uses HMAC signatures over the body,
//                                                  no IP allowlist needed.
//   3. UNIQUE(event_type, provider_resource_id) -> here (webhook_events table)
//   4. Order state machine                      -> here (Lock + IsPostPayTerminal)
//   5. UNIQUE(order_no, action_type) on topup_callbacks
//      + one-api's UNIQUE(order_no, action) on internal_topup_records (PR #1)
//                                              -> here + one-api side
//
// Late-callback handling: on a `checkout.completed`-style webhook arriving
// after the order is already expired/canceled/failed:
//   - DO NOT credit.
//   - Move the order -> needs_manual_review.
//   - Append a metadata snippet for forensics.
//   - Page humans via Alerter.
type WebhookService struct {
	db          *gorm.DB
	orderRepo   *repository.OrderRepo
	webhookRepo *repository.WebhookRepo
	topupCBRepo *repository.TopupCallbackRepo
	topupClient *TopupClient
	provider    provider.PaymentProvider
	alerter     Alerter
	now         func() time.Time
}

func NewWebhookService(
	db *gorm.DB,
	orderRepo *repository.OrderRepo,
	webhookRepo *repository.WebhookRepo,
	topupCBRepo *repository.TopupCallbackRepo,
	topupClient *TopupClient,
	prov provider.PaymentProvider,
	alerter Alerter,
) *WebhookService {
	return &WebhookService{
		db:          db,
		orderRepo:   orderRepo,
		webhookRepo: webhookRepo,
		topupCBRepo: topupCBRepo,
		topupClient: topupClient,
		provider:    prov,
		alerter:     alerter,
		now:         func() time.Time { return time.Now().UTC() },
	}
}

// IncomingWebhook is the wire-side bundle the HTTP handler hands us. It
// contains the raw payload and headers (for signature verification + audit)
// plus the source IP for the audit row.
type IncomingWebhook struct {
	RawPayload []byte
	Headers    map[string]string
	SourceIP   string
}

// ProcessOutcome carries the result of Process so the handler can reply to
// the provider. 200 OK by default; only mid-process DB-write failures get
// a 5xx (so the provider retries).
type ProcessOutcome struct {
	Result  model.WebhookEventProcessResult
	Code    string // for the response body's "code" field; "ok" / "duplicate" / etc
	Message string // English; audience is the provider's webhook log
}

// Process is the entry point for one webhook delivery.
//
//   1. Ask the provider adapter to verify the signature and normalize the payload.
//   2. INSERT into webhook_events; UNIQUE collision -> duplicate replay.
//   3. Apply the state-machine update (with SELECT FOR UPDATE).
//   4. Trigger credit if the event was a successful payment.
func (s *WebhookService) Process(ctx context.Context, in IncomingWebhook) ProcessOutcome {
	ev, err := s.provider.VerifyWebhook(ctx, in.RawPayload, in.Headers)
	if err != nil {
		if errors.Is(err, provider.ErrInvalidSignature) {
			logger.L(ctx).Warn("webhook signature verification failed",
				zap.String("source_ip", in.SourceIP), zap.Error(err))
			return ProcessOutcome{
				Result: model.WebhookResultError, Code: "bad_signature",
				Message: "signature verification failed",
			}
		}
		if errors.Is(err, provider.ErrUnrecognizedEvent) {
			// Recognized shape, unmapped type. Drop quietly so we don't get
			// stuck retrying on events we never agreed to handle.
			logger.L(ctx).Info("webhook ignored (unrecognized event type)")
			return ProcessOutcome{
				Result: model.WebhookResultIgnoredNonFinal, Code: "ignored",
				Message: "event type not handled by this version",
			}
		}
		logger.L(ctx).Error("webhook parse failed", zap.Error(err))
		return ProcessOutcome{
			Result: model.WebhookResultError, Code: "parse_error",
			Message: err.Error(),
		}
	}

	ctx = logger.WithFields(ctx,
		zap.String("event_type", string(ev.Type)),
		zap.String("resource_id", ev.ProviderResourceId),
		zap.String("order_no", ev.OrderNo),
	)

	// Layer 3: DB-level dedupe.
	row := &model.WebhookEvent{
		Provider:           s.provider.Name(),
		EventType:          string(ev.Type),
		ProviderResourceId: ev.ProviderResourceId,
		OrderNo:            ev.OrderNo,
		RawPayload:         string(in.RawPayload),
		Signature:          in.Headers["webhook-signature"],
		SourceIP:           in.SourceIP,
		ReceivedAt:         s.now(),
	}
	if err := s.webhookRepo.Insert(ctx, row); err != nil {
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

	outcome := s.processInsertedEvent(ctx, row, ev)
	if err := s.webhookRepo.MarkProcessed(ctx, row.Id, outcome.Result, outcome.Message); err != nil {
		logger.L(ctx).Error("webhook MarkProcessed failed", zap.Error(err))
	}
	return outcome
}

// processInsertedEvent runs layers 4 and 5 of the defense.
func (s *WebhookService) processInsertedEvent(
	ctx context.Context, row *model.WebhookEvent, ev *provider.NormalizedEvent,
) ProcessOutcome {

	// Only act on events that mean "money settled". Other recognized types
	// (subscription.created etc.) are recorded but not credited in PR-B's scope.
	if !isCreditTriggering(ev.Type) {
		logger.L(ctx).Info("webhook recorded; no credit action for this event type")
		return ProcessOutcome{
			Result: model.WebhookResultIgnoredNonFinal, Code: "ignored",
			Message: "event recorded but no credit action defined",
		}
	}

	if ev.OrderNo == "" {
		s.alerter.NeedsManualReview(ctx, "(unknown)",
			"webhook has no order_no metadata",
			zap.String("event_type", string(ev.Type)),
			zap.String("resource_id", ev.ProviderResourceId))
		return ProcessOutcome{
			Result: model.WebhookResultIgnoredNoOrder, Code: "no_order",
			Message: "webhook payload missing order reference",
		}
	}

	// Layer 4: order state machine under SELECT FOR UPDATE. We use enum
	// flags rather than sentinel errors because returning an error from a
	// GORM Transaction closure rolls back the tx - and we need the late-
	// callback metadata + status updates to commit.
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
		order, err = s.orderRepo.LockByOrderNo(ctx, tx, ev.OrderNo)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				outcome = outcomeOrderNotFound
				return nil
			}
			return err
		}

		// Late callback against a terminal-non-credited order.
		if order.IsPostPayTerminal() {
			snippet := fmt.Sprintf(
				"late callback received at %s; webhook_event_id=%d; provider_payment_id=%s; prior_status=%s",
				s.now().Format(time.RFC3339), row.Id, ev.ProviderPaymentId, order.Status,
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
						"provider_payment_id": ev.ProviderPaymentId,
					}); err != nil {
					return err
				}
			}
			outcome = outcomeEscalated
			return nil
		}

		// pending -> paid: record paid_at + provider_payment_id, flag for
		// post-tx credit.
		if order.Status == model.StatusPending {
			now := s.now()
			if err := order.CanTransition(model.StatusPaid); err != nil {
				return err
			}
			if err := s.orderRepo.UpdateStatusAndFields(ctx, tx, order.OrderNo,
				model.StatusPaid, map[string]any{
					"paid_at":             now,
					"provider_payment_id": ev.ProviderPaymentId,
				}); err != nil {
				return err
			}
			// Keep in-memory pointer consistent so post-tx state machine
			// checks see the new status.
			order.Status = model.StatusPaid
			order.PaidAt = &now
			creditNeeded = true
			return nil
		}

		// paid -> still paid: prior credit attempt failed, retry it.
		if order.Status == model.StatusPaid {
			creditNeeded = true
			return nil
		}

		// credited -> credited: done.
		if order.Status == model.StatusCredited {
			outcome = outcomeAlreadyCredited
			return nil
		}

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
		s.alerter.NeedsManualReview(ctx, ev.OrderNo,
			"webhook for unknown order_no",
			zap.String("event_type", string(ev.Type)),
			zap.String("resource_id", ev.ProviderResourceId))
		return ProcessOutcome{
			Result: model.WebhookResultIgnoredNoOrder, Code: "no_order",
			Message: "order_no not found",
		}
	case outcomeEscalated:
		s.alerter.NeedsManualReview(ctx, ev.OrderNo,
			"late paid webhook against terminal-non-credited order",
			zap.String("event_type", string(ev.Type)),
			zap.String("resource_id", ev.ProviderResourceId))
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

	// Currency / amount cross-check. A mismatch likely means we mis-priced
	// the order vs what the provider billed; we still credit at the locked
	// quota_to_credit but page humans on the delta.
	if order != nil && ev.AmountUSDCents > 0 && ev.AmountUSDCents != order.AmountUSDCents {
		s.alerter.NeedsManualReview(ctx, ev.OrderNo,
			"paid amount does not match order amount",
			zap.Int64("paid_cents", ev.AmountUSDCents),
			zap.Int64("expected_cents", order.AmountUSDCents))
	}
	if order != nil && ev.Currency != "" && ev.Currency != order.Currency {
		s.alerter.NeedsManualReview(ctx, ev.OrderNo,
			"paid currency does not match order currency",
			zap.String("paid_currency", ev.Currency),
			zap.String("expected_currency", order.Currency))
	}

	if !creditNeeded {
		return ProcessOutcome{Result: model.WebhookResultOK, Code: "ok", Message: "no credit needed"}
	}

	// Layer 5: topup_callbacks UNIQUE + one-api internal_topup_records UNIQUE.
	if err := s.CreditOrder(ctx, order); err != nil {
		logger.L(ctx).Error("credit-to-oneapi failed; will retry via cron", zap.Error(err))
		s.alerter.OneAPIError(ctx, order.OrderNo, "topup", err)
		// Order stays in `paid`; topup_retry cron picks it up.
		return ProcessOutcome{
			Result: model.WebhookResultError, Code: "credit_failed",
			Message: "credit deferred to retry: " + err.Error(),
		}
	}

	return ProcessOutcome{Result: model.WebhookResultOK, Code: "ok", Message: "credited"}
}

// CreditOrder is the public entry point for crediting a paid order. Used
// both by Process and by the topup_retry cron.
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
		existing, err := s.topupCBRepo.GetByOrderAction(ctx, order.OrderNo, action)
		if err != nil {
			return fmt.Errorf("topup_callbacks reload: %w", err)
		}
		cb = existing
		if cb.IsSuccess() {
			return s.finalizeCreditedOrder(ctx, order, cb)
		}
	}

	req := TopupRequest{
		Action:  "topup",
		OrderNo: order.OrderNo,
		UserId:  order.UserId,
		Quota:   order.QuotaToCredit,
		Remark:  fmt.Sprintf("%s %d %s", order.Provider, order.AmountUSDCents, order.Currency),
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
		logger.L(ctx).Error("topup_callbacks MarkSuccess failed",
			zap.String("order_no", order.OrderNo), zap.Error(err))
	}

	return s.finalizeCreditedOrder(ctx, order, cb)
}

func (s *WebhookService) finalizeCreditedOrder(
	ctx context.Context, order *model.Order, cb *model.TopupCallback,
) error {
	if order.Status == model.StatusCredited {
		return nil
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

// isCreditTriggering returns true for normalized event types that mean
// "money has settled; credit the user."
func isCreditTriggering(t provider.EventType) bool {
	switch t {
	case provider.EventCheckoutCompleted,
		provider.EventSubscriptionCreated, // first cycle credits on creation
		provider.EventSubscriptionRenewed:
		return true
	}
	return false
}

func mustJSON(v any) string {
	b, _ := jsonMarshal(v)
	return string(b)
}

var jsonMarshal = func(v any) ([]byte, error) {
	return jsonStdMarshal(v)
}
