package xendit

import (
	"context"
	"time"
)

// CreateVirtualAccountRequest is the body for POST /callback_virtual_accounts.
//
// `bank_code` accepted values include BCA, BNI, BRI, MANDIRI, PERMATA, SAHABAT_SAMPOERNA.
// PR #2 only validates "BCA" upstream, but the Xendit endpoint accepts the rest
// for future expansion.
type CreateVirtualAccountRequest struct {
	ExternalId     string    `json:"external_id"`     // our order_no
	BankCode       string    `json:"bank_code"`       // "BCA"
	Name           string    `json:"name"`            // payer's name (shown in BCA mobile)
	ExpectedAmount int64     `json:"expected_amount"` // IDR integer; required when IsClosed
	IsClosed       bool      `json:"is_closed"`       // true = single-use, fixed-amount
	IsSingleUse    bool      `json:"is_single_use"`   // true = one payment, then auto-closed
	ExpirationDate time.Time `json:"expiration_date,omitempty"`
}

// CreateVirtualAccountResponse mirrors the Xendit response.
type CreateVirtualAccountResponse struct {
	Id             string    `json:"id"`             // xendit_payment_id for VA
	ExternalId     string    `json:"external_id"`    // our order_no
	OwnerId        string    `json:"owner_id"`
	BankCode       string    `json:"bank_code"`
	MerchantCode   string    `json:"merchant_code"`
	Name           string    `json:"name"`
	AccountNumber  string    `json:"account_number"` // user pays to this
	ExpectedAmount int64     `json:"expected_amount"`
	Status         string    `json:"status"`         // ACTIVE / INACTIVE / EXPIRED
	IsClosed       bool      `json:"is_closed"`
	IsSingleUse    bool      `json:"is_single_use"`
	ExpirationDate time.Time `json:"expiration_date"`
	Currency       string    `json:"currency"`
}

// CreateVirtualAccount creates a closed, single-use VA for the given amount.
// `orderNo` is forwarded as external_id and idempotency key.
//
// `payerName` shows up in the customer's mobile banking app - it should
// be the user's display name from one-api when available.
func (c *Client) CreateVirtualAccount(
	ctx context.Context,
	orderNo, bankCode, payerName string,
	amountIDR int64, expiresAt time.Time,
) (*CreateVirtualAccountResponse, error) {
	req := CreateVirtualAccountRequest{
		ExternalId:     orderNo,
		BankCode:       bankCode,
		Name:           payerName,
		ExpectedAmount: amountIDR,
		IsClosed:       true,
		IsSingleUse:    true,
		ExpirationDate: expiresAt,
	}
	var resp CreateVirtualAccountResponse
	if err := c.do(ctx, "POST", "/callback_virtual_accounts", orderNo, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
