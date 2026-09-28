// Package telemetry provides a unified, zero-wrapper bootstrap library for
// OpenTelemetry Tracing, Metrics, and Logs, pushing data over OTLP gRPC to
// any compatible collector (Grafana Alloy, OpenTelemetry Collector, ...).
package telemetry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-kit/log"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// instrumentationName identifies this package's instrumentation scope on the
// telemetry it produces (for example the slog bridge's logger).
const instrumentationName = "github.com/pobochiigo/silo/telemetry"

// Defaults applied when the corresponding Config field is zero.
const (
	DefaultMaxExportBatchSize = 500
	DefaultBatchTimeout       = 5 * time.Second
	DefaultMetricInterval     = 30 * time.Second
	defaultOTLPPort           = "4317"
)

// Config represents the parameter set required to wire up your microservices
// to an OTLP gRPC collector (e.g. Grafana Alloy, the OpenTelemetry Collector).
type Config struct {
	ServiceName    string // Name of the application (e.g., "order-service")
	ServiceVersion string // Build/semantic version of the service (e.g., "1.2.4")
	Environment    string // Runtime stage (e.g., "production", "staging", "dev")

	// Endpoint is the address of the OTLP gRPC collector, either host:port
	// ("grafana-alloy.monitoring:4317") or a URL ("https://collector:4317",
	// "http://localhost:4317"). A URL's scheme decides transport security:
	// http means plaintext and https means TLS. In both forms a missing port
	// defaults to 4317, the OTLP gRPC port, rather than to the gRPC default
	// of 443.
	//
	// When set, InitTelemetry opens one gRPC connection that all three
	// exporters share. When empty, each exporter dials on its own, following
	// OTEL_EXPORTER_OTLP_ENDPOINT (and the per-signal variables) and then
	// the OTLP default, localhost:4317.
	Endpoint string

	// AlloyEndpoint is an alias for Endpoint, used only when Endpoint is empty.
	//
	// Deprecated: use Endpoint.
	AlloyEndpoint string

	// Insecure disables transport security for the exporter connections.
	// When false (the default), TLS with the system certificate pool is used.
	// Ignored when Endpoint is a URL, whose scheme decides.
	Insecure bool

	// TLSConfig configures TLS for the exporter connections: a private CA in
	// RootCAs, client certificates for mutual TLS, and so on. Nil uses the
	// system certificate pool. Ignored for plaintext connections.
	TLSConfig *tls.Config

	// Compression names the gRPC compressor used for exports: "gzip", or
	// empty for none.
	Compression string

	// DialOptions are extra gRPC options (keepalive parameters, a custom
	// resolver, ...) applied to the shared connection when Endpoint is set.
	DialOptions []grpc.DialOption

	// Headers are extra gRPC metadata sent with every export request,
	// typically used for collector authentication tokens.
	Headers map[string]string

	// TraceSampleRatio is the fraction of new traces to record, in (0, 1].
	// Child spans always follow their parent's decision. Zero leaves the SDK
	// default: OTEL_TRACES_SAMPLER when set, otherwise every trace.
	TraceSampleRatio float64

	// MaxExportBatchSize is the largest batch of spans or log records sent
	// in one export. Zero means DefaultMaxExportBatchSize.
	MaxExportBatchSize int

	// BatchTimeout is the longest a span or log record waits in a batch
	// before it is exported. Zero means DefaultBatchTimeout.
	BatchTimeout time.Duration

	// MetricInterval is how often metrics are collected and pushed. Zero
	// means DefaultMetricInterval.
	MetricInterval time.Duration

	// SkipHostDetection leaves host.name out of the resource.
	SkipHostDetection bool

	// SkipSlogDefault prevents InitLogs from replacing the process-wide
	// slog default logger with the OTel-bridged logger. The OTel logger
	// provider is still registered globally, so a custom handler can be
	// built with otelslog.NewHandler.
	SkipSlogDefault bool

	// LocalLogHandler receives every log record in addition to the OTLP
	// exporter, so logs stay visible on the machine when the collector is
	// unreachable. When nil, a text handler writing to os.Stderr is used.
	// Ignored when SkipSlogDefault or DisableLocalLogs is set.
	LocalLogHandler slog.Handler

	// LocalLogLevel is the minimum level of the default local handler. Nil
	// means slog.LevelInfo. Ignored when LocalLogHandler is set.
	LocalLogLevel slog.Leveler

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

// exporterTarget resolves Endpoint to a gRPC target and reports whether the
// connection is plaintext. An empty target means the exporters decide on
// their own from the environment.
func (c Config) exporterTarget() (target string, plaintext bool, err error) {
	ep := strings.TrimSpace(c.endpoint())
	if ep == "" {
		return "", c.Insecure, nil
	}
	if !strings.Contains(ep, "://") {
		return withDefaultPort(ep), c.Insecure, nil
	}

	u, err := url.Parse(ep)
	if err != nil {
		return "", false, fmt.Errorf("telemetry: invalid Endpoint %q: %w", ep, err)
	}
	switch u.Scheme {
	case "http":
		plaintext = true
	case "https":
		plaintext = false
	default:
		return "", false, fmt.Errorf("telemetry: Endpoint %q: unsupported scheme %q (use http, https or host:port)", ep, u.Scheme)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("telemetry: Endpoint %q has no host", ep)
	}
	return withDefaultPort(u.Host), plaintext, nil
}

// withDefaultPort returns hostport unchanged when it names a port, and with
// the OTLP default port appended otherwise. Without it a bare host would be
// dialled on gRPC's default port, 443.
func withDefaultPort(hostport string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	return net.JoinHostPort(strings.Trim(hostport, "[]"), defaultOTLPPort)
}

// validate reports configuration that cannot work.
func (c Config) validate() error {
	if c.TraceSampleRatio < 0 || c.TraceSampleRatio > 1 {
		return fmt.Errorf("telemetry: TraceSampleRatio must be within [0, 1], got %g", c.TraceSampleRatio)
	}
	switch c.Compression {
	case "", "gzip":
	default:
		return fmt.Errorf("telemetry: Compression must be \"gzip\" or empty, got %q", c.Compression)
	}
	if c.MaxExportBatchSize < 0 || c.BatchTimeout < 0 || c.MetricInterval < 0 {
		return errors.New("telemetry: MaxExportBatchSize, BatchTimeout and MetricInterval must not be negative")
	}
	_, _, err := c.exporterTarget()
	return err
}

func (c Config) maxExportBatchSize() int {
	if c.MaxExportBatchSize > 0 {
		return c.MaxExportBatchSize
	}
	return DefaultMaxExportBatchSize
}

func (c Config) batchTimeout() time.Duration {
	if c.BatchTimeout > 0 {
		return c.BatchTimeout
	}
	return DefaultBatchTimeout
}

func (c Config) metricInterval() time.Duration {
	if c.MetricInterval > 0 {
		return c.MetricInterval
	}
	return DefaultMetricInterval
}

// ShutdownFunc safely flushes and releases OTel collectors on app termination.
type ShutdownFunc func(context.Context) error

// NewResource defines the standard service metadata injected into all Traces,
// Metrics, and Logs. Besides the attributes derived from cfg it includes the
// telemetry SDK attributes, host.name (unless cfg.SkipHostDetection), and
// honours OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES; non-empty cfg
// values take precedence over the environment, which takes precedence over
// detection.
//
// The resource's schema URL comes from the SDK's own detectors, so it always
// matches the semantic-conventions version the installed SDK was built with.
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

	// No explicit schema URL: the SDK detectors carry the one that matches
	// their semantic conventions, and pinning a different version here would
	// make resource.New fail with a schema conflict after an SDK upgrade.
	opts := []resource.Option{resource.WithTelemetrySDK()}
	if !cfg.SkipHostDetection {
		opts = append(opts, resource.WithHost())
	}
	opts = append(opts,
		resource.WithFromEnv(),
		resource.WithAttributes(attrs...),
	)

	res, err := resource.New(ctx, opts...)
	switch {
	case errors.Is(err, resource.ErrPartialResource):
		// A detector failed (for example no hostname is available); the
		// attributes that were detected are still valid.
		slog.Warn("telemetry: resource detection incomplete", slog.Any("error", err))
		return res, nil
	case errors.Is(err, resource.ErrSchemaURLConflict):
		// Detectors disagreed on the schema version; the merged attributes
		// are still valid, only the schema URL is dropped.
		slog.Warn("telemetry: resource schema URL conflict, continuing without one", slog.Any("error", err))
		return res, nil
	}
	return res, err
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

// exporterOptionSet adapts one exporter package's option constructors so the
// three OTLP exporters can be configured by the same code.
type exporterOptionSet[O any] struct {
	grpcConn   func(*grpc.ClientConn) O
	endpoint   func(string) O
	insecure   func() O
	tlsCreds   func(credentials.TransportCredentials) O
	compressor func(string) O
	headers    func(map[string]string) O
}

var (
	traceOptionSet = exporterOptionSet[otlptracegrpc.Option]{
		grpcConn:   otlptracegrpc.WithGRPCConn,
		endpoint:   otlptracegrpc.WithEndpoint,
		insecure:   otlptracegrpc.WithInsecure,
		tlsCreds:   otlptracegrpc.WithTLSCredentials,
		compressor: otlptracegrpc.WithCompressor,
		headers:    otlptracegrpc.WithHeaders,
	}
	metricOptionSet = exporterOptionSet[otlpmetricgrpc.Option]{
		grpcConn:   otlpmetricgrpc.WithGRPCConn,
		endpoint:   otlpmetricgrpc.WithEndpoint,
		insecure:   otlpmetricgrpc.WithInsecure,
		tlsCreds:   otlpmetricgrpc.WithTLSCredentials,
		compressor: otlpmetricgrpc.WithCompressor,
		headers:    otlpmetricgrpc.WithHeaders,
	}
	logOptionSet = exporterOptionSet[otlploggrpc.Option]{
		grpcConn:   otlploggrpc.WithGRPCConn,
		endpoint:   otlploggrpc.WithEndpoint,
		insecure:   otlploggrpc.WithInsecure,
		tlsCreds:   otlploggrpc.WithTLSCredentials,
		compressor: otlploggrpc.WithCompressor,
		headers:    otlploggrpc.WithHeaders,
	}
)

// exporterOptions builds one exporter's options: the shared connection when
// there is one, otherwise the endpoint and transport security from cfg (or
// nothing, so the exporter follows the OTLP environment variables).
func exporterOptions[O any](cfg Config, conn *grpc.ClientConn, set exporterOptionSet[O]) ([]O, error) {
	var opts []O
	if conn != nil {
		opts = append(opts, set.grpcConn(conn))
	} else {
		target, plaintext, err := cfg.exporterTarget()
		if err != nil {
			return nil, err
		}
		if target != "" {
			opts = append(opts, set.endpoint(target))
		}
		switch {
		case plaintext:
			opts = append(opts, set.insecure())
		case cfg.TLSConfig != nil:
			opts = append(opts, set.tlsCreds(credentials.NewTLS(cfg.TLSConfig)))
		}
	}
	if cfg.Compression != "" {
		opts = append(opts, set.compressor(cfg.Compression))
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, set.headers(cfg.Headers))
	}
	return opts, nil
}

