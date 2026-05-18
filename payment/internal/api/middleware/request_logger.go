package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/songquanpeng/one-api/payment/internal/logger"
)

// RequestLogger emits one structured log line per HTTP request. Errors are
// logged at WARN/ERROR depending on status code.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)

		status := c.Writer.Status()
		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.String("query", c.Request.URL.RawQuery),
			zap.String("client_ip", c.ClientIP()),
			zap.Int("status", status),
			zap.Int("size", c.Writer.Size()),
			zap.Duration("latency", latency),
		}
		switch {
		case status >= 500:
			logger.L(c.Request.Context()).Error("http", fields...)
		case status >= 400:
			logger.L(c.Request.Context()).Warn("http", fields...)
		default:
			logger.L(c.Request.Context()).Info("http", fields...)
		}
	}
}
