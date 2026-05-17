package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/common/logger"
)

const (
	internalAuthMaxClockSkewSeconds = 60
	internalAuthSecretEnvVar        = "INTERNAL_API_SECRET"
	internalAuthMinSecretLen        = 32

	internalAuthTimestampHeader = "X-Internal-Timestamp"
	internalAuthSignatureHeader = "X-Internal-Signature"
)

// InternalAuth authenticates requests from trusted in-cluster services
// (e.g. the payment-service) using a shared secret and HMAC-SHA256.
//
// Signing scheme — both sides MUST follow it byte-for-byte:
//
//	canonical = timestamp + "\n" + METHOD + "\n" + path + "\n" + hex(sha256(body))
//	signature = hex(hmac_sha256(secret, canonical))
//
//   - timestamp : unix seconds as decimal string, in header X-Internal-Timestamp
//   - METHOD    : HTTP method, uppercase (e.g. "POST")
//   - path      : request URL path only — querystring is NOT included
//   - body      : the raw request body bytes (sha256 of empty body for GET)
//   - signature : lower-case hex, in header X-Internal-Signature
//
// Replay protection: requests with |now - timestamp| > 60s are rejected.
//
// The secret comes from the INTERNAL_API_SECRET environment variable. It
// is intentionally separate from any webhook-token / OAuth secret. A short
// or unset secret causes every request to fail closed.
func InternalAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		secret := strings.TrimSpace(os.Getenv(internalAuthSecretEnvVar))
		if len(secret) < internalAuthMinSecretLen {
			logger.SysError("INTERNAL_API_SECRET is unset or too short (<32 chars); refusing internal request")
			abortInternalAuth(c, http.StatusServiceUnavailable, "internal_auth_not_configured",
				"server not configured for internal auth")
			return
		}

		timestampStr := c.GetHeader(internalAuthTimestampHeader)
		signatureHex := c.GetHeader(internalAuthSignatureHeader)
		if timestampStr == "" || signatureHex == "" {
			abortInternalAuth(c, http.StatusUnauthorized, "missing_internal_auth_headers",
				"X-Internal-Timestamp and X-Internal-Signature are required")
			return
		}

		ts, err := strconv.ParseInt(timestampStr, 10, 64)
		if err != nil {
			abortInternalAuth(c, http.StatusUnauthorized, "invalid_timestamp_format",
				"timestamp must be unix seconds as decimal integer")
			return
		}
		now := time.Now().Unix()
		delta := now - ts
		if delta < 0 {
			delta = -delta
		}
		if delta > internalAuthMaxClockSkewSeconds {
			abortInternalAuth(c, http.StatusUnauthorized, "timestamp_out_of_window",
				"timestamp outside the 60s replay window")
			return
		}

		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			abortInternalAuth(c, http.StatusBadRequest, "cannot_read_body",
				"failed to read request body")
			return
		}
		_ = c.Request.Body.Close()
		// Restore body so the handler can read it again.
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		bodyHash := sha256.Sum256(body)
		canonical := timestampStr + "\n" +
			strings.ToUpper(c.Request.Method) + "\n" +
			c.Request.URL.Path + "\n" +
			hex.EncodeToString(bodyHash[:])

		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(canonical))
		expected := mac.Sum(nil)

		provided, err := hex.DecodeString(strings.TrimSpace(signatureHex))
		if err != nil {
			abortInternalAuth(c, http.StatusUnauthorized, "invalid_signature_format",
				"signature must be lower-case hex")
			return
		}
		if !hmac.Equal(provided, expected) {
			abortInternalAuth(c, http.StatusUnauthorized, "signature_mismatch",
				"signature does not match canonical request")
			return
		}

		c.Set(ctxkey.InternalAuth, true)
		c.Next()
	}
}

func abortInternalAuth(c *gin.Context, status int, code, message string) {
	logger.Errorf(c.Request.Context(),
		"internal auth rejected: code=%s ip=%s path=%s method=%s",
		code, c.ClientIP(), c.Request.URL.Path, c.Request.Method)
	c.AbortWithStatusJSON(status, gin.H{
		"success": false,
		"code":    code,
		"message": message,
	})
}
