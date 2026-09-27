// Package telemetry provides a unified, zero-wrapper bootstrap library for
// OpenTelemetry Tracing, Metrics, and Logs, pushing data over OTLP gRPC to
// any compatible collector (Grafana Alloy, OpenTelemetry Collector, ...).
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/go-kit/log"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// Default telemetry configuration limits matched to your existing architecture metrics.
const (
	maxBatchSize   = 500
	batchTimeout   = 5 * time.Second
	metricInterval = 30 * time.Second
)

// Config represents the parameter set required to wire up your microservices
// to an OTLP gRPC collector (e.g. Grafana Alloy, the OpenTelemetry Collector).
type Config struct {
	ServiceName    string // Name of the application (e.g., "order-service")
	ServiceVersion string // Build/semantic version of the service (e.g., "1.2.4")
	Environment    string // Runtime stage (e.g., "production", "staging", "dev")

	// Endpoint is the host:port of the OTLP gRPC collector
	// (e.g. "grafana-alloy.monitoring:4317"). When empty, the exporters fall
	// back to the OTEL_EXPORTER_OTLP_ENDPOINT environment variable and then
	// to the OTLP default, localhost:4317.
	Endpoint string

	// AlloyEndpoint is an alias for Endpoint, used only when Endpoint is empty.
	//
	// Deprecated: use Endpoint.
	AlloyEndpoint string

	// Insecure disables transport security for the exporter connections.
	// When false (the default), TLS with the system certificate pool is used.
	Insecure bool

	// Headers are extra gRPC metadata sent with every export request,
	// typically used for collector authentication tokens.
	Headers map[string]string

	// SkipSlogDefault prevents InitLogs from replacing the process-wide
	// slog default logger with the OTel-bridged logger.
	SkipSlogDefault bool

	// LocalLogHandler receives every log record in addition to the OTLP
	// exporter, so logs stay visible on the machine when the collector is
	// unreachable. When nil, a text handler writing to os.Stderr is used.
	// Ignored when SkipSlogDefault or DisableLocalLogs is set.
	LocalLogHandler slog.Handler

	// DisableLocalLogs sends slog output to the collector only. Note that
	// slog.SetDefault also routes the standard library "log" package through
	// the default slog handler, so with this set nothing is written locally.
	DisableLocalLogs bool
}

func (c Config) endpoint() string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	return c.AlloyEndpoint
}

// ShutdownFunc safely flushes and releases OTel collectors on app termination.
type ShutdownFunc func(context.Context) error

// NewResource defines the standard service metadata injected into all Traces,
// Metrics, and Logs. Besides the attributes derived from cfg it includes the
// telemetry SDK attributes and honours OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES; non-empty cfg values take precedence over the
// environment.
//
// The environment is emitted under both deployment.environment.name (current
// semantic conventions) and deployment.environment (pre-1.27 name) so existing
// dashboards keep working.
func NewResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	var attrs []attribute.KeyValue
	if cfg.ServiceName != "" {
		attrs = append(attrs, semconv.ServiceNameKey.String(cfg.ServiceName))
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersionKey.String(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs,
			semconv.DeploymentEnvironmentNameKey.String(cfg.Environment),
			attribute.String("deployment.environment", cfg.Environment),
		)
	}

	return resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
		resource.WithAttributes(attrs...),
	)
}

// InitPropagators registers the standard W3C TraceContext and Baggage
// propagators globally, enabling context propagation across network layers
// (HTTP headers, gRPC metadata). It is called by InitTraces; applications
// that skip tracing but still forward trace headers can call it directly.
func InitPropagators() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

// InitTraces configures the global tracer provider, registers the OTLP/gRPC exporter,
// sets up batch processing, and initializes standard context propagators.
func InitTraces(ctx context.Context, cfg Config, res *resource.Resource) (ShutdownFunc, error) {
	var opts []otlptracegrpc.Option
	if ep := cfg.endpoint(); ep != "" {
		opts = append(opts, otlptracegrpc.WithEndpoint(ep))
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxExportBatchSize(maxBatchSize),
			sdktrace.WithBatchTimeout(batchTimeout),
		),
		sdktrace.WithResource(res),
	)

	// Set global tracing provider for raw 'otel.Tracer()' calls
	otel.SetTracerProvider(tp)

	InitPropagators()

	return tp.Shutdown, nil
}

