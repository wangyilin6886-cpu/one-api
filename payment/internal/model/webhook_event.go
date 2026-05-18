package model

import "time"

// WebhookEventProcessResult enumerates terminal outcomes for a webhook.
type WebhookEventProcessResult string

const (
	WebhookResultOK              WebhookEventProcessResult = "ok"
	WebhookResultSkipDuplicate   WebhookEventProcessResult = "skip_duplicate"
	WebhookResultError           WebhookEventProcessResult = "error"
	WebhookResultIgnoredExpired  WebhookEventProcessResult = "ignored_expired" // late callback against expired/canceled order
	WebhookResultIgnoredNoOrder  WebhookEventProcessResult = "ignored_no_order"
	WebhookResultIgnoredNonFinal WebhookEventProcessResult = "ignored_non_final" // event we don't care about (e.g. invoice.pending)
)

// WebhookEvent records every webhook the service receives - including the
// rejected / duplicate ones. The UNIQUE(event_type, xendit_resource_id)
// constraint is the OUTER layer of webhook idempotency: a second delivery of
// the same logical event from Xendit will fail to INSERT, and we'll treat it
// as a replay.
type WebhookEvent struct {
	Id               int64                     `gorm:"primaryKey;autoIncrement" json:"id"`
	EventType        string                    `gorm:"type:varchar(64);not null;uniqueIndex:uk_webhook_event_resource,priority:1" json:"event_type"`
	XenditResourceId string                    `gorm:"type:varchar(64);not null;uniqueIndex:uk_webhook_event_resource,priority:2" json:"xendit_resource_id"`
	OrderNo          string                    `gorm:"type:varchar(32);index:idx_webhook_events_order_no" json:"order_no"`
	RawPayload       string                    `gorm:"type:mediumtext" json:"raw_payload"`
	Signature        string                    `gorm:"type:varchar(256)" json:"signature"`
	SourceIP         string                    `gorm:"type:varchar(45)" json:"source_ip"`
	ReceivedAt       time.Time                 `gorm:"type:datetime;not null" json:"received_at"`
	ProcessedAt      *time.Time                `gorm:"type:datetime" json:"processed_at,omitempty"`
	ProcessResult    WebhookEventProcessResult `gorm:"type:varchar(24)" json:"process_result"`
	ErrorMsg         string                    `gorm:"type:varchar(512)" json:"error_msg"`
}

func (WebhookEvent) TableName() string { return "webhook_events" }
