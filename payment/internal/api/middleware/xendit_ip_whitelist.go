// Package middleware contains Gin middlewares for the payment-service HTTP
// layer: Xendit webhook auth (IP whitelist + shared token), one-api-backed
// user auth, and request logging.
package middleware

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/songquanpeng/one-api/payment/internal/errors"
	"github.com/songquanpeng/one-api/payment/internal/logger"
	"github.com/songquanpeng/one-api/payment/internal/model"
	"github.com/songquanpeng/one-api/payment/internal/repository"
)

// XenditIPWhitelistMiddleware enforces the runtime-configurable IP whitelist
// from payment_config.xendit_ip_whitelist_json (correction 7).
//
// Cache TTL is 5 minutes; admins editing the whitelist see at most a 5-min lag.
//
// Real-IP resolution:
//   - Trust X-Forwarded-For ONLY when TrustForwardedFor is true (i.e. we are
//     behind a reverse proxy we control).
//   - Take the LEFTMOST entry of XFF as the originating client.
//
// On empty / missing config, the default seeds ship "0.0.0.0/0" so the
// service still works in dev. Operators MUST lock this down before going
// live - the seed Description string says so.
type XenditIPWhitelistOptions struct {
	ConfigRepo        *repository.PaymentConfigRepo
	CacheTTL          time.Duration
	TrustForwardedFor bool
}

func XenditIPWhitelist(opts XenditIPWhitelistOptions) gin.HandlerFunc {
	if opts.CacheTTL == 0 {
		opts.CacheTTL = 5 * time.Minute
	}
	cache := &ipWhitelistCache{configRepo: opts.ConfigRepo, ttl: opts.CacheTTL}
	return func(c *gin.Context) {
		clientIP := resolveClientIP(c, opts.TrustForwardedFor)
		ipObj := net.ParseIP(clientIP)
		if ipObj == nil {
			respondError(c, errors.IPNotAllowed, "cannot parse source IP: "+clientIP)
			return
		}

		nets, err := cache.get(c.Request.Context())
		if err != nil {
			respondError(c, errors.ServiceUnavailable, "ip whitelist unavailable: "+err.Error())
			return
		}
		for _, n := range nets {
			if n.Contains(ipObj) {
				c.Set("payment_source_ip", clientIP)
				c.Next()
				return
			}
		}
		logger.L(c.Request.Context()).Warn("webhook source IP rejected",
			zap.String("source_ip", clientIP),
			zap.String("path", c.Request.URL.Path))
		respondError(c, errors.IPNotAllowed, "source IP "+clientIP+" not in whitelist")
	}
}

// resolveClientIP picks the real client IP, honoring X-Forwarded-For ONLY
// when the operator marked the proxy as trusted. When untrusted, we
// MUST bypass gin's c.ClientIP() (which trusts XFF by default) and use
// c.RemoteIP() — the actual TCP peer.
func resolveClientIP(c *gin.Context, trustForwarded bool) string {
	if trustForwarded {
		if xff := c.Request.Header.Get("X-Forwarded-For"); xff != "" {
			// Take the leftmost = originating client.
			parts := strings.Split(xff, ",")
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
		if xri := c.Request.Header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri)
		}
		// Fall through to gin's resolution if no forwarded header is present.
		return c.ClientIP()
	}
	// Untrusted mode: use the TCP peer, never the forwarded headers.
	return c.RemoteIP()
}

// ipWhitelistCache loads + caches the parsed CIDR list from payment_config.
type ipWhitelistCache struct {
	configRepo *repository.PaymentConfigRepo
	ttl        time.Duration

	mu       sync.RWMutex
	cached   []*net.IPNet
	loadedAt time.Time
}

func (c *ipWhitelistCache) get(ctx context.Context) ([]*net.IPNet, error) {
	c.mu.RLock()
	fresh := time.Since(c.loadedAt) < c.ttl && len(c.cached) > 0
	cached := c.cached
	c.mu.RUnlock()
	if fresh {
		return cached, nil
	}
	return c.reload(ctx)
}

func (c *ipWhitelistCache) reload(ctx context.Context) ([]*net.IPNet, error) {
	raw, err := c.configRepo.Get(ctx, model.CfgXenditIPWhitelistJSON)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		// Operator hasn't set it. Fail closed.
		c.mu.Lock()
		c.cached = nil
		c.loadedAt = time.Now()
		c.mu.Unlock()
		return nil, nil
	}
	var cidrs []string
	if err := json.Unmarshal([]byte(raw), &cidrs); err != nil {
		return nil, err
	}
	parsed := make([]*net.IPNet, 0, len(cidrs))
	for _, s := range cidrs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// Accept bare IPs by promoting to /32 or /128.
		if !strings.Contains(s, "/") {
			if strings.Contains(s, ":") {
				s = s + "/128"
			} else {
				s = s + "/32"
			}
		}
		_, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			logger.Sys().Warn("invalid CIDR in xendit_ip_whitelist_json",
				zap.String("cidr", s), zap.Error(err))
			continue
		}
		parsed = append(parsed, ipnet)
	}
	c.mu.Lock()
	c.cached = parsed
	c.loadedAt = time.Now()
	c.mu.Unlock()
	return parsed, nil
}

func respondError(c *gin.Context, code errors.Code, detail string) {
	lang := errors.PickLang(c.Request.Header.Get("Accept-Language"))
	c.AbortWithStatusJSON(code.HTTPStatus(), errors.Build(code, lang, detail))
}
