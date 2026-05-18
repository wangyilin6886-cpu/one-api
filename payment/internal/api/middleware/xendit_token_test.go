package middleware

import (
	"context"
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

func setupTokenRepo(t *testing.T, key, value string) *repository.PaymentConfigRepo {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	if err := db.AutoMigrate(&model.PaymentConfig{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := repository.NewPaymentConfigRepo(db, 10*time.Millisecond)
	if value != "" {
		if err := repo.Set(context.Background(), key, value, 0); err != nil {
			t.Fatalf("set: %v", err)
		}
	}
	return repo
}

func newTokenTestRouter(repo *repository.PaymentConfigRepo, configKey string) *gin.Engine {
	r := gin.New()
	r.POST("/webhook", XenditTokenAuth(repo, configKey), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})
	return r
}

func TestTokenAuth_HappyPath(t *testing.T) {
	repo := setupTokenRepo(t, model.CfgXenditWebhookTokenQRIS, "secret-qris-token-xxxx")
	r := newTokenTestRouter(repo, model.CfgXenditWebhookTokenQRIS)
	req := httptest.NewRequest("POST", "/webhook", nil)
	req.Header.Set("X-Callback-Token", "secret-qris-token-xxxx")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
}

func TestTokenAuth_Mismatch(t *testing.T) {
	repo := setupTokenRepo(t, model.CfgXenditWebhookTokenQRIS, "secret-qris-token-xxxx")
	r := newTokenTestRouter(repo, model.CfgXenditWebhookTokenQRIS)
	req := httptest.NewRequest("POST", "/webhook", nil)
	req.Header.Set("X-Callback-Token", "wrong-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestTokenAuth_Missing(t *testing.T) {
	repo := setupTokenRepo(t, model.CfgXenditWebhookTokenQRIS, "secret-qris-token-xxxx")
	r := newTokenTestRouter(repo, model.CfgXenditWebhookTokenQRIS)
	req := httptest.NewRequest("POST", "/webhook", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestTokenAuth_NotConfigured(t *testing.T) {
	// Empty value in payment_config -> fail closed.
	repo := setupTokenRepo(t, model.CfgXenditWebhookTokenQRIS, "")
	r := newTokenTestRouter(repo, model.CfgXenditWebhookTokenQRIS)
	req := httptest.NewRequest("POST", "/webhook", nil)
	req.Header.Set("X-Callback-Token", "whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
}