// InitMetrics bootstraps the periodic push metric pipeline to the collector.
func InitMetrics(ctx context.Context, cfg Config, res *resource.Resource) (ShutdownFunc, error) {
	var opts []otlpmetricgrpc.Option
	if ep := cfg.endpoint(); ep != "" {
		opts = append(opts, otlpmetricgrpc.WithEndpoint(ep))
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.Headers))
	}

	exporter, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP metrics exporter: %w", err)
	}

	// Read state metrics in memory and push cumulative state to Alloy every 30 seconds
	reader := sdkmetric.NewPeriodicReader(
		exporter,
		sdkmetric.WithInterval(metricInterval),
	)

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)

	// Set global metrics provider for raw 'otel.Meter()' calls
	otel.SetMeterProvider(mp)

	return mp.Shutdown, nil
}

// InitLogs instantiates the OpenTelemetry logs engine and, unless
// cfg.SkipSlogDefault is set, installs a default slog logger that writes
// every record both locally (cfg.LocalLogHandler, or stderr) and to the OTLP
// exporter, so logs carry the active trace/span IDs without disappearing
// from the machine when the collector is down.
func InitLogs(ctx context.Context, cfg Config, res *resource.Resource) (ShutdownFunc, error) {
	var opts []otlploggrpc.Option
	if ep := cfg.endpoint(); ep != "" {
		opts = append(opts, otlploggrpc.WithEndpoint(ep))
	}
	if cfg.Insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploggrpc.WithHeaders(cfg.Headers))
	}

	exporter, err := otlploggrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}

	processor := sdklog.NewBatchProcessor(exporter)

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(processor),
		sdklog.WithResource(res),
	)

	// Inject OTel logs pipeline into Go's standard logger 'slog'
	if !cfg.SkipSlogDefault {
		slog.SetDefault(slog.New(defaultLogHandler(cfg, lp)))
	}

	return lp.Shutdown, nil
}

// defaultLogHandler builds the slog handler InitLogs installs: the OTel
// bridge, fanned out with a local handler unless cfg.DisableLocalLogs is set.
func defaultLogHandler(cfg Config, lp *sdklog.LoggerProvider) slog.Handler {
	otelHandler := otelslog.NewHandler(cfg.ServiceName, otelslog.WithLoggerProvider(lp))
	if cfg.DisableLocalLogs {
		return otelHandler
	}
	local := cfg.LocalLogHandler
	if local == nil {
		local = slog.NewTextHandler(os.Stderr, nil)
	}
	return NewFanoutHandler(local, otelHandler)
}

// InitTelemetry simplifies bootstrap logic by configuring all three pipelines
// in one step, returning a composite ShutdownFunc that safely winds down the stack.
func InitTelemetry(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	res, err := NewResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to establish OTel resource attributes: %w", err)
	}

	traceShutdown, err := InitTraces(ctx, cfg, res)
	if err != nil {
		return nil, err
	}

	metricShutdown, err := InitMetrics(ctx, cfg, res)
	if err != nil {
		_ = traceShutdown(ctx) // Attempt cleaning traces if metrics setup fails
		return nil, err
	}

	logShutdown, err := InitLogs(ctx, cfg, res)
	if err != nil {
		_ = traceShutdown(ctx)
		_ = metricShutdown(ctx)
		return nil, err
	}

	// Unified execution of gracefully closing resources
	return func(shutdownCtx context.Context) error {
		var firstErr error
		if err := logShutdown(shutdownCtx); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := metricShutdown(shutdownCtx); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := traceShutdown(shutdownCtx); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	}, nil
}

// NewLogger creates a new go-kit compatible logger backed by slog.
//
// Deprecated: use NewSlogAdapter, which this function delegates to.
func NewLogger(ctx context.Context, _ Config) log.Logger {
	return NewSlogAdapter(ctx)
}
