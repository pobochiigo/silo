package telemetry

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

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
		AlloyEndpoint:  lis.Addr().String(),
	}

	ctx := context.Background()

	t.Run("NewResource", func(t *testing.T) {
		res, err := NewResource(ctx, cfg)
		assert.NoError(t, err)
		assert.NotNil(t, res)
	})

	t.Run("InitTelemetry success", func(t *testing.T) {
		shutdown, err := InitTelemetry(ctx, cfg)
		assert.NoError(t, err)
		assert.NotNil(t, shutdown)

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
