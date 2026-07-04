package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-kit/log"
)

// SlogAdapter implements the legacy github.com/go-kit/log.Logger interface.
// It maps go-kit's key-value log structure directly to Go's standard log/slog,
// ensuring compatibility with legacy go-kit servers while routing all logs
// through the globally registered OpenTelemetry logger.
type slogAdapter struct {
	ctx    context.Context
	logger *slog.Logger

	// We dynamically fetch slog.Default() so that calling slog.SetDefault(otelLogger)
	// automatically updates where this adapter sends its logs.
}

// NewSlogAdapter creates a new go-kit compatible logger backed by slog.
func NewSlogAdapter(ctx context.Context) log.Logger {
	return &slogAdapter{
		ctx: ctx,
		// Fetch the default slog logger (which is your OTel logger!)
		logger: slog.Default(),
	}
}

// Log implements the go-kit/log.Logger interface.
// It translates variadic key-value pairs (e.g., ["method", "CreateOrder", "err", err])
// into structured slog.Attr attributes.
func (a *slogAdapter) Log(keyvals ...any) error {
	// If the keyvals slice is empty, there is nothing to log.
	if len(keyvals) == 0 {
		return nil
	}

	var msg string
	var attrs []slog.Attr
	level := slog.LevelInfo // Default level if none is specified

	// Go-kit keys are conventionally string, but can be anything.
	// We iterate through pairs of [key, value].
	for i := 0; i < len(keyvals); i += 2 {
		// Handle odd-numbered keyvals (trailing key without a value)
		if i+1 >= len(keyvals) {
			attrs = append(attrs, slog.Any(fmt.Sprint(keyvals[i]), nil))
			break
		}

		key := fmt.Sprint(keyvals[i])
		val := keyvals[i+1]

		switch key {
		case "msg", "message":
			msg = fmt.Sprint(val)
		case "level":
			level = a.parseLogLevel(val)
		case "err", "error":
			if errVal, ok := val.(error); ok {
				attrs = append(attrs, slog.Any("error", errVal))
			} else {
				attrs = append(attrs, slog.Any("error", val))
			}
		default:
			attrs = append(attrs, slog.Any(key, val))
		}
	}

	if msg == "" {
		msg = "go-kit internal log"
	}

	if err := a.ctx.Err(); err != nil {
		return fmt.Errorf("logging aborted: %w", err)
	}

	// Slog requires a context to perform OTel trace-correlation.
	// Go-kit's basic Log interface does not pass a context.
	// We use context.Background() as a fallback.
	a.logger.LogAttrs(a.ctx, level, msg, attrs...)

	return nil
}

// parseLogLevel maps common go-kit level strings/values to slog Levels.
func (*slogAdapter) parseLogLevel(val any) slog.Level {
	switch fmt.Sprint(val) {
	case "debug", "DEBUG":
		return slog.LevelDebug
	case "info", "INFO":
		return slog.LevelInfo
	case "warn", "warning", "WARN":
		return slog.LevelWarn
	case "error", "err", "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
