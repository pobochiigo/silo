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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Mock trace exporter to collect trace spans
type mockSpanExporter struct {
	spans []sdktrace.ReadOnlySpan
}

func (m *mockSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	m.spans = append(m.spans, spans...)
	return nil
}

func (m *mockSpanExporter) Shutdown(ctx context.Context) error {
	return nil
}

func TestMetricsMiddleware(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	defer func() {
		_ = mp.Shutdown(context.Background())
	}()

	// Temporarily override the global meter provider for testing
	oldMP := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	defer otel.SetMeterProvider(oldMP)

	mw := MetricsMiddleware("TestOp")

	nextCalled := false
	next := func(ctx context.Context, req any) (any, error) {
		nextCalled = true
		if req == "error" {
			return nil, errors.New("business error")
		}
		return "success_resp", nil
	}

	ep := mw(next)
	ctx := context.Background()

	// 1. Run success
	resp, err := ep(ctx, "hello")
	assert.NoError(t, err)
	assert.Equal(t, "success_resp", resp)
	assert.True(t, nextCalled)

	// 2. Run error
	nextCalled = false
	resp, err = ep(ctx, "error")
	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.True(t, nextCalled)

	// Collect metrics
	var rm metricdata.ResourceMetrics
	err = reader.Collect(ctx, &rm)
	assert.NoError(t, err)

	metricsMap := make(map[string]metricdata.Metrics)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metricsMap[m.Name] = m
		}
	}

	assert.Contains(t, metricsMap, "gokit_requests_total")
	assert.Contains(t, metricsMap, "gokit_request_duration_seconds")

	reqMetric := metricsMap["gokit_requests_total"]
	sumData, ok := reqMetric.Data.(metricdata.Sum[int64])
	assert.True(t, ok)

	// We expect 2 datapoints (one for success=true, one for success=false)
	assert.Len(t, sumData.DataPoints, 2)

	var successTrueVal, successFalseVal int64
	for _, dp := range sumData.DataPoints {
		for _, attr := range dp.Attributes.ToSlice() {
			if attr.Key == "success" {
				if attr.Value.AsString() == "true" {
					successTrueVal = dp.Value
				} else if attr.Value.AsString() == "false" {
					successFalseVal = dp.Value
				}
			}
		}
	}
	assert.Equal(t, int64(1), successTrueVal)
	assert.Equal(t, int64(1), successFalseVal)
}

func TestLoggingMiddleware(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) {
		var buf bytes.Buffer
		h := slog.NewJSONHandler(&buf, nil)
		logger := slog.New(h)

		mw := LoggingMiddleware("TestOp", logger)
		next := func(ctx context.Context, req any) (any, error) {
			return "resp", nil
		}

		ep := mw(next)
		_, err := ep(context.Background(), "req")
		assert.NoError(t, err)

		var rec map[string]any
		err = json.Unmarshal(buf.Bytes(), &rec)
		require.NoError(t, err)

		assert.Equal(t, "endpoint execution succeeded", rec["msg"])
		assert.Equal(t, "TestOp", rec["operation"])
		assert.Contains(t, rec, "duration")
	})

	t.Run("failed", func(t *testing.T) {
		var buf bytes.Buffer
		h := slog.NewJSONHandler(&buf, nil)
		logger := slog.New(h)

		mw := LoggingMiddleware("TestOp", logger)
		next := func(ctx context.Context, req any) (any, error) {
			return nil, errors.New("some error")
		}

		ep := mw(next)
		_, err := ep(context.Background(), "req")
		assert.Error(t, err)

		var rec map[string]any
		err = json.Unmarshal(buf.Bytes(), &rec)
		require.NoError(t, err)

		assert.Equal(t, "endpoint execution failed", rec["msg"])
		assert.Equal(t, "TestOp", rec["operation"])
		assert.Equal(t, "some error", rec["err"])
	})

	t.Run("fallback logger", func(t *testing.T) {
		// Verify that providing nil logger uses slog.Default and does not panic
		mw := LoggingMiddleware("TestOp", nil)
		next := func(ctx context.Context, req any) (any, error) {
			return "resp", nil
		}
		ep := mw(next)
		_, err := ep(context.Background(), "req")
		assert.NoError(t, err)
	})
}

func TestTracingMiddleware(t *testing.T) {
	exporter := &mockSpanExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() {
		_ = tp.Shutdown(context.Background())
	}()

	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(oldTP)

	mw := TracingMiddleware("TestOp")

	t.Run("succeeded span", func(t *testing.T) {
		exporter.spans = nil
		next := func(ctx context.Context, req any) (any, error) {
			return "resp", nil
		}

		ep := mw(next)
		_, err := ep(context.Background(), "req")
		assert.NoError(t, err)

		// Force trace flushing
		tp.ForceFlush(context.Background())

		require.Len(t, exporter.spans, 1)
		span := exporter.spans[0]
		assert.Equal(t, "TestOp", span.Name())
		assert.Equal(t, sdktrace.Status{Code: codes.Ok, Description: ""}, span.Status())
	})

	t.Run("failed span", func(t *testing.T) {
		exporter.spans = nil
		expectedErr := errors.New("span failed")
		next := func(ctx context.Context, req any) (any, error) {
			return nil, expectedErr
		}

		ep := mw(next)
		_, err := ep(context.Background(), "req")
		assert.Equal(t, expectedErr, err)

		tp.ForceFlush(context.Background())

		require.Len(t, exporter.spans, 1)
		span := exporter.spans[0]
		assert.Equal(t, "TestOp", span.Name())
		assert.Equal(t, sdktrace.Status{Code: codes.Error, Description: "span failed"}, span.Status())
		assert.Len(t, span.Events(), 1)
		assert.Equal(t, "exception", span.Events()[0].Name)
	})
}
