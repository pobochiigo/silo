package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// DefaultLatencyBuckets are the histogram bucket boundaries, in seconds,
// used for request latency. They match the Prometheus client defaults, from
// 5ms to 10s, so percentiles are meaningful for typical service calls. The
// OpenTelemetry SDK's own defaults are sized for milliseconds and would put
// every sub-5-second request into a single bucket.
var DefaultLatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// MetricsRecorder records request counts, error counts and latency for one
// subsystem (typically one decorated interface).
type MetricsRecorder struct {
	meter            metric.Meter
	requestCounter   metric.Int64Counter
	errorCounter     metric.Int64Counter
	latencyHistogram metric.Float64Histogram
}

// NewMetricsRecorder creates the instruments <subsystem>_requests_total,
// <subsystem>_errors_total and <subsystem>_request_duration_seconds on meter.
func NewMetricsRecorder(meter metric.Meter, subsystem string) *MetricsRecorder {
	requestCounter, err := meter.Int64Counter(subsystem+"_requests_total",
		metric.WithDescription("Total number of requests"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		otel.Handle(err)
	}
	errorCounter, err := meter.Int64Counter(subsystem+"_errors_total",
		metric.WithDescription("Total number of failed requests"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		otel.Handle(err)
	}
	latencyHistogram, err := meter.Float64Histogram(subsystem+"_request_duration_seconds",
		metric.WithDescription("Request latency in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(DefaultLatencyBuckets...),
	)
	if err != nil {
		otel.Handle(err)
	}

	return &MetricsRecorder{
		meter:            meter,
		requestCounter:   requestCounter,
		errorCounter:     errorCounter,
		latencyHistogram: latencyHistogram,
	}
}

// Meter returns the meter the recorder's instruments were created on, so
// callers can add custom instruments next to them.
func (r *MetricsRecorder) Meter() metric.Meter {
	return r.meter
}

// Observe records one call of method that started at start: one request,
// its latency, and one error when err is non-nil.
func (r *MetricsRecorder) Observe(ctx context.Context, method string, start time.Time, err error, attrs ...attribute.KeyValue) {
	allAttrs := append([]attribute.KeyValue{attribute.String("method", method)}, attrs...)
	opts := metric.WithAttributes(allAttrs...)

	r.requestCounter.Add(ctx, 1, opts)
	r.latencyHistogram.Record(ctx, time.Since(start).Seconds(), opts)
	if err != nil {
		r.errorCounter.Add(ctx, 1, opts)
	}
}
