// Package xendit is a thin, dependency-free HTTP client for the Xendit API.
// We intentionally do not depend on github.com/xendit/xendit-go because the
// SDK is auto-generated and its types are awkward to mock. The Xendit REST
// API is simple enough that a small typed wrapper here gives us better
// control over retry / logging / testability.
//
// All calls share these properties:
//   - Basic Auth: username = secret key, password = empty
//   - Per-attempt timeout (default 10s)
//   - Retry with exponential backoff (1s / 3s / 9s for the 2nd / 3rd / 4th attempt)
//   - Idempotency key forwarded in the X-IDEMPOTENCY-KEY header
//   - Non-2xx responses parsed into XenditAPIError
package xendit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/songquanpeng/one-api/payment/internal/logger"
	"go.uber.org/zap"
)

// Client is the typed Xendit wrapper. Construct via New.
type Client struct {
	baseURL    string
	secretKey  string
	httpClient *http.Client
	retries    int
}

// Options for constructing a Client. Zero-value Timeout / Retries get
// sensible defaults.
type Options struct {
	BaseURL   string
	SecretKey string
	Timeout   time.Duration
	Retries   int
}

func New(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.xendit.co"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Retries == 0 {
		opts.Retries = 3
	}
	return &Client{
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		secretKey:  opts.SecretKey,
		httpClient: &http.Client{Timeout: opts.Timeout},
		retries:    opts.Retries,
	}
}

// XenditAPIError is the typed error returned for non-2xx responses.
type XenditAPIError struct {
	HTTPStatus int    `json:"http_status"`
	ErrorCode  string `json:"error_code"`
	Message    string `json:"message"`
	RawBody    string `json:"raw_body"`
}

func (e *XenditAPIError) Error() string {
	return fmt.Sprintf("xendit api error: status=%d code=%s message=%s",
		e.HTTPStatus, e.ErrorCode, e.Message)
}

// IsRetryable returns true for transient HTTP failures that warrant a retry.
func (e *XenditAPIError) IsRetryable() bool {
	return e.HTTPStatus >= 500 || e.HTTPStatus == http.StatusTooManyRequests
}

// do performs a Xendit POST with retry. requestBody is JSON-marshaled,
// responseInto is JSON-unmarshaled. idempotencyKey is forwarded.
func (c *Client) do(
	ctx context.Context, method, path string,
	idempotencyKey string,
	requestBody any, responseInto any,
) error {
	var bodyBytes []byte
	if requestBody != nil {
		var err error
		bodyBytes, err = json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
	}

	auth := base64.StdEncoding.EncodeToString([]byte(c.secretKey + ":"))
	url := c.baseURL + path

	backoff := []time.Duration{0, 1 * time.Second, 3 * time.Second, 9 * time.Second}
	var lastErr error

	for attempt := 0; attempt < c.retries+1; attempt++ {
		// Each attempt re-creates the request because http.Request.Body is
		// consumed on send.
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Basic "+auth)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if idempotencyKey != "" {
			// Xendit's documented header. Same key prevents duplicate side
			// effects if Xendit replays our request.
			req.Header.Set("X-IDEMPOTENCY-KEY", idempotencyKey)
		}

		if attempt > 0 && attempt < len(backoff) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff[attempt]):
			}
		}

		logger.L(ctx).Debug("xendit call",
			zap.String("method", method), zap.String("url", url),
			zap.Int("attempt", attempt+1), zap.String("idem", idempotencyKey))

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// Network-level error - retry.
			lastErr = fmt.Errorf("http transport: %w", err)
			continue
		}

		respBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if responseInto != nil && len(respBytes) > 0 {
				if err := json.Unmarshal(respBytes, responseInto); err != nil {
					return fmt.Errorf("parse response: %w", err)
				}
			}
			return nil
		}

		apiErr := parseAPIError(resp.StatusCode, respBytes)
		lastErr = apiErr
		if !apiErr.IsRetryable() {
			return apiErr
		}
		logger.L(ctx).Warn("xendit retryable error",
			zap.Int("status", resp.StatusCode),
			zap.String("code", apiErr.ErrorCode),
			zap.Int("attempt", attempt+1))
	}
	return fmt.Errorf("xendit retries exhausted: %w", lastErr)
}

func parseAPIError(status int, body []byte) *XenditAPIError {
	e := &XenditAPIError{HTTPStatus: status, RawBody: string(body)}
	var parsed struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		e.ErrorCode = parsed.ErrorCode
		e.Message = parsed.Message
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
	}
	return e
}

// ErrNotConfigured is returned when callers attempt to use an unconfigured
// Client (e.g. empty secret key).
var ErrNotConfigured = errors.New("xendit client is not configured")