// dialCollector opens the gRPC connection the exporters share when Endpoint
// is set. It returns nil when the exporters should dial on their own. The
// connection is established lazily, on the first export.
func dialCollector(cfg Config) (*grpc.ClientConn, error) {
	target, plaintext, err := cfg.exporterTarget()
	if err != nil || target == "" {
		return nil, err
	}
	creds := credentials.NewTLS(cfg.TLSConfig)
	if plaintext {
		creds = insecure.NewCredentials()
	}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, cfg.DialOptions...)
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: failed to create gRPC client for %s: %w", target, err)
	}
	return conn, nil
}

// InitTraces configures the global tracer provider, registers the OTLP/gRPC exporter,
// sets up batch processing, and initializes standard context propagators.
func InitTraces(ctx context.Context, cfg Config, res *resource.Resource) (ShutdownFunc, error) {
	return initTraces(ctx, cfg, res, nil)
}

func initTraces(ctx context.Context, cfg Config, res *resource.Resource, conn *grpc.ClientConn) (ShutdownFunc, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	opts, err := exporterOptions(cfg, conn, traceOptionSet)
	if err != nil {
		return nil, err
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxExportBatchSize(cfg.maxExportBatchSize()),
			sdktrace.WithBatchTimeout(cfg.batchTimeout()),
		),
		sdktrace.WithResource(res),
	}
	if ratio := cfg.TraceSampleRatio; ratio > 0 {
		tpOpts = append(tpOpts, sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))))
	}
	tp := sdktrace.NewTracerProvider(tpOpts...)

	// Set global tracing provider for raw 'otel.Tracer()' calls
	otel.SetTracerProvider(tp)

	InitPropagators()

	return tp.Shutdown, nil
}

