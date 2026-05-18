package api

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/api/handler"
	"github.com/songquanpeng/one-api/payment/internal/api/middleware"
	"github.com/songquanpeng/one-api/payment/internal/config"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/repository"
	"github.com/songquanpeng/one-api/payment/internal/service"
)

// Deps groups the singletons the router needs. Keeping them in one struct
// makes main.go's wiring explicit and the router easy to test.
type Deps struct {
	Cfg              *config.Config
	DB               *gorm.DB
	OrderService     *service.OrderService
	WebhookService   *service.WebhookService
	ConfigRepo       *repository.PaymentConfigRepo
}

// NewRouter builds the full HTTP router. Mount points:
//
//	GET  /healthz                            health probe (no auth)
//	POST /orders                             user auth via one-api
//	GET  /orders/:order_no                   user auth via one-api
//	POST /webhooks/xendit/qris               IP whitelist + token (QRIS)
//	POST /webhooks/xendit/va                 IP whitelist + token (VA)
//	POST /webhooks/xendit/invoice            IP whitelist + token (Invoice)
func NewRouter(d Deps) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.RequestLogger())

	// Public health
	r.GET("/healthz", handler.Health(d.DB))

	// User-facing
	userAuth := middleware.NewOneAPIUserAuth(
		d.Cfg.OneAPIBaseURL, d.Cfg.OneAPIUserAuthPath, d.Cfg.OneAPITimeout,
	).Handler()
	orderHandler := handler.NewOrderHandler(d.OrderService)
	user := r.Group("/")
	user.Use(userAuth)
	{
		user.POST("/orders", orderHandler.Create)
		user.GET("/orders/:order_no", orderHandler.Get)
	}

	// Webhook ingress
	ipMW := middleware.XenditIPWhitelist(middleware.XenditIPWhitelistOptions{
		ConfigRepo:        d.ConfigRepo,
		CacheTTL:          d.Cfg.RuntimeConfigCacheTTL,
		TrustForwardedFor: true, // payment-service typically runs behind nginx/Cloudflare
	})

	webhookHandler := handler.NewWebhookHandler(d.WebhookService)

	r.POST("/webhooks/xendit/qris",
		ipMW,
		middleware.XenditTokenAuth(d.ConfigRepo, model.CfgXenditWebhookTokenQRIS),
		webhookHandler.QRIS)

	r.POST("/webhooks/xendit/va",
		ipMW,
		middleware.XenditTokenAuth(d.ConfigRepo, model.CfgXenditWebhookTokenVA),
		webhookHandler.VA)

	r.POST("/webhooks/xendit/invoice",
		ipMW,
		middleware.XenditTokenAuth(d.ConfigRepo, model.CfgXenditWebhookTokenInv),
		webhookHandler.Invoice)

	return r
}
