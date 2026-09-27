package telemetry

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
)

// restoreGlobals resets the slog default and the OTel global providers when
// the test ends, so providers shut down by one test are not used by another.
func restoreGlobals(t *testing.T) {
	t.Helper()
	prevSlog := slog.Default()
	prevLogs := global.GetLoggerProvider()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		global.SetLoggerProvider(prevLogs)
	})
}

// countingListener counts accepted connections so a test can prove an
// exporter actually dialled this address.
type countingListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

func TestInitTelemetry_EmptyEndpointHonoursEnvironment(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	lis := &countingListener{Listener: raw}

	s := grpc.NewServer()
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	// No Endpoint in Config: the exporters must fall back to the standard
	// OTLP environment variable instead of dialling an empty address.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+lis.Addr().String())

	restoreGlobals(t)

	ctx := context.Background()
	shutdown, err := InitTelemetry(ctx, Config{ServiceName: "env-test", Insecure: true})
	require.NoError(t, err)

	// Produce a span so the trace exporter has something to flush.
	_, span := otel.Tracer("env-test").Start(ctx, "op")
	span.End()
	_ = shutdown(ctx) // the dummy server answers Unimplemented; only the dial matters

	assert.Greater(t, lis.accepted.Load(), int32(0), "exporter never connected to the env endpoint")
}

func TestNewResource(t *testing.T) {
	ctx := context.Background()
	t.Setenv("OTEL_SERVICE_NAME", "from-env")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=platform")

	attrsOf := func(cfg Config) map[string]string {
		t.Helper()
		res, err := NewResource(ctx, cfg)
		require.NoError(t, err)
		assert.NotEmpty(t, res.SchemaURL())
		out := map[string]string{}
		for _, kv := range res.Attributes() {
			out[string(kv.Key)] = kv.Value.AsString()
		}
		return out
	}

	explicit := attrsOf(Config{ServiceName: "explicit", ServiceVersion: "1.2.3", Environment: "prod"})
	assert.Equal(t, "explicit", explicit["service.name"], "config wins over OTEL_SERVICE_NAME")
	assert.Equal(t, "1.2.3", explicit["service.version"])
	assert.Equal(t, "prod", explicit["deployment.environment.name"])
	assert.Equal(t, "prod", explicit["deployment.environment"], "legacy key kept for existing dashboards")
	assert.Equal(t, "platform", explicit["team"], "OTEL_RESOURCE_ATTRIBUTES merged")
	assert.Equal(t, "opentelemetry", explicit["telemetry.sdk.name"])
	assert.Equal(t, "go", explicit["telemetry.sdk.language"])

	blank := attrsOf(Config{})
	assert.Equal(t, "from-env", blank["service.name"], "environment fills blanks")
	_, hasEnv := blank["deployment.environment.name"]
	assert.False(t, hasEnv, "empty config values must not emit empty attributes")
}

func TestInitTelemetry(t *testing.T) {
	// Start a mock gRPC server to receive telemetry connections
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := grpc.NewServer()
	go func() {
		_ = s.Serve(lis)
	}()
	defer s.Stop()
	defer lis.Close()

	cfg := Config{
		ServiceName:    "test-service",
		ServiceVersion: "1.0.0",
		Environment:    "test",
		Endpoint:       lis.Addr().String(),
		Insecure:       true,
	}

	ctx := context.Background()

	t.Run("deprecated AlloyEndpoint alias", func(t *testing.T) {
		assert.Equal(t, "alias:4317", Config{AlloyEndpoint: "alias:4317"}.endpoint())
		assert.Equal(t, "new:4317", Config{Endpoint: "new:4317", AlloyEndpoint: "alias:4317"}.endpoint())
	})

	t.Run("NewResource", func(t *testing.T) {
		res, err := NewResource(ctx, cfg)
		assert.NoError(t, err)
		assert.NotNil(t, res)
	})

	t.Run("InitTelemetry success", func(t *testing.T) {
		// InitTelemetry replaces the process-wide default logger and the
		// global providers; put them back so later tests in this package do
		// not use shut-down providers.
		restoreGlobals(t)
		prev := slog.Default()

		shutdown, err := InitTelemetry(ctx, cfg)
		assert.NoError(t, err)
		assert.NotNil(t, shutdown)
		assert.NotSame(t, prev, slog.Default(), "InitLogs installs a new default logger")

		// Verify NewLogger works
		logger := NewLogger(ctx, cfg)
		assert.NotNil(t, logger)

		// Clean up telemetry providers.
		// It is acceptable for shutdown to return an error (like Unimplemented or connection closed)
		// because our dummy server does not implement the actual OTel collector services.
		err = shutdown(ctx)
		if err != nil {
			assert.True(t, strings.Contains(err.Error(), "unknown service") || strings.Contains(err.Error(), "Unimplemented") || strings.Contains(err.Error(), "connection"))
		}
	})
}