// InitMetrics bootstraps the periodic push metric pipeline to the collector.
func InitMetrics(ctx context.Context, cfg Config, res *resource.Resource) (ShutdownFunc, error) {
	return initMetrics(ctx, cfg, res, nil)
}

func initMetrics(ctx context.Context, cfg Config, res *resource.Resource, conn *grpc.ClientConn) (ShutdownFunc, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	opts, err := exporterOptions(cfg, conn, metricOptionSet)
	if err != nil {
		return nil, err
	}

	exporter, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP metrics exporter: %w", err)
	}

	// Read state metrics in memory and push cumulative state to the collector periodically
	reader := sdkmetric.NewPeriodicReader(
		exporter,
		sdkmetric.WithInterval(cfg.metricInterval()),
	)

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)

	// Set global metrics provider for raw 'otel.Meter()' calls
	otel.SetMeterProvider(mp)

	return mp.Shutdown, nil
}

// InitLogs instantiates the OpenTelemetry logs engine, registers its logger
// provider globally, and, unless cfg.SkipSlogDefault is set, installs a
// default slog logger that writes every record both locally
// (cfg.LocalLogHandler, or stderr) and to the OTLP exporter, so logs carry
// the active trace/span IDs without disappearing from the machine when the
// collector is down.
func InitLogs(ctx context.Context, cfg Config, res *resource.Resource) (ShutdownFunc, error) {
	return initLogs(ctx, cfg, res, nil)
}

