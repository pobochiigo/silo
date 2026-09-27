// Command middlegen runs the generated decorators of the inventory domain
// without any infrastructure: an in-memory repository, a transactor that
// prints BEGIN/COMMIT/ROLLBACK, spans printed one per line and metrics read
// back in-process. Watch the order of the lines to see when deferred writes
// really execute.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/pobochiigo/silo/uow"

	"github.com/pobochiigo/silo/examples/internal/demo"
	"github.com/pobochiigo/silo/examples/middlegen/inventory"
	"github.com/pobochiigo/silo/examples/middlegen/inventory/svcmw"
)

func main() {
	ctx := context.Background()

	// Observability, all in-process: Debug logging shows the generated
	// "<Method> started" lines, every finished span is printed, and metrics
	// are collected on demand by a manual reader.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(spanPrinter{})))
	otel.SetTracerProvider(tp)
	defer func() { _ = tp.Shutdown(ctx) }()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	// Wiring: decorators are applied inside out, the UoW middleware closest
	// to the implementation. The transactor only prints; there is no database.
	raw := inventory.NewMemory(os.Stdout)
	repo := inventory.RepositoryUoWMiddleware()(raw)
	repo = inventory.RepositoryMetricsMiddleware()(repo)
	repo = inventory.RepositoryTracingMiddleware()(repo)
	repo = inventory.RepositoryLoggingMiddleware()(repo)

	manager := uow.NewManager(printingTransactor{})
	svc := svcmw.ServiceUoWMiddleware(manager)(inventory.NewService(repo))
	svc = svcmw.ServiceTracingMiddleware()(svc)
	svc = svcmw.ServiceLoggingMiddleware()(svc)

	demo.Step(1, "RunWith: Restock reads at once, Save is queued and runs after the method returns, between BEGIN and COMMIT")
	item, err := svc.Restock(ctx, "widget", 10)
	if err != nil {
		demo.Fail("restock", err)
	}
	fmt.Printf("   Restock returned the item it saved: %+v\n", *item)

	demo.Step(2, "RunInTx (//middlegen:in-tx): BEGIN comes first, Get runs inside the transaction, the queued Save runs before COMMIT")
	if err := svc.Reserve(ctx, "widget", 3); err != nil {
		demo.Fail("reserve", err)
	}

	demo.Step(3, "Redaction: the key is logged as [REDACTED]")
	if err := svc.Rotate(ctx, "widget", "s3cr3t-key"); err != nil {
		demo.Fail("rotate", err)
	}

	demo.Step(4, "Echo rules while queued: Merge returns the parameter named by the directive, Archive returns nil, Decrement returns zero values")
	err = manager.RunWith(ctx, func(ctx context.Context) error {
		if _, err := repo.Save(ctx, &inventory.Item{SKU: "gadget", Name: "gadget", Quantity: 5}); err != nil {
			return err
		}
		if _, err := repo.Save(ctx, &inventory.Item{SKU: "old-gadget", Name: "gadget (old)", Quantity: 2}); err != nil {
			return err
		}
		from, into := &inventory.Item{SKU: "old-gadget", Quantity: 2}, &inventory.Item{SKU: "gadget", Quantity: 5}
		merged, err := repo.Merge(ctx, from, into)
		if err != nil {
			return err
		}
		fmt.Printf("   Merge returned into (%s), merged quantity not known yet: %d\n", merged.SKU, merged.Quantity)
		archived, err := repo.Archive(ctx, &inventory.Item{SKU: "gadget"})
		if err != nil {
			return err
		}
		fmt.Printf("   Archive returned %v\n", archived)
		left, err := repo.Decrement(ctx, "widget", 1)
		if err != nil {
			return err
		}
		fmt.Printf("   Decrement returned %d; the real quantity is printed by the repository at commit\n", left)
		return nil
	})
	if err != nil {
		demo.Fail("run with", err)
	}

	demo.Step(5, "Failure inside RunInTx rolls back; the logging middleware reports it at Error level")
	err = svc.Reserve(ctx, "widget", 1000)
	fmt.Printf("   Reserve returned: %v\n", err)
	if !errors.Is(err, inventory.ErrInsufficientStock) {
		demo.Fail("expected ErrInsufficientStock", err)
	}

	demo.Step(6, "Methods without a context (io.Closer) are logged and measured, never traced or deferred")
	if err := repo.Close(); err != nil {
		demo.Fail("close", err)
	}

	demo.Step(7, "Metrics recorded by the generated metrics middleware")
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		demo.Fail("collect metrics", err)
	}
	printMetrics(rm)
}

// printingTransactor stands in for a database: it prints the transaction
// lifecycle so the output shows when the unit of work opens and commits.
type printingTransactor struct{}

func (printingTransactor) BeginTx(ctx context.Context) (uow.Tx, context.Context, error) {
	fmt.Println("   [tx] BEGIN")
	return printingTx{}, ctx, nil
}

type printingTx struct{}

func (printingTx) Commit(context.Context) error {
	fmt.Println("   [tx] COMMIT")
	return nil
}

func (printingTx) Rollback(context.Context) error {
	fmt.Println("   [tx] ROLLBACK")
	return nil
}

// spanPrinter prints one line per finished span.
type spanPrinter struct{}

func (spanPrinter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, s := range spans {
		status := "ok"
		if s.Status().Code != 0 {
			status = s.Status().Code.String() + ": " + s.Status().Description
		}
		fmt.Printf("   [span] %-22s %8s  %s\n", s.Name(), s.EndTime().Sub(s.StartTime()).Round(time.Microsecond), status)
	}
	return nil
}

func (spanPrinter) Shutdown(context.Context) error { return nil }

// printMetrics prints counters and histogram counts with their attributes.
func printMetrics(rm metricdata.ResourceMetrics) {
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					fmt.Printf("   %s{%s} = %d\n", m.Name, dp.Attributes.Encoded(attribute.DefaultEncoder()), dp.Value)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					fmt.Printf("   %s{%s} count=%d sum=%.6fs\n", m.Name, dp.Attributes.Encoded(attribute.DefaultEncoder()), dp.Count, dp.Sum)
				}
			}
		}
	}
}
