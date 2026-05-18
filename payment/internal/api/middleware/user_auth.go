package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	apierrors "github.com/songquanpeng/one-api/payment/internal/errors"
	"github.com/songquanpeng/one-api/payment/internal/logger"
)

// OneAPIUserAuth validates the caller by forwarding the Authorization header
// to one-api's GET /api/user/self endpoint.
//
// On success, the validated user_id and display_name are stashed on the gin
// context for downstream handlers (use UserId(c), UserDisplayName(c)).
//
// Notes:
//   - one-api's /api/user/self accepts the bare access_token (no "Bearer "
//     prefix) per its authHelper(). We forward whatever the client sent;
//     one-api strips "Bearer " itself.
//   - This adds 1 RTT per /orders call. Acceptable for low QPS (top-up flow).
//     If we ever hit ~100 QPS, add a 30-second local cache keyed by token hash.
type OneAPIUserAuth struct {
	BaseURL    string
	Path       string
	HTTPClient *http.Client
}

// NewOneAPIUserAuth builds the middleware factory.
func NewOneAPIUserAuth(baseURL, path string, timeout time.Duration) *OneAPIUserAuth {
	return &OneAPIUserAuth{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Path:       path,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

// Handler is the Gin middleware.
func (a *OneAPIUserAuth) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if auth == "" {
			respondError(c, apierrors.Unauthorized, "Authorization header missing")
			return
		}

		userId, displayName, err := a.validate(c.Request.Context(), auth)
		if err != nil {
			logger.L(c.Request.Context()).Info("user auth failed",
				zap.Error(err), zap.String("path", c.Request.URL.Path))
			respondError(c, apierrors.Unauthorized, "token validation failed: "+err.Error())
			return
		}
		c.Set("payment_user_id", userId)
		c.Set("payment_display_name", displayName)
		c.Next()
	}
}

// validate makes the actual HTTP call. Separated for testability.
func (a *OneAPIUserAuth) validate(ctx context.Context, authHeader string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+a.Path, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, "", errors.New("one-api responded " + resp.Status)
	}

	// one-api returns { "success": bool, "message": str, "data": { "id":..., "display_name":..., ... } }
	var envelope struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Id          int    `json:"id"`
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0, "", err
	}
	if !envelope.Success {
		return 0, "", errors.New(envelope.Message)
	}
	if envelope.Data.Id <= 0 {
		return 0, "", errors.New("user id missing from one-api response")
	}
	name := envelope.Data.DisplayName
	if name == "" {
		name = envelope.Data.Username
	}
	return envelope.Data.Id, name, nil
}

// UserId returns the authenticated user id, or 0 if the middleware did not run.
func UserId(c *gin.Context) int {
	v, _ := c.Get("payment_user_id")
	if n, ok := v.(int); ok {
		return n
	}
	return 0
}

// UserDisplayName returns the authenticated user's display name, or "".
func UserDisplayName(c *gin.Context) string {
	v, _ := c.Get("payment_display_name")
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
