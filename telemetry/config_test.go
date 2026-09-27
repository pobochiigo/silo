package telemetry

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

func TestConfigExporterTarget(t *testing.T) {
	testCases := []struct {
		name      string
		cfg       Config
		target    string
		plaintext bool
		wantErr   string
	}{
		{name: "empty leaves the exporters to the environment", cfg: Config{}, target: "", plaintext: false},
		{name: "empty keeps Insecure", cfg: Config{Insecure: true}, target: "", plaintext: true},
		{name: "host:port passthrough", cfg: Config{Endpoint: "alloy:4317"}, target: "alloy:4317", plaintext: false},
		{name: "host:port with Insecure", cfg: Config{Endpoint: "alloy:4317", Insecure: true}, target: "alloy:4317", plaintext: true},
		{name: "deprecated alias", cfg: Config{AlloyEndpoint: "old:4317"}, target: "old:4317"},
		{name: "http URL is plaintext", cfg: Config{Endpoint: "http://localhost:4317"}, target: "localhost:4317", plaintext: true},
		{name: "https URL is TLS even with Insecure", cfg: Config{Endpoint: "https://collector.example.com:443", Insecure: true}, target: "collector.example.com:443"},
		{name: "URL without port gets the OTLP default", cfg: Config{Endpoint: "https://collector.example.com"}, target: "collector.example.com:4317"},
		{name: "IPv6 URL without port", cfg: Config{Endpoint: "http://[::1]"}, target: "[::1]:4317", plaintext: true},
		{name: "unsupported scheme", cfg: Config{Endpoint: "grpc://collector:4317"}, wantErr: "unsupported scheme"},
		{name: "URL without host", cfg: Config{Endpoint: "https://"}, wantErr: "has no host"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			target, plaintext, err := tc.cfg.exporterTarget()
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.target, target)
			assert.Equal(t, tc.plaintext, plaintext)
		})
	}
}

func TestConfigValidate(t *testing.T) {
	assert.NoError(t, Config{}.validate())
	assert.NoError(t, Config{TraceSampleRatio: 1, Compression: "gzip", MaxExportBatchSize: 10, BatchTimeout: time.Second, MetricInterval: time.Minute}.validate())
	assert.ErrorContains(t, Config{TraceSampleRatio: 1.5}.validate(), "TraceSampleRatio")
	assert.ErrorContains(t, Config{TraceSampleRatio: -0.1}.validate(), "TraceSampleRatio")
	assert.ErrorContains(t, Config{Compression: "zstd"}.validate(), "Compression")
	assert.ErrorContains(t, Config{BatchTimeout: -time.Second}.validate(), "negative")
	assert.ErrorContains(t, Config{Endpoint: "ftp://x"}.validate(), "unsupported scheme")

	// Invalid configuration is rejected before any exporter is built.
	_, err := InitTelemetry(context.Background(), Config{TraceSampleRatio: 2})
	assert.ErrorContains(t, err, "TraceSampleRatio")
	res, err := NewResource(context.Background(), Config{})
	require.NoError(t, err)
	_, err = InitTraces(context.Background(), Config{Compression: "lz4"}, res)
	assert.ErrorContains(t, err, "Compression")
}

func TestConfigDefaults(t *testing.T) {
	var cfg Config
	assert.Equal(t, DefaultMaxExportBatchSize, cfg.maxExportBatchSize())
	assert.Equal(t, DefaultBatchTimeout, cfg.batchTimeout())
	assert.Equal(t, DefaultMetricInterval, cfg.metricInterval())

	cfg = Config{MaxExportBatchSize: 64, BatchTimeout: time.Second, MetricInterval: 10 * time.Second}
	assert.Equal(t, 64, cfg.maxExportBatchSize())
	assert.Equal(t, time.Second, cfg.batchTimeout())
	assert.Equal(t, 10*time.Second, cfg.metricInterval())
}

func TestDialCollector(t *testing.T) {
	conn, err := dialCollector(Config{})
	require.NoError(t, err)
	assert.Nil(t, conn, "without an Endpoint every exporter dials on its own")

	conn, err = dialCollector(Config{
		Endpoint:    "https://collector.example.com",
		TLSConfig:   &tls.Config{MinVersion: tls.VersionTLS13},
		DialOptions: []grpc.DialOption{grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second})},
	})
	require.NoError(t, err)
	require.NotNil(t, conn, "an Endpoint yields the shared connection")
	assert.Equal(t, "dns:///collector.example.com:4317", conn.CanonicalTarget())
	assert.NoError(t, conn.Close())

	_, err = dialCollector(Config{Endpoint: "bad scheme://x"})
	assert.Error(t, err)
}

func TestExporterOptions(t *testing.T) {
	count := func(cfg Config, conn *grpc.ClientConn) int {
		t.Helper()
		opts, err := exporterOptions(cfg, conn, traceOptionSet)
		require.NoError(t, err)
		return len(opts)
	}

	assert.Equal(t, 0, count(Config{}, nil), "environment-driven: nothing overrides the OTLP variables")
	assert.Equal(t, 1, count(Config{Insecure: true}, nil), "Insecure still applies to an env-driven endpoint")
	assert.Equal(t, 2, count(Config{Endpoint: "http://c:4317"}, nil), "endpoint + insecure")
	assert.Equal(t, 2, count(Config{Endpoint: "c:4317", TLSConfig: &tls.Config{}}, nil), "endpoint + TLS credentials")
	assert.Equal(t, 1, count(Config{Endpoint: "c:4317"}, nil), "endpoint with system roots")
	assert.Equal(t, 3, count(Config{Endpoint: "c:4317", Compression: "gzip", Headers: map[string]string{"authorization": "x"}}, nil))

	conn, err := grpc.NewClient("dns:///c:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, 1, count(Config{Endpoint: "https://ignored", TLSConfig: &tls.Config{}}, conn), "a shared connection replaces endpoint and TLS options")
	assert.Equal(t, 3, count(Config{Compression: "gzip", Headers: map[string]string{"authorization": "x"}}, conn), "compression and headers still apply per call")
}

func TestInitLogs_RegistersGlobalProviderWithoutTouchingSlog(t *testing.T) {
	prev := global.GetLoggerProvider()
	t.Cleanup(func() { global.SetLoggerProvider(prev) })

	res, err := NewResource(context.Background(), Config{ServiceName: "svc"})
	require.NoError(t, err)

	// The exporter connects lazily, so an unreachable endpoint is fine here.
	shutdown, err := InitLogs(context.Background(), Config{Endpoint: "127.0.0.1:1", Insecure: true, SkipSlogDefault: true}, res)
	require.NoError(t, err)

	_, isSDK := global.GetLoggerProvider().(*sdklog.LoggerProvider)
	assert.True(t, isSDK, "custom handlers must be able to reach the OTel log pipeline")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

func TestNewResource_HostDetection(t *testing.T) {
	ctx := context.Background()
	attrsOf := func(cfg Config) map[string]bool {
		res, err := NewResource(ctx, cfg)
		require.NoError(t, err)
		out := map[string]bool{}
		for _, kv := range res.Attributes() {
			out[string(kv.Key)] = true
		}
		return out
	}

	assert.True(t, attrsOf(Config{})["host.name"], "host.name is detected by default")
	assert.False(t, attrsOf(Config{SkipHostDetection: true})["host.name"])

	// Environment attributes win over detection.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "host.name=from-env")
	res, err := NewResource(ctx, Config{})
	require.NoError(t, err)
	for _, kv := range res.Attributes() {
		if kv.Key == "host.name" {
			assert.Equal(t, "from-env", kv.Value.AsString())
		}
	}
}
