// Package telemetry wires the OpenTelemetry SDK behind the engine and
// agent-loop instrumentation seams: one tracer provider and one meter provider
// feed a Recorder, a Prometheus exporter serves /metrics, and an OTLP gRPC
// exporter pushes traces and metrics to a collector when one is configured.
// Disabled telemetry still hands out a working Recorder over no-op providers,
// so wiring code never branches.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"

	"github.com/chadit/CairnOS/internal/config"
)

// serviceName labels every exported span and metric.
const serviceName = "cairnos"

// scopeName is the instrumentation scope for the tracer and meter.
const scopeName = "github.com/chadit/CairnOS/internal/telemetry"

// metricsReadHeaderTimeout bounds slow-header clients on the scrape
// listener, matching the API server's stance.
const metricsReadHeaderTimeout = 10 * time.Second

// ListenFunc binds a listener, in the shape of net.ListenConfig.Listen.
type ListenFunc func(ctx context.Context, network, address string) (net.Listener, error)

// Telemetry owns the configured providers and exporters. Shutdown flushes
// and releases everything; a disabled instance shuts down as a no-op.
type Telemetry struct {
	recorder       *Recorder
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	promServer     *http.Server
	promAddr       string
}

// Setup builds the telemetry stack from config. Disabled config returns a
// no-op-backed instance, so callers wire the Recorder unconditionally. OTLP
// adds gRPC push; a Prometheus bind adds /metrics, bound through listen (nil
// for a host socket). With neither, spans only thread IDs into logs, which is
// what the one-shot run command wants.
func Setup(ctx context.Context, cfg config.Telemetry, version string, listen ListenFunc) (*Telemetry, error) {
	if !cfg.Enabled {
		recorder, err := NewRecorder(
			nooptrace.NewTracerProvider().Tracer(scopeName),
			noopmetric.NewMeterProvider().Meter(scopeName),
		)
		if err != nil {
			return nil, err
		}

		return &Telemetry{recorder: recorder}, nil
	}

	res, err := buildResource(version)
	if err != nil {
		return nil, err
	}

	tracerProvider, err := newTracerProvider(ctx, cfg.OTLPEndpoint, res)
	if err != nil {
		return nil, err
	}

	tel := &Telemetry{tracerProvider: tracerProvider}

	if err := tel.startMeters(ctx, cfg, res, listen); err != nil {
		// Construction failed midway. The setup error is the one the caller
		// acts on, so a cleanup failure is logged rather than returned over it.
		if shutdownErr := tel.Shutdown(ctx); shutdownErr != nil {
			slog.WarnContext(ctx, "shutdown after failed telemetry setup", "error", shutdownErr)
		}

		return nil, err
	}

	return tel, nil
}

// PrometheusAddr reports the bound /metrics address, empty when the scrape
// endpoint is not serving. The bind in config may ask for port zero, so
// callers read the real port here.
func (t *Telemetry) PrometheusAddr() string {
	return t.promAddr
}

// Recorder returns the instrumentation implementation to wire into
// engine.Config.Instrumentation and agentloop.New.
func (t *Telemetry) Recorder() *Recorder {
	return t.recorder
}

// Shutdown drains the scrape endpoint and flushes both providers, bounded
// by ctx. Safe on partially constructed instances; nil members skip.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error

	if t.promServer != nil {
		if err := t.promServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown metrics endpoint: %w", err))
		}
	}

	if t.meterProvider != nil {
		if err := t.meterProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown meter provider: %w", err))
		}
	}

	if t.tracerProvider != nil {
		if err := t.tracerProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown tracer provider: %w", err))
		}
	}

	if joined := errors.Join(errs...); joined != nil {
		return fmt.Errorf("shutdown telemetry: %w", joined)
	}

	return nil
}

// startMeters assembles the meter provider from the configured exporters
// and builds the Recorder over it.
func (t *Telemetry) startMeters(ctx context.Context, cfg config.Telemetry, res *resource.Resource, listen ListenFunc) error {
	options := []sdkmetric.Option{sdkmetric.WithResource(res)}

	if cfg.Prometheus != "" {
		promExporter, err := t.startPrometheus(ctx, cfg.Prometheus, listen)
		if err != nil {
			return err
		}

		options = append(options, sdkmetric.WithReader(promExporter))
	}

	if cfg.OTLPEndpoint != "" {
		exporter, err := otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlpmetricgrpc.WithInsecure())
		if err != nil {
			return fmt.Errorf("build otlp metric exporter: %w", err)
		}

		options = append(options, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	}

	t.meterProvider = sdkmetric.NewMeterProvider(options...)

	recorder, err := NewRecorder(t.tracerProvider.Tracer(scopeName), t.meterProvider.Meter(scopeName))
	if err != nil {
		return err
	}

	t.recorder = recorder

	return nil
}

// startPrometheus binds the scrape listener and serves /metrics from its
// own HTTP server rather than the API gateway mux: the endpoint then works
// when the API is disabled (and in embedders without the gateway), and the
// gateway's loopback-only guard never constrains where an operator points
// Prometheus; the metrics bind is the operator's own choice.
func (t *Telemetry) startPrometheus(ctx context.Context, bind string, listen ListenFunc) (*otelprom.Exporter, error) {
	registry := prometheus.NewRegistry()

	exporter, err := otelprom.New(
		otelprom.WithRegisterer(registry),
		// The catalog names carry their own _total and _seconds suffixes,
		// so translation must escape characters only, never append.
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithoutSuffixes),
	)
	if err != nil {
		return nil, fmt.Errorf("build prometheus exporter: %w", err)
	}

	if listen == nil {
		listen = (&net.ListenConfig{}).Listen
	}

	listener, err := listen(ctx, "tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("listen %s for /metrics: %w", bind, err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	t.promServer = &http.Server{Handler: mux, ReadHeaderTimeout: metricsReadHeaderTimeout}
	t.promAddr = listener.Addr().String()

	go func() {
		if serveErr := t.promServer.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			slog.Error("metrics endpoint stopped", "addr", t.promAddr, "error", serveErr)
		}
	}()

	return exporter, nil
}

// buildResource names the service for every exported span and metric.
func buildResource(version string) (*resource.Resource, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("service.version", version),
	))
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}

	return res, nil
}

// newTracerProvider builds the tracer provider. Without an OTLP endpoint
// spans export nowhere, deliberately: they still flow through contexts, so
// trace IDs reach logs via the slog bridge and the span tree stays
// testable, at zero exporter cost. The OTLP connection is plain text by
// contract: the endpoint is a local or sidecar collector, and Phase 4a
// owns transport hardening.
func newTracerProvider(ctx context.Context, otlpEndpoint string, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	options := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}

	if otlpEndpoint != "" {
		exporter, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(otlpEndpoint),
			otlptracegrpc.WithInsecure())
		if err != nil {
			return nil, fmt.Errorf("build otlp trace exporter: %w", err)
		}

		options = append(options, sdktrace.WithBatcher(exporter))
	}

	return sdktrace.NewTracerProvider(options...), nil
}
