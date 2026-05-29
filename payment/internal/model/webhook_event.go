package model

import "time"

// WebhookEventProcessResult enumerates terminal outcomes for a webhook.
type WebhookEventProcessResult string

const (
	WebhookResultOK              WebhookEventProcessResult = "ok"
	WebhookResultSkipDuplicate   WebhookEventProcessResult = "skip_duplicate"
	WebhookResultError           WebhookEventProcessResult = "error"
	WebhookResultIgnoredExpired  WebhookEventProcessResult = "ignored_expired"   // late callback against expired/canceled order
	WebhookResultIgnoredNoOrder  WebhookEventProcessResult = "ignored_no_order"  // payload had no order_no metadata
	WebhookResultIgnoredNonFinal WebhookEventProcessResult = "ignored_non_final" // event we recognize but don't act on (subscription.created etc when v1 only handles topup)
)

// WebhookEvent records every webhook the service receives - including the
// rejected / duplicate ones. The UNIQUE(event_type, provider_resource_id)
// constraint is the OUTER layer of webhook idempotency: a second delivery
// of the same logical event from the provider will fail to INSERT and we
// treat it as a replay.
//
// `Provider` distinguishes Polar from any future MoR so the unique
// constraint stays meaningful in a multi-provider deployment.
type WebhookEvent struct {
	Id                 int64                     `gorm:"primaryKey;autoIncrement" json:"id"`
	Provider           string                    `gorm:"type:varchar(32);not null;default:'polar'" json:"provider"`
	EventType          string                    `gorm:"type:varchar(64);not null;uniqueIndex:uk_webhook_event_resource,priority:1" json:"event_type"`
	ProviderResourceId string                    `gorm:"type:varchar(128);not null;uniqueIndex:uk_webhook_event_resource,priority:2" json:"provider_resource_id"`
	OrderNo            string                    `gorm:"type:varchar(32);index:idx_webhook_events_order_no" json:"order_no,omitempty"`
	RawPayload         string                    `gorm:"type:mediumtext" json:"raw_payload"`
	Signature          string                    `gorm:"type:varchar(512)" json:"signature,omitempty"`
	SourceIP           string                    `gorm:"type:varchar(45)" json:"source_ip,omitempty"`
	ReceivedAt         time.Time                 `gorm:"type:datetime;not null" json:"received_at"`
	ProcessedAt        *time.Time                `gorm:"type:datetime" json:"processed_at,omitempty"`
	ProcessResult      WebhookEventProcessResult `gorm:"type:varchar(24)" json:"process_result,omitempty"`
	ErrorMsg           string                    `gorm:"type:varchar(512)" json:"error_msg,omitempty"`
}

func (WebhookEvent) TableName() string { return "webhook_events" }
