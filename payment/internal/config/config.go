// Package config holds bootstrap configuration sourced from environment
// variables. Runtime-tunable configuration (exchange rate, webhook tokens,
// IP whitelist, etc.) lives in the payment_config table - see
// internal/repository.PaymentConfigRepo for that.
//
// All env vars are read once in Load(). After that, the *Config struct is
// passed around explicitly; we do NOT use global state for runtime config.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the immutable bootstrap config. Runtime config (read from DB)
// is fetched via the PaymentConfigRepo and cached in memory.
type Config struct {
	Port             int
	LogLevel         string
	LogFormat        string

	// Database (MySQL). DSN should already include the schema.
	DatabaseURL      string
	DatabaseLogLevel string

	// Outgoing call to one-api's internal topup endpoint.
	OneAPIBaseURL          string        // e.g. http://one-api:3000
	OneAPIInternalSecret   string        // shared with one-api's INTERNAL_API_SECRET (PR #1)
	OneAPITimeout          time.Duration // per attempt

	// Outgoing call to one-api for user validation (Authorization passthrough).
	OneAPIUserAuthPath string // default /api/user/self

	// Xendit
	XenditSecretKey string        // basic-auth username; password is empty per Xendit docs
	XenditBaseURL   string        // default https://api.xendit.co
	XenditTimeout   time.Duration // per attempt
	XenditRetries   int           // including the first attempt

	// Public base URL of the payment-service, used to build webhook URLs
	// and (optionally) hosted checkout return URLs.
	PaymentBaseURL string

	// Runtime config cache TTL.
	RuntimeConfigCacheTTL time.Duration

	// Cron intervals.
	ExpireSweepInterval time.Duration
	TopupRetryInterval  time.Duration
	TopupRetryMaxAge    time.Duration // give up retrying after this
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
		XenditSecretKey:       os.Getenv("XENDIT_SECRET_KEY"),
		XenditBaseURL:         envStrDefault("XENDIT_BASE_URL", "https://api.xendit.co"),
		XenditTimeout:         envDurDefault("XENDIT_TIMEOUT", 10*time.Second),
		XenditRetries:         envIntDefault("XENDIT_RETRIES", 3),
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
	if c.XenditSecretKey == "" {
		return nil, errors.New("XENDIT_SECRET_KEY is required")
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
		"Config{Port=%d OneAPI=%s Xendit=%s PaymentBase=%s ExpireSweep=%s "+
			"OneAPISecret=%s* XenditKey=%s*}",
		c.Port, c.OneAPIBaseURL, c.XenditBaseURL, c.PaymentBaseURL,
		c.ExpireSweepInterval,
		redact(c.OneAPIInternalSecret), redact(c.XenditSecretKey),
	)
}

func redact(s string) string {
	if len(s) < 6 {
		return "***"
	}
	return s[:4] + "..."
}
