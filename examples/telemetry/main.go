// Command telemetry shows the telemetry package around a go-kit HTTP
// endpoint: the endpoint middlewares (tracing, logging, metrics), trace
// context propagation from a client to a server over HTTP headers and over
// gRPC metadata, the go-kit log adapter and the fan-out slog handler.
//
// Without OTEL_EXPORTER_OTLP_ENDPOINT every span is printed as it ends and
// the metrics are printed at the end; with it (see docker-compose.yml) they go
// to the collector through telemetry.InitTelemetry.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/go-kit/kit/endpoint"
	"github.com/go-kit/kit/transport"
	httptransport "github.com/go-kit/kit/transport/http"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"

	"github.com/pobochiigo/silo/telemetry"

	"github.com/pobochiigo/silo/examples/internal/demo"
)

type greetRequest struct {
	Name string `json:"name"`
}

type greetResponse struct {
	Greeting string `json:"greeting"`
	TraceID  string `json:"trace_id"`
}

var errEmptyName = errors.New("name must not be empty")

func main() {
	verbose := flag.Bool("v", false, "debug logging")
	flag.Parse()

	ctx, stop := demo.Context()
	defer stop()
	shutdown := demo.Telemetry(ctx, "silo-example-telemetry", *verbose)
	defer shutdown()

	demo.Step(1, "A go-kit endpoint wrapped with the tracing, logging and metrics middlewares, served over HTTP")
	// The business endpoint reports the trace ID it sees so the client can
	// compare it with its own.
	var greet endpoint.Endpoint = func(ctx context.Context, request any) (any, error) {
		req := request.(greetRequest)
		if req.Name == "" {
			return nil, errEmptyName
		}
		return greetResponse{
			Greeting: "Hello, " + req.Name,
			TraceID:  trace.SpanContextFromContext(ctx).TraceID().String(),
		}, nil
	}
	greet = telemetry.MetricsMiddleware("greet")(greet)
	greet = telemetry.LoggingMiddleware("greet", nil)(greet)
	greet = telemetry.TracingMiddleware("greet")(greet)

	// go-kit's transport reports its own errors through a go-kit logger; the
	// adapter routes them through slog, and so through the OTLP pipeline.
	kitLogger := telemetry.NewSlogAdapter(ctx)
	server := httptransport.NewServer(greet,
		func(_ context.Context, r *http.Request) (any, error) {
			var req greetRequest
			return req, json.NewDecoder(r.Body).Decode(&req)
		},
		func(_ context.Context, w http.ResponseWriter, response any) error {
			return json.NewEncoder(w).Encode(response)
		},
		// ServerBefore runs before the endpoint: the incoming traceparent
		// header becomes the parent of the server span.
		httptransport.ServerBefore(telemetry.ExtractHTTPTraceContext()),
		httptransport.ServerErrorHandler(transport.NewLogErrorHandler(kitLogger)),
	)
	ts := httptest.NewServer(server)
	defer ts.Close()
	fmt.Println("   listening on", ts.URL)

	demo.Step(2, "A go-kit HTTP client injects the trace context; server and client end up in one trace")
	target, err := url.Parse(ts.URL)
	if err != nil {
		demo.Fail("url", err)
	}
	var client endpoint.Endpoint = httptransport.NewClient(http.MethodPost, target,
		func(_ context.Context, r *http.Request, request any) error {
			body, err := json.Marshal(request)
			if err != nil {
				return err
			}
			r.ContentLength = int64(len(body))
			r.Body = io.NopCloser(bytes.NewReader(body))
			return nil
		},
		func(_ context.Context, r *http.Response) (any, error) {
			if r.StatusCode != http.StatusOK {
				// go-kit's default error encoder writes the endpoint error as
				// plain text; hand it back instead of decoding it as JSON.
				body, _ := io.ReadAll(r.Body)
				return nil, fmt.Errorf("server replied %d: %s", r.StatusCode, strings.TrimSpace(string(body)))
			}
			var resp greetResponse
			return resp, json.NewDecoder(r.Body).Decode(&resp)
		},
		// ClientBefore runs before the request is sent: the active span is
		// written to the traceparent header.
		httptransport.ClientBefore(telemetry.InjectHTTPTraceContext()),
	).Endpoint()
	client = telemetry.TracingMiddleware("greet.client")(client)

	// A root span stands for the caller's own work; everything below hangs off it.
	rootCtx, root := otel.Tracer("example").Start(ctx, "example.main")
	resp, err := client(rootCtx, greetRequest{Name: "Ada"})
	if err != nil {
		demo.Fail("call", err)
	}
	clientTrace := root.SpanContext().TraceID().String()
	root.End()
	fmt.Printf("   response: %q\n", resp.(greetResponse).Greeting)
	fmt.Printf("   trace id seen by the client: %s\n", clientTrace)
	fmt.Printf("   trace id seen by the server: %s\n", resp.(greetResponse).TraceID)
	if resp.(greetResponse).TraceID != clientTrace {
		demo.Fail("propagation", errors.New("the server did not join the client's trace"))
	}

	demo.Step(3, "An endpoint error: the span is marked, the log line is at Error level, the metric has success=false")
	_, err = client(ctx, greetRequest{Name: ""})
	fmt.Printf("   client received: %v\n", err)

	demo.Step(4, "The same propagation over gRPC metadata, without a gRPC server")
	spanCtx, span := otel.Tracer("example").Start(ctx, "example.grpc-caller")
	md := metadata.MD{}
	telemetry.InjectGRPCTraceContext()(spanCtx, &md)
	span.End()
	fmt.Printf("   outgoing metadata: traceparent=%q\n", md.Get("traceparent"))
	incoming := telemetry.ExtractGRPCTraceContext()(context.Background(), md)
	fmt.Printf("   extracted on the callee side: trace=%s parent span=%s\n",
		trace.SpanContextFromContext(incoming).TraceID(), trace.SpanContextFromContext(incoming).SpanID())
	if trace.SpanContextFromContext(incoming).TraceID() != span.SpanContext().TraceID() {
		demo.Fail("grpc propagation", errors.New("trace id did not survive the metadata round trip"))
	}

	demo.Step(5, "go-kit style logging through the slog adapter, and a fan-out handler counting records by level")
	var infos, errs atomic.Int64
	counting := &countingHandler{infos: &infos, errs: &errs}
	previous := slog.Default()
	slog.SetDefault(slog.New(telemetry.NewFanoutHandler(previous.Handler(), counting)))
	kitLogger.Log("level", "info", "msg", "handled through the adapter", "component", "server", "ts", "dropped by the adapter")
	kitLogger.Log("level", "error", "msg", "adapter maps err to error", "err", errEmptyName)
	slog.SetDefault(previous)
	fmt.Printf("   fan-out counted %d info and %d error records\n", infos.Load(), errs.Load())

	demo.Step(6, "Metrics recorded by MetricsMiddleware")
	demo.PrintMetrics(ctx)
}

// countingHandler is a minimal slog.Handler that counts records by level.
type countingHandler struct {
	infos, errs *atomic.Int64
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.errs.Add(1)
	} else {
		h.infos.Add(1)
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }
