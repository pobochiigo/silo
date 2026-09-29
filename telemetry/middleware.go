package telemetry

import (
	"context"
	"log/slog"
	"time"

	kitendpoint "github.com/go-kit/kit/endpoint"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/pobochiigo/silo/endpoint"
	"github.com/pobochiigo/silo/middleware"
)

// Tracing returns a middleware that wraps every call of a typed endpoint in
// a span named operationName, of kind internal, from the global tracer
// provider. An error is recorded on the span and sets its status; success
// leaves the status unset, as the OpenTelemetry specification asks of
// instrumentation.
//
// The request and response are never inspected, so the type parameters only
// carry the endpoint's types through. For a go-kit endpoint instantiate with
// [any, any] and convert with [Kit].
func Tracing[Req, Resp any](operationName string) middleware.Middleware[endpoint.Endpoint[Req, Resp]] {
	tracer := otel.Tracer(instrumentationName)
	return func(next endpoint.Endpoint[Req, Resp]) endpoint.Endpoint[Req, Resp] {
		return func(ctx context.Context, request Req) (Resp, error) {
			ctx, span := tracer.Start(ctx, operationName, trace.WithSpanKind(trace.SpanKindInternal))
			defer span.End()

			response, err := next(ctx, request)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return response, err
		}
	}
}

// Logging returns a middleware that logs one record per call of a typed
// endpoint: "endpoint execution succeeded" at Info, or "endpoint execution
// failed" at Error with the error, both with the operation and the duration.
// Records go through the context-aware slog methods, so they carry the
// active trace and span IDs. A nil logger means slog.Default(), resolved on
// every call, so the middleware can be built before InitTelemetry installs
// the default logger.
func Logging[Req, Resp any](operationName string, logger *slog.Logger) middleware.Middleware[endpoint.Endpoint[Req, Resp]] {
	return func(next endpoint.Endpoint[Req, Resp]) endpoint.Endpoint[Req, Resp] {
		return func(ctx context.Context, request Req) (Resp, error) {
			begin := time.Now()
			response, err := next(ctx, request)
			duration := time.Since(begin)

			l := logger
			if l == nil {
				l = slog.Default()
			}
			if err != nil {
				l.ErrorContext(ctx, "endpoint execution failed",
					slog.String("operation", operationName),
					slog.Duration("duration", duration),
					slog.Any("error", err),
				)
				return response, err
			}
			l.InfoContext(ctx, "endpoint execution succeeded",
				slog.String("operation", operationName),
				slog.Duration("duration", duration),
			)
			return response, err
		}
	}
}

// Metrics returns a middleware that records every call of a typed endpoint
// on recorder as method operationName: one request, its latency and, on
// failure, one error, in <subsystem>_requests_total,
// <subsystem>_request_duration_seconds and <subsystem>_errors_total with the
// attribute method. The subsystem is the recorder's, so a service names its
// own series (auth_endpoint_requests_total for a recorder created with
// NewMetricsRecorder(meter, "auth_endpoint")), and one recorder serves every
// endpoint of that subsystem. The generated metrics middlewares record on
// the same instruments with the interface name as subsystem.
func Metrics[Req, Resp any](recorder *MetricsRecorder, operationName string) middleware.Middleware[endpoint.Endpoint[Req, Resp]] {
	return func(next endpoint.Endpoint[Req, Resp]) endpoint.Endpoint[Req, Resp] {
		return func(ctx context.Context, request Req) (Resp, error) {
			start := time.Now()
			response, err := next(ctx, request)
			recorder.Observe(ctx, operationName, start, err)
			return response, err
		}
	}
}

// TracingMiddleware is Tracing for a go-kit endpoint.
//
// Deprecated: use Kit(Tracing[any, any](operationName)).
func TracingMiddleware(operationName string) kitendpoint.Middleware {
	return Kit(Tracing[any, any](operationName))
}

// LoggingMiddleware is Logging for a go-kit endpoint.
//
// Deprecated: use Kit(Logging[any, any](operationName, logger)).
func LoggingMiddleware(operationName string, logger *slog.Logger) kitendpoint.Middleware {
	return Kit(Logging[any, any](operationName, logger))
}

// MetricsMiddleware is Metrics for a go-kit endpoint on a recorder for the
// subsystem "endpoint", created on this package's meter: the instruments are
// endpoint_requests_total, endpoint_errors_total and
// endpoint_request_duration_seconds with the attribute method. Earlier
// versions recorded gokit_requests_total and gokit_request_duration_seconds
// with the attributes operation and success.
//
// Deprecated: use Kit(Metrics[any, any](recorder, operationName)) with a
// recorder for your own subsystem.
func MetricsMiddleware(operationName string) kitendpoint.Middleware {
	recorder := NewMetricsRecorder(otel.Meter(instrumentationName), "endpoint")
	return Kit(Metrics[any, any](recorder, operationName))
}
