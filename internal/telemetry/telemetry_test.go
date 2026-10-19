package telemetry_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/telemetry"
)

// loopbackEphemeral asks for any free loopback port, so parallel tests
// never collide on a fixed one.
const loopbackEphemeral = "127.0.0.1:0"

// testVersion stamps the service.version resource in tests.
const testVersion = "test"

// scrapeTimeout bounds the /metrics HTTP exchange.
const scrapeTimeout = 5 * time.Second

// TestSetupPaths covers the wiring matrix: disabled, enabled with no
// exporters (the otlp-unset path), and enabled with a Prometheus bind.
// Every variant must hand out a usable Recorder and shut down cleanly.
func TestSetupPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cfg      config.Telemetry
		wantProm bool
	}{
		{name: "disabled is a no-op stack", cfg: config.Telemetry{}, wantProm: false},
		{name: "enabled without exporters", cfg: config.Telemetry{Enabled: true}, wantProm: false},
		{name: "enabled with prometheus bind", cfg: config.Telemetry{Enabled: true, Prometheus: loopbackEphemeral}, wantProm: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tel, err := telemetry.Setup(t.Context(), tt.cfg, testVersion, nil)
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}

			if tel.Recorder() == nil {
				t.Fatal("Recorder() = nil, want a usable recorder on every path")
			}

			if gotProm := tel.PrometheusAddr() != ""; gotProm != tt.wantProm {
				t.Fatalf("PrometheusAddr() = %q, want serving=%t", tel.PrometheusAddr(), tt.wantProm)
			}

			if err := tel.Shutdown(context.WithoutCancel(t.Context())); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
		})
	}
}

// TestMetricsEndpointServesCatalogNames proves a recorded measurement
// reaches /metrics under its exact catalog name, with no exporter-added
// suffix mangling.
func TestMetricsEndpointServesCatalogNames(t *testing.T) {
	t.Parallel()

	// The scrape runs on an in-memory network because a loopback dial can
	// land on a port with stale connection tracking (see internal/nettest).
	network := nettest.NewNetwork()

	tel, err := telemetry.Setup(t.Context(),
		config.Telemetry{Enabled: true, Prometheus: loopbackEphemeral}, testVersion, network.Listen)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	defer func() {
		if shutdownErr := tel.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Errorf("Shutdown: %v", shutdownErr)
		}
	}()

	tel.Recorder().BreakerTransition(t.Context(), "mock", reliability.StateOpen)

	body := scrapeMetrics(t, network.Client(), tel.PrometheusAddr())

	if !strings.Contains(body, "cairnos_circuit_transitions_total") {
		t.Fatalf("/metrics is missing cairnos_circuit_transitions_total:\n%s", body)
	}

	if !strings.Contains(body, `to_state="open"`) {
		t.Fatalf("/metrics is missing the to_state label:\n%s", body)
	}
}

// TestLogHandlerStampsTraceIDs proves records logged from a span context
// carry trace_id and span_id, and records outside one stay untouched.
func TestLogHandlerStampsTraceIDs(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	ctx, span := provider.Tracer(testVersion).Start(t.Context(), "demo")
	defer span.End()

	var buf bytes.Buffer

	logger := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil)))

	logger.InfoContext(ctx, "inside span")

	wantTrace := span.SpanContext().TraceID().String()
	if !strings.Contains(buf.String(), wantTrace) {
		t.Fatalf("log record %q is missing trace_id %s", buf.String(), wantTrace)
	}

	if !strings.Contains(buf.String(), "span_id") {
		t.Fatalf("log record %q is missing span_id", buf.String())
	}

	buf.Reset()
	logger.InfoContext(t.Context(), "outside span")

	if strings.Contains(buf.String(), "trace_id") {
		t.Fatalf("log record outside a span gained trace_id: %q", buf.String())
	}
}

// scrapeMetrics fetches the Prometheus text exposition from addr.
func scrapeMetrics(t *testing.T, client *http.Client, addr string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), scrapeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", http.NoBody)
	if err != nil {
		t.Fatalf("build scrape request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("scrape /metrics: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}

	return string(body)
}
