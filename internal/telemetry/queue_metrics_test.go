package telemetry_test

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestQueueMetricsRecorded drives the work-queue recorder methods through the
// manual reader and asserts each lands on the right Prometheus series, so the
// scheduler-queue dashboard panels have data behind them (S8).
func TestQueueMetricsRecorded(t *testing.T) {
	t.Parallel()

	rec, reader, _ := newTestRecorder(t)
	ctx := t.Context()

	rec.QueueItemRun(ctx, true)
	rec.QueueItemRun(ctx, false)
	rec.QueueDeadLettered(ctx)
	rec.QueueHeld(ctx)
	rec.QueueResumed(ctx)
	rec.QueueLeaseReclaimed(ctx, 2)
	rec.QueueDepth(ctx, 3, 1, 0, 2, 5)

	data := collect(t, reader)

	if got := sumIntFiltered(t, &data, "cairnos_queue_runs_total", "outcome", "ok"); got != 1 {
		t.Errorf("ok runs = %d, want 1", got)
	}

	if got := sumIntFiltered(t, &data, "cairnos_queue_runs_total", "outcome", "failed"); got != 1 {
		t.Errorf("failed runs = %d, want 1", got)
	}

	if got := sumInt(t, &data, "cairnos_queue_dead_lettered_total"); got != 1 {
		t.Errorf("dead-lettered = %d, want 1", got)
	}

	if got := sumInt(t, &data, "cairnos_queue_held_total"); got != 1 {
		t.Errorf("held = %d, want 1", got)
	}

	if got := sumInt(t, &data, "cairnos_queue_resumed_total"); got != 1 {
		t.Errorf("resumed = %d, want 1", got)
	}

	if got := sumInt(t, &data, "cairnos_queue_reclaimed_total"); got != 2 {
		t.Errorf("reclaimed = %d, want 2", got)
	}

	if got := floatGaugeWithAttr(t, &data, "cairnos_queue_depth", "state", "ready"); got != 3 {
		t.Errorf("ready depth = %v, want 3", got)
	}

	if got := floatGaugeWithAttr(t, &data, "cairnos_queue_depth", "state", "dead"); got != 2 {
		t.Errorf("dead depth = %v, want 2", got)
	}

	if got := floatGaugeWithAttr(t, &data, "cairnos_queue_oldest_age_seconds", "", ""); got != 5 {
		t.Errorf("oldest ready age = %v, want 5", got)
	}
}

// floatGaugeWithAttr reads one datapoint off a float64 gauge, optionally
// filtered to the point whose key attribute equals value. An empty key returns
// the first point.
func floatGaugeWithAttr(t *testing.T, data *metricdata.ResourceMetrics, name, key, value string) float64 {
	t.Helper()

	item, found := findMetric(data, name)
	if !found {
		t.Fatalf("metric %s was never observed", name)
	}

	gauge, isGauge := item.Data.(metricdata.Gauge[float64])
	if !isGauge {
		t.Fatalf("metric %s is %T, want Gauge[float64]", name, item.Data)
	}

	for _, point := range gauge.DataPoints {
		if key == "" {
			return point.Value
		}

		attr, present := point.Attributes.Value(attribute.Key(key))
		if present && attr.AsString() == value {
			return point.Value
		}
	}

	t.Fatalf("gauge %s has no datapoint with %s=%s", name, key, value)

	return 0
}
