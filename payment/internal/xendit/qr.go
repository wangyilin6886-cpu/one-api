package xendit

import (
	"context"
	"time"
)

// CreateQRCodeRequest is the body for POST /qr_codes.
//
// Field reference (Xendit "QR Code" API):
//
//	reference_id : our order_no - links back when the qr.payment webhook fires
//	type         : DYNAMIC (recommended) vs STATIC
//	currency     : "IDR" for QRIS
//	amount       : IDR integer
//	expires_at   : ISO-8601 RFC3339
type CreateQRCodeRequest struct {
	ReferenceId string    `json:"reference_id"`
	Type        string    `json:"type"`
	Currency    string    `json:"currency"`
	Amount      int64     `json:"amount"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

// CreateQRCodeResponse mirrors Xendit's response. Only the fields the
// payment-service actually uses are decoded.
type CreateQRCodeResponse struct {
	Id          string    `json:"id"`            // xendit_payment_id for QR
	ReferenceId string    `json:"reference_id"`  // matches the request
	QRString    string    `json:"qr_string"`     // payload encoded into the QR image by the frontend
	Status      string    `json:"status"`        // ACTIVE / INACTIVE / EXPIRED
	Currency    string    `json:"currency"`
	Amount      int64     `json:"amount"`
	ExpiresAt   time.Time `json:"expires_at"`
	Created     time.Time `json:"created"`
}

// CreateQRCode creates a Dynamic QRIS code for the given amount. `orderNo`
// is forwarded both as reference_id and as the idempotency key, so accidental
// duplicate creation requests resolve to the same QR.
func (c *Client) CreateQRCode(
	ctx context.Context, orderNo string, amountIDR int64, expiresAt time.Time,
) (*CreateQRCodeResponse, error) {
	req := CreateQRCodeRequest{
		ReferenceId: orderNo,
		Type:        "DYNAMIC",
		Currency:    "IDR",
		Amount:      amountIDR,
		ExpiresAt:   expiresAt,
	}
	var resp CreateQRCodeResponse
	if err := c.do(ctx, "POST", "/qr_codes", orderNo, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
