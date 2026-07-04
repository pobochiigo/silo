package telemetry

import (
	"context"
	"net/http"

	grpctransport "github.com/go-kit/kit/transport/grpc"
	httptransport "github.com/go-kit/kit/transport/http"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc/metadata"
)

// ExtractHTTPTraceContext extracts OTel tracing context from HTTP Headers
// and injects it into the Go-kit context before the endpoint runs.
func ExtractHTTPTraceContext() httptransport.RequestFunc {
	// Using composite context & baggage propagator
	propagator := otel.GetTextMapPropagator()

	return func(ctx context.Context, r *http.Request) context.Context {
		// Reads standard headers (like traceparent, tracestate, baggage)
		// and returns a context carrying the active parent span data.
		return propagator.Extract(ctx, propagation.HeaderCarrier(r.Header))
	}
}

// ExtractGRPCTraceContext extracts OTel tracing context from gRPC metadata.
func ExtractGRPCTraceContext() grpctransport.ServerRequestFunc {
	propagator := otel.GetTextMapPropagator()

	return func(ctx context.Context, md metadata.MD) context.Context {
		// Wrap gRPC metadata as a carrier compatible with OpenTelemetry
		carrier := propagation.MapCarrier{}
		for k, vs := range md {
			if len(vs) > 0 {
				carrier[k] = vs[0]
			}
		}
		return propagator.Extract(ctx, carrier)
	}
}

// InjectHTTPTraceContext returns a Go-kit ClientRequestFunc that extracts the active
// trace/baggage context from the execution thread and injects it into outbound HTTP headers.
func InjectHTTPTraceContext() httptransport.RequestFunc {
	propagator := otel.GetTextMapPropagator()

	return func(ctx context.Context, r *http.Request) context.Context {
		// Inject writes standard headers (e.g., traceparent, tracestate, baggage)
		// directly into the outgoing HTTP request headers.
		propagator.Inject(ctx, propagation.HeaderCarrier(r.Header))
		return ctx
	}
}

// InjectGRPCTraceContext returns a Go-kit ClientRequestFunc that injects the active
// trace/baggage context from the execution thread into outgoing gRPC metadata.
func InjectGRPCTraceContext() grpctransport.ClientRequestFunc {
	propagator := otel.GetTextMapPropagator()

	return func(ctx context.Context, md *metadata.MD) context.Context {
		// Guard against nil metadata reference
		if *md == nil {
			*md = metadata.MD{}
		}

		// Adapt the standard gRPC metadata to meet OTel's TextMapCarrier interface
		carrier := propagation.MapCarrier{}
		propagator.Inject(ctx, carrier)

		// Set the values back into the gRPC metadata map
		for k, v := range carrier {
			md.Set(k, v)
		}

		return ctx
	}
}
