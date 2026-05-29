// Package config holds bootstrap configuration sourced from environment
// variables. Runtime-tunable configuration (Polar secrets, min/max amounts,
// etc.) lives in the payment_config table - see repository.PaymentConfigRepo.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the immutable bootstrap config.
type Config struct {
	Port      int
	LogLevel  string
	LogFormat string

	DatabaseURL      string
	DatabaseLogLevel string

	// Outgoing call to one-api's internal topup endpoint (PR #1).
	OneAPIBaseURL        string
	OneAPIInternalSecret string
	OneAPITimeout        time.Duration
	OneAPIUserAuthPath   string

	// Polar provider config. The webhook secret, org id, and product id
	// live in payment_config (runtime-editable). The access token is sensitive
	// enough that we keep it in env vars only.
	PolarAccessToken string
	PolarSandbox     bool
	PolarBaseURL     string // optional override

	// Public base URL of this service.
	PaymentBaseURL string

	// In-memory cache TTL for payment_config rows.
	RuntimeConfigCacheTTL time.Duration

	// Cron intervals.
	ExpireSweepInterval time.Duration
	TopupRetryInterval  time.Duration
	TopupRetryMaxAge    time.Duration
}

// Load reads env vars and validates required fields.
func Load() (*Config, error) {
	c := &Config{
		Port:                  envIntDefault("PORT", 3001),
		LogLevel:              envStrDefault("LOG_LEVEL", "info"),
		LogFormat:             envStrDefault("LOG_FORMAT", "json"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		DatabaseLogLevel:      envStrDefault("DATABASE_LOG_LEVEL", "warn"),
		OneAPIBaseURL:         strings.TrimRight(os.Getenv("ONE_API_INTERNAL_URL"), "/"),
		OneAPIInternalSecret:  os.Getenv("INTERNAL_API_SECRET"),
		OneAPITimeout:         envDurDefault("ONE_API_TIMEOUT", 5*time.Second),
		OneAPIUserAuthPath:    envStrDefault("ONE_API_USER_AUTH_PATH", "/api/user/self"),
		PolarAccessToken:      os.Getenv("POLAR_ACCESS_TOKEN"),
		PolarSandbox:          envBoolDefault("POLAR_SANDBOX", true),
		PolarBaseURL:          os.Getenv("POLAR_BASE_URL"),
		PaymentBaseURL:        strings.TrimRight(os.Getenv("PAYMENT_BASE_URL"), "/"),
		RuntimeConfigCacheTTL: envDurDefault("RUNTIME_CONFIG_CACHE_TTL", 5*time.Minute),
		ExpireSweepInterval:   envDurDefault("EXPIRE_SWEEP_INTERVAL", 1*time.Minute),
		TopupRetryInterval:    envDurDefault("TOPUP_RETRY_INTERVAL", 30*time.Second),
		TopupRetryMaxAge:      envDurDefault("TOPUP_RETRY_MAX_AGE", 24*time.Hour),
	}

	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if c.OneAPIBaseURL == "" {
		return nil, errors.New("ONE_API_INTERNAL_URL is required")
	}
	if len(c.OneAPIInternalSecret) < 32 {
		return nil, errors.New("INTERNAL_API_SECRET must be at least 32 chars (matches one-api PR #1)")
	}
	if c.PolarAccessToken == "" {
		return nil, errors.New("POLAR_ACCESS_TOKEN is required")
	}
	return c, nil
}

func envStrDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envIntDefault(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func envBoolDefault(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

func envDurDefault(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// String returns a redacted summary safe for logs.
func (c *Config) String() string {
	return fmt.Sprintf(
		"Config{Port=%d OneAPI=%s Polar=%s(sandbox=%v) PaymentBase=%s ExpireSweep=%s "+
			"OneAPISecret=%s* PolarToken=%s*}",
		c.Port, c.OneAPIBaseURL, c.polarEffectiveBase(), c.PolarSandbox, c.PaymentBaseURL,
		c.ExpireSweepInterval,
		redact(c.OneAPIInternalSecret), redact(c.PolarAccessToken),
	)
}

func (c *Config) polarEffectiveBase() string {
	if c.PolarBaseURL != "" {
		return c.PolarBaseURL
	}
	if c.PolarSandbox {
		return "https://sandbox-api.polar.sh"
	}
	return "https://api.polar.sh"
}

func redact(s string) string {
	if len(s) < 6 {
		return "***"
	}
	return s[:4] + "..."
}
