// Package demo holds the few helpers the examples share: environment
// defaults, a signal-aware context and the telemetry bootstrap.
package demo

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pobochiigo/silo/telemetry"
)

// Env returns the value of the environment variable key, or def when unset.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Context returns a context that is cancelled by SIGINT or SIGTERM.
func Context() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Telemetry bootstraps Silo's telemetry when OTEL_EXPORTER_OTLP_ENDPOINT is
// set (docker-compose.yml starts a collector on localhost:4317). Otherwise it
// installs a plain text slog handler, a tracer provider that prints finished
// spans and an in-process metric reader, so every example runs without any
// infrastructure. verbose enables Debug, the level of the generated logging
// middlewares' "<Method> started" lines. The returned function flushes and
// shuts the pipelines down; call it before exiting.
func Telemetry(ctx context.Context, service string, verbose bool) func() {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		slog.Info("no collector configured: spans are printed, metrics are read in-process; set OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 and run docker compose up to export instead")
		return localProviders()
	}

	shutdown, err := telemetry.InitTelemetry(ctx, telemetry.Config{
		ServiceName:      service,
		ServiceVersion:   "dev",
		Environment:      "example",
		Endpoint:         endpoint,
		Insecure:         true, // the compose collector speaks plaintext gRPC
		TraceSampleRatio: 1,
		MetricInterval:   5 * time.Second,
		BatchTimeout:     time.Second,
		LocalLogLevel:    level,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "telemetry:", err)
		os.Exit(1)
	}
	slog.Info("telemetry export enabled", slog.String("endpoint", endpoint))
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			slog.Error("telemetry shutdown", slog.Any("error", err))
		}
	}
}

// Step prints a numbered heading so the output of an example reads as a story.
func Step(n int, title string) {
	fmt.Printf("\n== %d. %s\n", n, title)
}

// Fail prints err and exits; examples are not servers, a failed step ends them.
func Fail(step string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", step, err)
	os.Exit(1)
}
