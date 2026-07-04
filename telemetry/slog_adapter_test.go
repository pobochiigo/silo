package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type logRecord struct {
	Level   string `json:"level"`
	Msg     string `json:"msg"`
	Error   string `json:"error"`
	Method  string `json:"method"`
	Another string `json:"another"`
}

func TestSlogAdapter(t *testing.T) {
	t.Run("empty keys", func(t *testing.T) {
		adapter := NewSlogAdapter(context.Background())
		err := adapter.Log()
		assert.NoError(t, err)
	})

	t.Run("successful logging", func(t *testing.T) {
		var buf bytes.Buffer
		h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})
		logger := slog.New(h)

		// Create the adapter using a direct struct initialization or temporarily swap default slog
		adapter := &slogAdapter{
			ctx:    context.Background(),
			logger: logger,
		}

		err := adapter.Log(
			"level", "DEBUG",
			"msg", "test message",
			"method", "CreateOrder",
			"err", errors.New("something went wrong"),
			"another", "value",
		)
		assert.NoError(t, err)

		var rec logRecord
		err = json.Unmarshal(buf.Bytes(), &rec)
		require.NoError(t, err)

		assert.Equal(t, "DEBUG", rec.Level)
		assert.Equal(t, "test message", rec.Msg)
		assert.Equal(t, "CreateOrder", rec.Method)
		assert.Equal(t, "something went wrong", rec.Error)
		assert.Equal(t, "value", rec.Another)
	})

	t.Run("odd number of keyvals", func(t *testing.T) {
		var buf bytes.Buffer
		h := slog.NewJSONHandler(&buf, nil)
		logger := slog.New(h)

		adapter := &slogAdapter{
			ctx:    context.Background(),
			logger: logger,
		}

		err := adapter.Log(
			"level", "info",
			"msg", "odd params",
			"onlyKey",
		)
		assert.NoError(t, err)

		var rawMap map[string]any
		err = json.Unmarshal(buf.Bytes(), &rawMap)
		require.NoError(t, err)

		assert.Equal(t, "odd params", rawMap["msg"])
		assert.Contains(t, rawMap, "onlyKey")
		assert.Nil(t, rawMap["onlyKey"])
	})

	t.Run("level parsing", func(t *testing.T) {
		var buf bytes.Buffer
		h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
		logger := slog.New(h)

		adapter := &slogAdapter{
			ctx:    context.Background(),
			logger: logger,
		}

		testCases := []struct {
			inputLevel any
			expected   string
		}{
			{"debug", "DEBUG"},
			{"DEBUG", "DEBUG"},
			{"info", "INFO"},
			{"WARN", "WARN"},
			{"warning", "WARN"},
			{"error", "ERROR"},
			{"err", "ERROR"},
			{"unknown", "INFO"},
		}

		for _, tc := range testCases {
			buf.Reset()
			err := adapter.Log("level", tc.inputLevel, "msg", "test")
			assert.NoError(t, err)

			var raw map[string]any
			err = json.Unmarshal(buf.Bytes(), &raw)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, raw["level"])
		}
	})

	t.Run("context cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		adapter := NewSlogAdapter(ctx)
		err := adapter.Log("msg", "won't log")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "logging aborted")
	})

	t.Run("slog fallback and error value as string", func(t *testing.T) {
		var buf bytes.Buffer
		h := slog.NewJSONHandler(&buf, nil)
		logger := slog.New(h)

		adapter := &slogAdapter{
			ctx:    context.Background(),
			logger: logger,
		}

		// Log "err" with a non-error type (e.g. string)
		err := adapter.Log("msg", "test", "err", "some string error")
		assert.NoError(t, err)

		var raw map[string]any
		err = json.Unmarshal(buf.Bytes(), &raw)
		require.NoError(t, err)
		assert.Equal(t, "some string error", raw["error"])
	})
}
