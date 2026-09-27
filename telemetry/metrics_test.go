package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricsRecorder(t *testing.T) {
	// Create a metric reader to scrape metrics
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	defer func() {
		_ = mp.Shutdown(context.Background())
	}()

	meter := mp.Meter("test-meter")
	recorder := NewMetricsRecorder(meter, "my_subsystem")

	assert.Equal(t, meter, recorder.Meter())

	ctx := context.Background()
	start := time.Now().Add(-100 * time.Millisecond)

	// Observe a successful request
	recorder.Observe(ctx, "GetItem", start, nil, attribute.String("custom_key", "custom_val"))

	// Observe a failed request
	recorder.Observe(ctx, "GetItem", start, errors.New("some error"), attribute.String("custom_key", "custom_val"))

	// Collect metrics
	var rm metricdata.ResourceMetrics
	err := reader.Collect(ctx, &rm)
	assert.NoError(t, err)

	// Verify metrics registered
	assert.Len(t, rm.ScopeMetrics, 1)
	sm := rm.ScopeMetrics[0]
	assert.Len(t, sm.Metrics, 3)

	metricsMap := make(map[string]metricdata.Metrics)
	for _, m := range sm.Metrics {
		metricsMap[m.Name] = m
	}

	assert.Contains(t, metricsMap, "my_subsystem_requests_total")
	assert.Contains(t, metricsMap, "my_subsystem_errors_total")
	assert.Contains(t, metricsMap, "my_subsystem_request_duration_seconds")

	// Let's assert on total requests
	reqMetric := metricsMap["my_subsystem_requests_total"]
	sumData, ok := reqMetric.Data.(metricdata.Sum[int64])
	assert.True(t, ok)
	assert.Len(t, sumData.DataPoints, 1)
	assert.Equal(t, int64(2), sumData.DataPoints[0].Value)

	// Check labels on the datapoint
	attrs := sumData.DataPoints[0].Attributes
	hasMethod := false
	hasCustom := false
	for _, kv := range attrs.ToSlice() {
		if kv.Key == "method" && kv.Value.AsString() == "GetItem" {
			hasMethod = true
		}
		if kv.Key == "custom_key" && kv.Value.AsString() == "custom_val" {
			hasCustom = true
		}
	}
	assert.True(t, hasMethod, "method attribute not found")
	assert.True(t, hasCustom, "custom_key attribute not found")

	// Assert on errors
	errMetric := metricsMap["my_subsystem_errors_total"]
	errSumData, ok := errMetric.Data.(metricdata.Sum[int64])
	assert.True(t, ok)
	assert.Len(t, errSumData.DataPoints, 1)
	assert.Equal(t, int64(1), errSumData.DataPoints[0].Value)
	assert.Equal(t, "{request}", reqMetric.Unit)
	assert.Equal(t, "{request}", errMetric.Unit)

	// Latency: unit and second-scale buckets, so percentiles are meaningful.
	latency := metricsMap["my_subsystem_request_duration_seconds"]
	assert.Equal(t, "s", latency.Unit)
	hist, ok := latency.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, hist.DataPoints, 1)
	assert.Equal(t, DefaultLatencyBuckets, hist.DataPoints[0].Bounds)
	assert.Equal(t, uint64(2), hist.DataPoints[0].Count)
	// Both observations took ~100ms: they must land in the 0.1-0.25s bucket,
	// not in a catch-all first bucket.
	var bucketIdx int
	for i, count := range hist.DataPoints[0].BucketCounts {
		if count == 2 {
			bucketIdx = i
		}
	}
	assert.Equal(t, 5, bucketIdx, "100ms must fall in the (0.1, 0.25] bucket")
}
