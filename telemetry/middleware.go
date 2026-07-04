package telemetry

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-kit/kit/endpoint"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// MetricsMiddleware returns a standard go-kit endpoint middleware that records
// request counts and execution latency using pure OpenTelemetry Metrics.
func MetricsMiddleware(operationName string) endpoint.Middleware {
	// Global instruments. OpenTelemetry handles proxying these automatically
	// before the concrete MeterProvider is initialized in main.go.
	var requestCounter metric.Int64Counter
	var latencyRecorder metric.Float64Histogram
	var err error

	// Obtain a meter from the global registry.
	meter := otel.Meter("gokit-endpoints")

	// 1. Initialize a cumulative Counter for total requests
	requestCounter, err = meter.Int64Counter(
		"gokit_requests_total",
		metric.WithDescription("Total number of requests processed by go-kit endpoints"),
		metric.WithUnit("1"),
	)
	if err != nil {
		otel.Handle(err)
	}

	// 2. Initialize a Histogram for request execution latency
	latencyRecorder, err = meter.Float64Histogram(
		"gokit_request_duration_seconds",
		metric.WithDescription("Latency of requests processed by go-kit endpoints in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		otel.Handle(err)
	}

	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, request any) (any, error) {
			begin := time.Now()

			// Execute the next middleware or core business logic
			response, err := next(ctx, request)

			duration := time.Since(begin).Seconds()

			// Determine execution status for metric labeling
			success := "true"
			if err != nil {
				success = "false"
			}

			// Define the standard set of attributes (labels) for our metrics
			attrs := metric.WithAttributes(
				attribute.String("operation", operationName),
				attribute.String("success", success),
			)

			// Record metrics safely. We pass the ctx to allow OpenTelemetry to automatically
			// associate context metadata (e.g. Exemplars) if supported by your pipeline.
			requestCounter.Add(ctx, 1, attrs)
			latencyRecorder.Record(ctx, duration, attrs)

			return response, err
		}
	}
}

// LoggingMiddleware returns a go-kit endpoint middleware that logs request execution
// details (duration, error status) using the standard library slog logger.
// If logger is nil, it falls back to slog.Default().
//
// It relies on context-aware slog methods to guarantee that the log lines are
// automatically correlated with the active OpenTelemetry TraceID/SpanID.
func LoggingMiddleware(operationName string, logger *slog.Logger) endpoint.Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, request any) (any, error) {
			begin := time.Now()

			// Execute the next middleware or business logic
			response, err := next(ctx, request)

			duration := time.Since(begin)

			// CRITICAL: We use InfoContext and ErrorContext, passing the 'ctx'.
			// This allows the global otelslog handler to find the active TraceID.
			if err != nil {
				logger.ErrorContext(ctx, "endpoint execution failed",
					slog.String("operation", operationName),
					slog.Duration("duration", duration),
					slog.String("err", err.Error()),
				)
				return response, err
			}

			logger.InfoContext(ctx, "endpoint execution succeeded",
				slog.String("operation", operationName),
				slog.Duration("duration", duration),
			)

			return response, err
		}
	}
}

// TracingMiddleware returns a standard go-kit endpoint middleware that wraps the endpoint execution
// in an OpenTelemetry trace span. It fetches the tracer dynamically from the global registry.
func TracingMiddleware(operationName string) endpoint.Middleware {
	// We retrieve the tracer from the global provider. No custom parameters are passed around.
	// "gokit-endpoints" represents the instrumentation library name.
	tracer := otel.Tracer("gokit-endpoints")

	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, request any) (any, error) {
			// Start a new span. If a parent span is already inside the ctx (injected by HTTP/gRPC transports),
			// this automatically creates a child span with correct parent-child hierarchy.
			ctx, span := tracer.Start(ctx, operationName, trace.WithSpanKind(trace.SpanKindInternal))
			defer span.End()

			// Run the next middleware / endpoint business logic
			response, err := next(ctx, request)
			// Record errors if the endpoint failed
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())

				return response, err
			}

			span.SetStatus(codes.Ok, "success")

			return response, err
		}
	}
}
