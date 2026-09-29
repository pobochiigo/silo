package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	kitendpoint "github.com/go-kit/kit/endpoint"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/pobochiigo/silo/endpoint"
)

// mockSpanExporter collects finished spans.
type mockSpanExporter struct {
	spans []sdktrace.ReadOnlySpan
}

func (m *mockSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	m.spans = append(m.spans, spans...)
	return nil
}

func (m *mockSpanExporter) Shutdown(context.Context) error { return nil }

type loginReq struct{ User string }

type loginResp struct{ Token string }

var errBadUser = errors.New("bad user")

func login(_ context.Context, r loginReq) (loginResp, error) {
	if r.User == "" {
		return loginResp{}, errBadUser
	}
	return loginResp{Token: "t-" + r.User}, nil
}

// collect reads every metric the reader has by name.
func collect(t *testing.T, reader *metric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func TestMetrics(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	// The subsystem is the caller's: nothing in the names says go-kit.
	recorder := NewMetricsRecorder(mp.Meter("auth"), "auth_endpoint")
	ep := Metrics[loginReq, loginResp](recorder, "login")(login)

	resp, err := ep(context.Background(), loginReq{User: "ada"})
	require.NoError(t, err)
	assert.Equal(t, loginResp{Token: "t-ada"}, resp, "the typed response passes through untouched")
	_, err = ep(context.Background(), loginReq{})
	assert.ErrorIs(t, err, errBadUser)

	metrics := collect(t, reader)
	require.Contains(t, metrics, "auth_endpoint_requests_total")
	require.Contains(t, metrics, "auth_endpoint_errors_total")
	require.Contains(t, metrics, "auth_endpoint_request_duration_seconds")
	assert.NotContains(t, metrics, "gokit_requests_total")

	requests := metrics["auth_endpoint_requests_total"].Data.(metricdata.Sum[int64])
	require.Len(t, requests.DataPoints, 1, "one series per method, success is the errors counter's job")
	assert.Equal(t, int64(2), requests.DataPoints[0].Value)
	method, ok := requests.DataPoints[0].Attributes.Value("method")
	require.True(t, ok)
	assert.Equal(t, "login", method.AsString())

	errs := metrics["auth_endpoint_errors_total"].Data.(metricdata.Sum[int64])
	assert.Equal(t, int64(1), errs.DataPoints[0].Value)

	latency := metrics["auth_endpoint_request_duration_seconds"]
	assert.Equal(t, "s", latency.Unit)
	hist := latency.Data.(metricdata.Histogram[float64])
	assert.Equal(t, DefaultLatencyBuckets, hist.DataPoints[0].Bounds)
	assert.Equal(t, uint64(2), hist.DataPoints[0].Count)
}

func TestLogging(t *testing.T) {
	record := func(t *testing.T, buf *bytes.Buffer) map[string]any {
		t.Helper()
		var rec map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
		return rec
	}

	t.Run("succeeded", func(t *testing.T) {
		var buf bytes.Buffer
		ep := Logging[loginReq, loginResp]("login", slog.New(slog.NewJSONHandler(&buf, nil)))(login)
		resp, err := ep(context.Background(), loginReq{User: "ada"})
		require.NoError(t, err)
		assert.Equal(t, loginResp{Token: "t-ada"}, resp)

		rec := record(t, &buf)
		assert.Equal(t, "endpoint execution succeeded", rec["msg"])
		assert.Equal(t, "login", rec["operation"])
		assert.Contains(t, rec, "duration")
		assert.NotContains(t, rec, "error")
	})

	t.Run("failed", func(t *testing.T) {
		var buf bytes.Buffer
		ep := Logging[loginReq, loginResp]("login", slog.New(slog.NewJSONHandler(&buf, nil)))(login)
		_, err := ep(context.Background(), loginReq{})
		assert.ErrorIs(t, err, errBadUser)

		rec := record(t, &buf)
		assert.Equal(t, "endpoint execution failed", rec["msg"])
		assert.Equal(t, "bad user", rec["error"], "same attribute key as the slog adapter and the generated middlewares")
		assert.NotContains(t, rec, "err")
	})

	t.Run("nil logger resolves slog.Default lazily", func(t *testing.T) {
		// Built before the default logger is swapped, as code that runs
		// before InitTelemetry does.
		ep := Logging[loginReq, loginResp]("login", nil)(login)

		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })

		_, err := ep(context.Background(), loginReq{User: "ada"})
		require.NoError(t, err)
		assert.Equal(t, "endpoint execution succeeded", record(t, &buf)["msg"])
	})
}

