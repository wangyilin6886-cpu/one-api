package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/repository"
)

func init() { gin.SetMode(gin.TestMode) }

func newConfigRepo(t *testing.T, whitelistJSON string) *repository.PaymentConfigRepo {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	if err := db.AutoMigrate(&model.PaymentConfig{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	r := repository.NewPaymentConfigRepo(db, 10*time.Millisecond)
	if whitelistJSON != "" {
		if err := r.Set(context.Background(), model.CfgXenditIPWhitelistJSON, whitelistJSON, 0); err != nil {
			t.Fatalf("set: %v", err)
		}
	}
	return r
}

func newTestRouter(repo *repository.PaymentConfigRepo, trustForwarded bool) *gin.Engine {
	r := gin.New()
	mw := XenditIPWhitelist(XenditIPWhitelistOptions{
		ConfigRepo: repo, CacheTTL: 10 * time.Millisecond, TrustForwardedFor: trustForwarded,
	})
	r.POST("/webhooks/xendit/qris", mw, func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})
	return r
}

func TestIPWhitelist_AllowsListedCIDR(t *testing.T) {
	repo := newConfigRepo(t, `["127.0.0.0/8"]`)
	r := newTestRouter(repo, false)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/webhooks/xendit/qris", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
}

func TestIPWhitelist_RejectsUnlisted(t *testing.T) {
	repo := newConfigRepo(t, `["10.0.0.0/8"]`)
	r := newTestRouter(repo, false)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/webhooks/xendit/qris", nil)
	req.RemoteAddr = "8.8.8.8:1234"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "IP_NOT_ALLOWED" {
		t.Fatalf("code: %v", body["code"])
	}
}

func TestIPWhitelist_BareIPInList(t *testing.T) {
	// Spec says JSON array of IPs; we accept bare IPs (auto /32) as well as CIDRs.
	repo := newConfigRepo(t, `["1.2.3.4"]`)
	r := newTestRouter(repo, false)
	for ip, wantOK := range map[string]bool{"1.2.3.4:5": true, "1.2.3.5:5": false} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/webhooks/xendit/qris", nil)
		req.RemoteAddr = ip
		r.ServeHTTP(w, req)
		got := w.Code == http.StatusOK
		if got != wantOK {
			t.Errorf("ip=%s: want allow=%v, got status=%d", ip, wantOK, w.Code)
		}
	}
}

func TestIPWhitelist_TrustedXForwardedFor(t *testing.T) {
	// With TrustForwardedFor=true, the LEFTMOST XFF address wins.
	repo := newConfigRepo(t, `["10.0.0.0/8"]`)
	r := newTestRouter(repo, true)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/webhooks/xendit/qris", nil)
	req.RemoteAddr = "172.16.0.1:1234"             // intermediate proxy
	req.Header.Set("X-Forwarded-For", "10.5.5.5, 172.16.0.1") // real client
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
}

func TestIPWhitelist_UntrustedXForwardedForIgnored(t *testing.T) {
	// With TrustForwardedFor=false, the XFF must be ignored (spoofable).
	repo := newConfigRepo(t, `["10.0.0.0/8"]`)
	r := newTestRouter(repo, false)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/webhooks/xendit/qris", nil)
	req.RemoteAddr = "8.8.8.8:1234"
	req.Header.Set("X-Forwarded-For", "10.5.5.5") // spoofed; ignored
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status: %d (XFF should NOT bypass when untrusted)", w.Code)
	}
}

func TestIPWhitelist_PermissiveDefault(t *testing.T) {
	// Seeded default is "0.0.0.0/0" - check that lets through arbitrary IPv4.
	repo := newConfigRepo(t, `["0.0.0.0/0"]`)
	r := newTestRouter(repo, false)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/webhooks/xendit/qris", nil)
	req.RemoteAddr = "8.8.8.8:1234"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
}
