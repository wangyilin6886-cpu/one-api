// payment-service entry point. Wires config -> DB -> provider -> services ->
// HTTP -> cron, and runs until SIGINT / SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/songquanpeng/one-api/payment/internal/api"
	"github.com/songquanpeng/one-api/payment/internal/config"
	"github.com/songquanpeng/one-api/payment/internal/cron"
	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/provider"
	"github.com/songquanpeng/one-api/payment/internal/repository"
	"github.com/songquanpeng/one-api/payment/internal/service"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config load failed:", err)
		os.Exit(2)
	}
	logger.Init(cfg.LogLevel, cfg.LogFormat)
	logger.Sys().Info("payment-service starting", zap.String("config", cfg.String()))

	db, err := openDB(cfg)
	if err != nil {
		logger.Sys().Fatal("DB open failed", zap.Error(err))
	}
	if err := autoMigrate(db); err != nil {
		logger.Sys().Fatal("DB migrate failed", zap.Error(err))
	}

	// Repositories
	orderRepo := repository.NewOrderRepo(db)
	webhookRepo := repository.NewWebhookRepo(db)
	topupCBRepo := repository.NewTopupCallbackRepo(db)
	configRepo := repository.NewPaymentConfigRepo(db, cfg.RuntimeConfigCacheTTL)
	if err := configRepo.SeedIfMissing(context.Background()); err != nil {
		logger.Sys().Fatal("payment_config seed failed", zap.Error(err))
	}

	// Provider. Polar runtime config (webhook secret, org id, product id)
	// lives in payment_config; we load it here so a misconfigured deploy
	// fails on startup, not on the first checkout.
	polarCfg, err := loadPolarConfig(context.Background(), cfg, configRepo)
	if err != nil {
		logger.Sys().Fatal("polar config load failed", zap.Error(err))
	}
	polarProvider, err := provider.NewPolar(polarCfg)
	if err != nil {
		logger.Sys().Fatal("polar provider init failed", zap.Error(err))
	}

	topupClient := service.NewTopupClient(cfg.OneAPIBaseURL, cfg.OneAPIInternalSecret, cfg.OneAPITimeout)

	alerter := service.NewLogAlerter()
	orderService := service.NewOrderService(db, orderRepo, configRepo, polarProvider, alerter)
	webhookService := service.NewWebhookService(db, orderRepo, webhookRepo, topupCBRepo, topupClient, polarProvider, alerter)

	router := api.NewRouter(api.Deps{
		Cfg: cfg, DB: db,
		OrderService:   orderService,
		WebhookService: webhookService,
		ConfigRepo:     configRepo,
	})

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           router,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go cron.NewExpireOrders(db, orderRepo, cfg.ExpireSweepInterval).Start(ctx)
	go cron.NewTopupRetry(orderRepo, webhookService, cfg.TopupRetryInterval, cfg.TopupRetryMaxAge).Start(ctx)

	serveErr := make(chan error, 1)
	go func() {
		logger.Sys().Info("listening", zap.Int("port", cfg.Port))
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		logger.Sys().Info("signal received, shutting down", zap.Stringer("signal", sig))
	case err := <-serveErr:
		logger.Sys().Error("HTTP server failed", zap.Error(err))
	}

	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Sys().Error("HTTP shutdown error", zap.Error(err))
	}
	logger.Sys().Info("payment-service stopped")
}

func loadPolarConfig(ctx context.Context, cfg *config.Config, cr *repository.PaymentConfigRepo) (provider.PolarConfig, error) {
	all, err := cr.GetAll(ctx)
	if err != nil {
		return provider.PolarConfig{}, err
	}
	return provider.PolarConfig{
		AccessToken:    cfg.PolarAccessToken,
		OrganizationId: all[model.CfgPolarOrgId],
		ProductTopupId: all[model.CfgPolarTopupProductId],
		WebhookSecret:  all[model.CfgPolarWebhookSecret],
		BaseURL:        cfg.PolarBaseURL,
		SandboxMode:    cfg.PolarSandbox,
	}, nil
}

func openDB(cfg *config.Config) (*gorm.DB, error) {
	gcfg := &gorm.Config{
		Logger: gormlogger.Default.LogMode(parseGormLogLevel(cfg.DatabaseLogLevel)),
	}
	if strings.HasPrefix(cfg.DatabaseURL, "sqlite://") {
		return gorm.Open(sqlite.Open(strings.TrimPrefix(cfg.DatabaseURL, "sqlite://")), gcfg)
	}
	return gorm.Open(mysql.Open(cfg.DatabaseURL), gcfg)
}

func parseGormLogLevel(s string) gormlogger.LogLevel {
	switch s {
	case "silent":
		return gormlogger.Silent
	case "error":
		return gormlogger.Error
	case "warn":
		return gormlogger.Warn
	case "info":
		return gormlogger.Info
	}
	return gormlogger.Warn
}

func autoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&model.Order{},
		&model.WebhookEvent{},
		&model.TopupCallback{},
		&model.Refund{},
		&model.PaymentConfig{},
	)
}
