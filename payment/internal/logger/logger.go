// Package logger wraps zap with a small surface so the rest of the code never
// imports zap directly. Two loggers are kept globally: a "sys" logger for
// startup / shutdown / cron events, and a per-request logger that carries
// request-id / order-no / user-id fields when available.
package logger

import (
	"context"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var base *zap.Logger

type ctxKey int

const fieldsKey ctxKey = 0

// Init creates the global logger. Call once during startup.
//
// `level` accepts "debug" / "info" / "warn" / "error" (case-insensitive).
// `format` accepts "json" (recommended for prod) or "console" (dev).
func Init(level, format string) {
	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = "ts"
	cfg.LevelKey = "level"
	cfg.MessageKey = "msg"
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder

	var enc zapcore.Encoder
	if format == "console" {
		cfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		enc = zapcore.NewConsoleEncoder(cfg)
	} else {
		enc = zapcore.NewJSONEncoder(cfg)
	}

	lvl := zapcore.InfoLevel
	_ = lvl.UnmarshalText([]byte(level))

	core := zapcore.NewCore(enc, zapcore.AddSync(os.Stdout), lvl)
	base = zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1))
}

// WithFields returns a new context carrying additional zap fields. Subsequent
// L(ctx) calls include these fields. Multiple WithFields stack additively.
func WithFields(ctx context.Context, fields ...zap.Field) context.Context {
	prev, _ := ctx.Value(fieldsKey).([]zap.Field)
	combined := make([]zap.Field, 0, len(prev)+len(fields))
	combined = append(combined, prev...)
	combined = append(combined, fields...)
	return context.WithValue(ctx, fieldsKey, combined)
}

// L returns the logger augmented with any fields stashed on ctx.
func L(ctx context.Context) *zap.Logger {
	if base == nil {
		// Safe fallback if Init() wasn't called (tests, scripts).
		Init("info", "console")
	}
	if ctx == nil {
		return base
	}
	if fields, ok := ctx.Value(fieldsKey).([]zap.Field); ok && len(fields) > 0 {
		return base.With(fields...)
	}
	return base
}

// Sys returns the global logger without any context-bound fields.
func Sys() *zap.Logger {
	if base == nil {
		Init("info", "console")
	}
	return base
}
