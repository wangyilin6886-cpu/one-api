package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const testInternalSecret = "test-secret-must-be-at-least-32-bytes!!"

func init() {
	gin.SetMode(gin.TestMode)
}

func newSignedRequest(t *testing.T, method, path string, body []byte, ts int64, secret string) *http.Request {
	t.Helper()
	bodyHash := sha256.Sum256(body)
	canonical := strconv.FormatInt(ts, 10) + "\n" +
		strings.ToUpper(method) + "\n" +
		path + "\n" +
		hex.EncodeToString(bodyHash[:])
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	sig := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("X-Internal-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Internal-Signature", sig)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func runWithSecret(secret string, fn func()) {
	prev, hadPrev := os.LookupEnv(internalAuthSecretEnvVar)
	_ = os.Setenv(internalAuthSecretEnvVar, secret)
	defer func() {
		if hadPrev {
			_ = os.Setenv(internalAuthSecretEnvVar, prev)
		} else {
			_ = os.Unsetenv(internalAuthSecretEnvVar)
		}
	}()
	fn()
}

func runMiddleware(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := gin.New()
	bodyRead := false
	r.POST("/api/internal/topup", InternalAuth(), func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		bodyRead = len(b) >= 0
		c.JSON(http.StatusOK, gin.H{"success": true, "read": bodyRead})
	})
	r.GET("/api/internal/topup", InternalAuth(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"success": true})
	})
	r.ServeHTTP(w, req)
	return w
}

func TestInternalAuth_HappyPath(t *testing.T) {
	runWithSecret(testInternalSecret, func() {
		body := []byte(`{"hello":"world"}`)
		req := newSignedRequest(t, "POST", "/api/internal/topup", body, time.Now().Unix(), testInternalSecret)
		w := runMiddleware(req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"success":true`) {
			t.Fatalf("body did not contain success=true: %s", w.Body.String())
		}
	})
}

func TestInternalAuth_BodyAvailableAfterMiddleware(t *testing.T) {
	runWithSecret(testInternalSecret, func() {
		body := []byte(`{"order_no":"IDR1234567890"}`)
		req := newSignedRequest(t, "POST", "/api/internal/topup", body, time.Now().Unix(), testInternalSecret)

		w := httptest.NewRecorder()
		r := gin.New()
		r.POST("/api/internal/topup", InternalAuth(), func(c *gin.Context) {
			b, err := io.ReadAll(c.Request.Body)
			if err != nil {
				c.JSON(500, gin.H{"err": err.Error()})
				return
			}
			c.Data(200, "application/octet-stream", b)
		})
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), body) {
			t.Fatalf("body lost in middleware. got=%q want=%q", w.Body.String(), string(body))
		}
	})
}

func TestInternalAuth_MissingSecret(t *testing.T) {
	_ = os.Unsetenv(internalAuthSecretEnvVar)
	body := []byte(`{}`)
	req := newSignedRequest(t, "POST", "/api/internal/topup", body, time.Now().Unix(), testInternalSecret)
	w := runMiddleware(req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInternalAuth_ShortSecret(t *testing.T) {
	runWithSecret("too-short", func() {
		body := []byte(`{}`)
		req := newSignedRequest(t, "POST", "/api/internal/topup", body, time.Now().Unix(), "too-short")
		w := runMiddleware(req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for short secret, got %d", w.Code)
		}
	})
}

func TestInternalAuth_MissingHeaders(t *testing.T) {
	runWithSecret(testInternalSecret, func() {
		req := httptest.NewRequest("POST", "/api/internal/topup", bytes.NewReader([]byte(`{}`)))
		w := runMiddleware(req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
	})
}

func TestInternalAuth_TimestampReplay(t *testing.T) {
	runWithSecret(testInternalSecret, func() {
		oldTs := time.Now().Unix() - 120 // 2 min ago
		body := []byte(`{}`)
		req := newSignedRequest(t, "POST", "/api/internal/topup", body, oldTs, testInternalSecret)
		w := runMiddleware(req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for stale timestamp, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "timestamp_out_of_window") {
			t.Fatalf("expected timestamp_out_of_window in body, got: %s", w.Body.String())
		}
	})
}

func TestInternalAuth_FutureTimestamp(t *testing.T) {
	runWithSecret(testInternalSecret, func() {
		futureTs := time.Now().Unix() + 120
		body := []byte(`{}`)
		req := newSignedRequest(t, "POST", "/api/internal/topup", body, futureTs, testInternalSecret)
		w := runMiddleware(req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for future timestamp, got %d", w.Code)
		}
	})
}

func TestInternalAuth_SignatureMismatch(t *testing.T) {
	runWithSecret(testInternalSecret, func() {
		body := []byte(`{"a":1}`)
		req := newSignedRequest(t, "POST", "/api/internal/topup", body, time.Now().Unix(), "wrong-secret-which-is-also-32-bytes!")
		w := runMiddleware(req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "signature_mismatch") {
			t.Fatalf("expected signature_mismatch, got %s", w.Body.String())
		}
	})
}

func TestInternalAuth_TamperedBody(t *testing.T) {
	runWithSecret(testInternalSecret, func() {
		body := []byte(`{"a":1}`)
		ts := time.Now().Unix()
		req := newSignedRequest(t, "POST", "/api/internal/topup", body, ts, testInternalSecret)
		// swap body after signing
		req.Body = io.NopCloser(bytes.NewReader([]byte(`{"a":2}`)))
		w := runMiddleware(req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for tampered body, got %d", w.Code)
		}
	})
}

func TestInternalAuth_TamperedPath(t *testing.T) {
	runWithSecret(testInternalSecret, func() {
		body := []byte(`{}`)
		ts := time.Now().Unix()
		// sign for /api/internal/topup but send to a different path
		bodyHash := sha256.Sum256(body)
		canonical := strconv.FormatInt(ts, 10) + "\nPOST\n/api/internal/topup\n" + hex.EncodeToString(bodyHash[:])
		mac := hmac.New(sha256.New, []byte(testInternalSecret))
		mac.Write([]byte(canonical))
		sig := hex.EncodeToString(mac.Sum(nil))

		req := httptest.NewRequest("POST", "/api/internal/other", bytes.NewReader(body))
		req.Header.Set("X-Internal-Timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("X-Internal-Signature", sig)

		w := httptest.NewRecorder()
		r := gin.New()
		r.POST("/api/internal/other", InternalAuth(), func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for path mismatch, got %d", w.Code)
		}
	})
}

func TestInternalAuth_QueryStringNotSigned(t *testing.T) {
	// querystring is intentionally not part of the signature; querystring may
	// vary between dev/prod but the canonical request stays stable.
	runWithSecret(testInternalSecret, func() {
		body := []byte(`{}`)
		ts := time.Now().Unix()
		// Sign on the bare path (no query)
		req := newSignedRequest(t, "POST", "/api/internal/topup", body, ts, testInternalSecret)
		// Re-target the request to include a querystring, leaving headers intact
		req.URL.RawQuery = "ignored=1"
		w := runMiddleware(req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestInternalAuth_InvalidSignatureHex(t *testing.T) {
	runWithSecret(testInternalSecret, func() {
		req := httptest.NewRequest("POST", "/api/internal/topup", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("X-Internal-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
		req.Header.Set("X-Internal-Signature", "nothex!")
		w := runMiddleware(req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
	})
}
