package telemetry

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

func TestTraceContextPropagation(t *testing.T) {
	// Set up global propagator
	oldPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTextMapPropagator(oldPropagator)

	// Create a dummy span context
	traceID, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	spanID, _ := trace.SpanIDFromHex("0123456789abcdef")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})

	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	t.Run("HTTP injection and extraction", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://localhost", nil)
		assert.NoError(t, err)

		injectFunc := InjectHTTPTraceContext()
		_ = injectFunc(ctx, req)

		// The header should contain traceparent
		assert.NotEmpty(t, req.Header.Get("traceparent"))

		extractFunc := ExtractHTTPTraceContext()
		extractedCtx := extractFunc(context.Background(), req)

		extractedSpanCtx := trace.SpanContextFromContext(extractedCtx)
		assert.Equal(t, spanCtx.TraceID(), extractedSpanCtx.TraceID())
		assert.Equal(t, spanCtx.SpanID(), extractedSpanCtx.SpanID())
	})

	t.Run("gRPC injection and extraction", func(t *testing.T) {
		md := metadata.MD{}
		injectFunc := InjectGRPCTraceContext()
		_ = injectFunc(ctx, &md)

		// The metadata should contain traceparent
		assert.NotEmpty(t, md.Get("traceparent"))

		extractFunc := ExtractGRPCTraceContext()
		extractedCtx := extractFunc(context.Background(), md)

		extractedSpanCtx := trace.SpanContextFromContext(extractedCtx)
		assert.Equal(t, spanCtx.TraceID(), extractedSpanCtx.TraceID())
		assert.Equal(t, spanCtx.SpanID(), extractedSpanCtx.SpanID())
	})
}
