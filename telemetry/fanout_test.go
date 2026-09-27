package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type failingHandler struct{ err error }

func (h failingHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h failingHandler) Handle(context.Context, slog.Record) error { return h.err }
func (h failingHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h failingHandler) WithGroup(string) slog.Handler             { return h }

func TestFanoutHandler(t *testing.T) {
	t.Run("every enabled handler receives the record", func(t *testing.T) {
		var a, b bytes.Buffer
		h := NewFanoutHandler(
			slog.NewTextHandler(&a, &slog.HandlerOptions{Level: slog.LevelDebug}),
			slog.NewJSONHandler(&b, &slog.HandlerOptions{Level: slog.LevelWarn}),
		)
		logger := slog.New(h).With(slog.String("svc", "x")).WithGroup("req")

		logger.Debug("only local", slog.Int("n", 1))
		logger.Warn("both", slog.Int("n", 2))

		assert.Contains(t, a.String(), "only local")
		assert.Contains(t, a.String(), "both")
		assert.Contains(t, a.String(), "svc=x")
		assert.Contains(t, a.String(), "req.n=2", "groups propagate to wrapped handlers")

		assert.NotContains(t, b.String(), "only local", "level filtering is per handler")
		assert.Contains(t, b.String(), `"both"`)
		assert.Contains(t, b.String(), `"svc":"x"`)
	})

	t.Run("enabled when any handler is enabled", func(t *testing.T) {
		h := NewFanoutHandler(
			slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError}),
			slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelInfo}),
		)
		assert.True(t, h.Enabled(context.Background(), slog.LevelInfo))
		assert.False(t, h.Enabled(context.Background(), slog.LevelDebug))
	})

	t.Run("one failing destination does not block the others", func(t *testing.T) {
		var out bytes.Buffer
		boom := errors.New("collector down")
		h := NewFanoutHandler(failingHandler{err: boom}, slog.NewTextHandler(&out, nil), nil)

		var rec slog.Record
		rec = slog.NewRecord(rec.Time, slog.LevelInfo, "still logged", 0)
		err := h.Handle(context.Background(), rec)

		require.ErrorIs(t, err, boom)
		assert.True(t, strings.Contains(out.String(), "still logged"))
	})
}

func TestDefaultLogHandler(t *testing.T) {
	lp := sdklog.NewLoggerProvider() // no processors: records are dropped
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })

	t.Run("fans out to stderr by default", func(t *testing.T) {
		h := defaultLogHandler(Config{ServiceName: "svc"}, lp)
		_, isFanout := h.(*fanoutHandler)
		assert.True(t, isFanout)
	})

	t.Run("uses the configured local handler", func(t *testing.T) {
		var buf bytes.Buffer
		local := slog.NewJSONHandler(&buf, nil)
		h := defaultLogHandler(Config{ServiceName: "svc", LocalLogHandler: local}, lp)
		fan, ok := h.(*fanoutHandler)
		require.True(t, ok)
		assert.Same(t, local, fan.handlers[0])

		slog.New(h).Info("hello", slog.String("k", "v"))
		assert.Contains(t, buf.String(), `"msg":"hello"`)
	})

	t.Run("collector only when local logs are disabled", func(t *testing.T) {
		h := defaultLogHandler(Config{ServiceName: "svc", DisableLocalLogs: true}, lp)
		_, isFanout := h.(*fanoutHandler)
		assert.False(t, isFanout)
	})

	t.Run("local log level", func(t *testing.T) {
		ctx := context.Background()
		assert.False(t, localLogHandler(Config{}).Enabled(ctx, slog.LevelDebug), "Info by default")
		assert.True(t, localLogHandler(Config{}).Enabled(ctx, slog.LevelInfo))
		assert.True(t, localLogHandler(Config{LocalLogLevel: slog.LevelDebug}).Enabled(ctx, slog.LevelDebug))
		assert.False(t, localLogHandler(Config{LocalLogLevel: slog.LevelError}).Enabled(ctx, slog.LevelWarn))
	})
}