func TestTracing(t *testing.T) {
	exporter := &mockSpanExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(oldTP)

	ep := Tracing[loginReq, loginResp]("login")(login)

	t.Run("succeeded span", func(t *testing.T) {
		exporter.spans = nil
		_, err := ep(context.Background(), loginReq{User: "ada"})
		require.NoError(t, err)
		require.Len(t, exporter.spans, 1)
		span := exporter.spans[0]
		assert.Equal(t, "login", span.Name())
		assert.Equal(t, instrumentationName, span.InstrumentationScope().Name)
		assert.Equal(t, sdktrace.Status{Code: codes.Unset}, span.Status(), "instrumentation leaves successful spans unset")
	})

	t.Run("failed span", func(t *testing.T) {
		exporter.spans = nil
		_, err := ep(context.Background(), loginReq{})
		assert.ErrorIs(t, err, errBadUser)
		require.Len(t, exporter.spans, 1)
		span := exporter.spans[0]
		assert.Equal(t, sdktrace.Status{Code: codes.Error, Description: "bad user"}, span.Status())
		require.Len(t, span.Events(), 1)
		assert.Equal(t, "exception", span.Events()[0].Name)
	})
}

func TestKit(t *testing.T) {
	exporter := &mockSpanExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	oldTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(oldTP)

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	oldMP := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	defer otel.SetMeterProvider(oldMP)

	var kitEP kitendpoint.Endpoint = func(_ context.Context, request any) (any, error) {
		if request == "boom" {
			return nil, errors.New("boom")
		}
		return "ok:" + request.(string), nil
	}

	// The [any, any] instantiation is a go-kit middleware after Kit, and
	// chains with go-kit's own Chain and with the deprecated wrappers.
	recorder := NewMetricsRecorder(mp.Meter("kit"), "kit_endpoint")
	ep := kitendpoint.Chain(
		Kit(Tracing[any, any]("op")),
		Kit(Metrics[any, any](recorder, "op")),
		LoggingMiddleware("op", slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))),
	)(kitEP)

	resp, err := ep(context.Background(), "x")
	require.NoError(t, err)
	assert.Equal(t, "ok:x", resp)
	_, err = ep(context.Background(), "boom")
	assert.EqualError(t, err, "boom")

	require.Len(t, exporter.spans, 2)
	assert.Equal(t, "op", exporter.spans[0].Name())
	metrics := collect(t, reader)
	assert.Equal(t, int64(2), metrics["kit_endpoint_requests_total"].Data.(metricdata.Sum[int64]).DataPoints[0].Value)
	assert.Equal(t, int64(1), metrics["kit_endpoint_errors_total"].Data.(metricdata.Sum[int64]).DataPoints[0].Value)

	t.Run("deprecated MetricsMiddleware records under the endpoint subsystem", func(t *testing.T) {
		_, err := MetricsMiddleware("legacy")(kitEP)(context.Background(), "x")
		require.NoError(t, err)
		metrics := collect(t, reader)
		require.Contains(t, metrics, "endpoint_requests_total")
		assert.NotContains(t, metrics, "gokit_requests_total")
	})

	t.Run("Kit is a conversion: the typed endpoint is the go-kit endpoint", func(t *testing.T) {
		typed := endpoint.Endpoint[any, any](kitEP)
		resp, err := typed(context.Background(), "y")
		require.NoError(t, err)
		assert.Equal(t, "ok:y", resp)
	})
}
