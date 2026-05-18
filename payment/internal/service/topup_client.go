package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TopupClient calls one-api's POST /api/internal/topup endpoint. The signing
// scheme must match middleware.InternalAuth in one-api (see PR #1):
//
//	canonical = ts + "\n" + METHOD + "\n" + path + "\n" + hex(sha256(body))
//	signature = hex(hmac_sha256(secret, canonical))
type TopupClient struct {
	baseURL    string
	secret     string
	httpClient *http.Client
}

func NewTopupClient(baseURL, secret string, timeout time.Duration) *TopupClient {
	return &TopupClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		secret:     secret,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// TopupRequest mirrors the body controller/internal_topup.go expects.
type TopupRequest struct {
	Action  string `json:"action"`
	OrderNo string `json:"order_no"`
	UserId  int    `json:"user_id"`
	Quota   int64  `json:"quota"`
	Remark  string `json:"remark,omitempty"`
}

// TopupResponseData mirrors the `data` block of a successful response.
type TopupResponseData struct {
	OrderNo          string `json:"order_no"`
	Action           string `json:"action"`
	UserId           int    `json:"user_id"`
	Quota            int64  `json:"quota"`
	NewBalance       int64  `json:"new_balance"`
	IdempotentReplay bool   `json:"idempotent_replay"`
}

// TopupResponse is the full envelope returned by one-api.
type TopupResponse struct {
	Success bool              `json:"success"`
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Data    *TopupResponseData `json:"data"`
}

// TopupResult bundles the HTTP layer info with the parsed body so callers
// can persist both into topup_callbacks.
type TopupResult struct {
	HTTPStatus int
	RawBody    string
	Parsed     *TopupResponse
}

// Call sends a single POST /api/internal/topup with HMAC signing. Returns
// non-nil error only for network / signing failures; HTTP non-2xx is
// reflected in TopupResult.HTTPStatus and Parsed.Success.
func (c *TopupClient) Call(ctx context.Context, req TopupRequest) (*TopupResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	method := "POST"
	path := "/api/internal/topup"
	url := c.baseURL + path
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	bodyHash := sha256.Sum256(body)
	canonical := ts + "\n" + method + "\n" + path + "\n" + hex.EncodeToString(bodyHash[:])

	mac := hmac.New(sha256.New, []byte(c.secret))
	mac.Write([]byte(canonical))
	sig := hex.EncodeToString(mac.Sum(nil))

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Internal-Timestamp", ts)
	httpReq.Header.Set("X-Internal-Signature", sig)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)

	out := &TopupResult{
		HTTPStatus: resp.StatusCode,
		RawBody:    string(respBody),
	}
	parsed := &TopupResponse{}
	if jsonErr := json.Unmarshal(respBody, parsed); jsonErr == nil {
		out.Parsed = parsed
	}
	return out, nil
}
