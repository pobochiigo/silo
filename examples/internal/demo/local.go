package demo

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/pobochiigo/silo/telemetry"
)

// localReader collects metrics in-process when no collector is configured.
var localReader *sdkmetric.ManualReader

// localProviders installs a tracer provider that prints one line per finished
// span and a meter provider read by PrintMetrics, so the examples show real
// span contexts and metrics without a collector.
func localProviders() func() {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(spanPrinter{})))
	otel.SetTracerProvider(tp)
	telemetry.InitPropagators()

	localReader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(localReader))
	otel.SetMeterProvider(mp)

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
	}
}

// spanPrinter prints one line per finished span.
type spanPrinter struct{}

func (spanPrinter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, s := range spans {
		status := s.Status().Code.String()
		if s.Status().Description != "" {
			status += ": " + s.Status().Description
		}
		fmt.Printf("   [span] %-26s trace=%s %8s  %s\n", s.Name(), s.SpanContext().TraceID(),
			s.EndTime().Sub(s.StartTime()).Round(time.Microsecond), status)
	}
	return nil
}

func (spanPrinter) Shutdown(context.Context) error { return nil }

// PrintMetrics prints the counters and histograms recorded so far. Without a
// collector they come from the in-process reader; with one they are on their
// way to the collector and nothing is printed.
func PrintMetrics(ctx context.Context) {
	if localReader == nil {
		fmt.Println("   metrics are exported to the collector; see its logs")
		return
	}
	var rm metricdata.ResourceMetrics
	if err := localReader.Collect(ctx, &rm); err != nil {
		fmt.Println("   collect metrics:", err)
		return
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					fmt.Printf("   %s{%s} = %d\n", m.Name, dp.Attributes.Encoded(attribute.DefaultEncoder()), dp.Value)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					fmt.Printf("   %s{%s} count=%d\n", m.Name, dp.Attributes.Encoded(attribute.DefaultEncoder()), dp.Count)
				}
			}
		}
	}
}