func initLogs(ctx context.Context, cfg Config, res *resource.Resource, conn *grpc.ClientConn) (ShutdownFunc, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	opts, err := exporterOptions(cfg, conn, logOptionSet)
	if err != nil {
		return nil, err
	}

	exporter, err := otlploggrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}

	processor := sdklog.NewBatchProcessor(exporter,
		sdklog.WithExportMaxBatchSize(cfg.maxExportBatchSize()),
		sdklog.WithExportInterval(cfg.batchTimeout()),
	)

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(processor),
		sdklog.WithResource(res),
	)

	// Make the pipeline reachable for custom handlers (otelslog.NewHandler
	// without an explicit provider) even when the slog default is kept.
	global.SetLoggerProvider(lp)

	// Inject OTel logs pipeline into Go's standard logger 'slog'
	if !cfg.SkipSlogDefault {
		slog.SetDefault(slog.New(defaultLogHandler(cfg, lp)))
	}

	return lp.Shutdown, nil
}

// defaultLogHandler builds the slog handler InitLogs installs: the OTel
// bridge, fanned out with a local handler unless cfg.DisableLocalLogs is set.
func defaultLogHandler(cfg Config, lp *sdklog.LoggerProvider) slog.Handler {
	otelHandler := otelslog.NewHandler(instrumentationName, otelslog.WithLoggerProvider(lp))
	if cfg.DisableLocalLogs {
		return otelHandler
	}
	return NewFanoutHandler(localLogHandler(cfg), otelHandler)
}

// localLogHandler returns the handler that keeps logs visible on the machine.
func localLogHandler(cfg Config) slog.Handler {
	if cfg.LocalLogHandler != nil {
		return cfg.LocalLogHandler
	}
	return slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LocalLogLevel})
}

// InitTelemetry simplifies bootstrap logic by configuring all three pipelines
// in one step, returning a composite ShutdownFunc that safely winds down the
// stack. When cfg.Endpoint is set the three exporters share one gRPC
// connection, closed by the returned ShutdownFunc.
func InitTelemetry(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	res, err := NewResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to establish OTel resource attributes: %w", err)
	}

	conn, err := dialCollector(cfg)
	if err != nil {
		return nil, err
	}
	closeConn := func() {
		if conn != nil {
			_ = conn.Close()
		}
	}

	traceShutdown, err := initTraces(ctx, cfg, res, conn)
	if err != nil {
		closeConn()
		return nil, err
	}

	metricShutdown, err := initMetrics(ctx, cfg, res, conn)
	if err != nil {
		_ = traceShutdown(ctx) // Attempt cleaning traces if metrics setup fails
		closeConn()
		return nil, err
	}

	logShutdown, err := initLogs(ctx, cfg, res, conn)
	if err != nil {
		_ = traceShutdown(ctx)
		_ = metricShutdown(ctx)
		closeConn()
		return nil, err
	}

	// Unified execution of gracefully closing resources
	return func(shutdownCtx context.Context) error {
		var errs []error
		for _, shutdown := range []ShutdownFunc{logShutdown, metricShutdown, traceShutdown} {
			if err := shutdown(shutdownCtx); err != nil {
				errs = append(errs, err)
			}
		}
		if conn != nil {
			if err := conn.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}, nil
}

// NewLogger creates a new go-kit compatible logger backed by slog.
//
// Deprecated: use NewSlogAdapter, which this function delegates to.
func NewLogger(ctx context.Context, _ Config) log.Logger {
	return NewSlogAdapter(ctx)
}
