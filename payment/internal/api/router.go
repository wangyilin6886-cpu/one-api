package api

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/payment/internal/api/handler"
	"github.com/songquanpeng/one-api/payment/internal/api/middleware"
	"github.com/songquanpeng/one-api/payment/internal/config"
	"github.com/songquanpeng/one-api/payment/internal/repository"
	"github.com/songquanpeng/one-api/payment/internal/service"
)

// Deps groups the singletons the router needs.
type Deps struct {
	Cfg            *config.Config
	DB             *gorm.DB
	OrderService   *service.OrderService
	WebhookService *service.WebhookService
	ConfigRepo     *repository.PaymentConfigRepo
}

// NewRouter builds the HTTP router. Mount points:
//
//	GET  /healthz                  health probe (no auth)
//	POST /orders                   user auth via one-api
//	GET  /orders/:order_no         user auth via one-api
//	POST /webhooks/polar           provider signature verified inside service
//
// Auth notes:
//   - /orders uses OneAPIUserAuth which forwards the Authorization header to
//     one-api's /api/user/self.
//   - /webhooks/polar has NO middleware-level auth; verification happens
//     inside WebhookService.Process via the provider adapter, because the
//     signature scheme depends on which adapter is wired in.
func NewRouter(d Deps) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.RequestLogger())

	r.GET("/healthz", handler.Health(d.DB))

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

	webhookHandler := handler.NewWebhookHandler(d.WebhookService)
	r.POST("/webhooks/polar", webhookHandler.Handle)

	return r
}
