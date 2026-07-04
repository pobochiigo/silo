package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type MetricsRecorder struct {
	meter            metric.Meter
	requestCounter   metric.Int64Counter
	errorCounter     metric.Int64Counter
	latencyHistogram metric.Float64Histogram
}

func NewMetricsRecorder(meter metric.Meter, subsystem string) *MetricsRecorder {
	requestCounter, err := meter.Int64Counter(subsystem+"_requests_total", metric.WithDescription("Total number of requests"))
	if err != nil {
		otel.Handle(err)
	}
	errorCounter, err := meter.Int64Counter(subsystem+"_errors_total", metric.WithDescription("Total number of failed requests"))
	if err != nil {
		otel.Handle(err)
	}
	latencyHistogram, err := meter.Float64Histogram(subsystem+"_request_duration_seconds", metric.WithDescription("Request latency in seconds"))
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

func (r *MetricsRecorder) Meter() metric.Meter {
	return r.meter
}

func (r *MetricsRecorder) Observe(ctx context.Context, method string, start time.Time, err error, attrs ...attribute.KeyValue) {
	allAttrs := append([]attribute.KeyValue{attribute.String("method", method)}, attrs...)
	opts := metric.WithAttributes(allAttrs...)

	r.requestCounter.Add(ctx, 1, opts)
	r.latencyHistogram.Record(ctx, time.Since(start).Seconds(), opts)
	if err != nil {
		r.errorCounter.Add(ctx, 1, opts)
	}
}
