package cli_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cli"
)

// serveReadyMarker is the last line serve prints before blocking, giving
// the smoke check an output signal to wait on instead of a clock.
const serveReadyMarker = "serving; interrupt to stop"

// serveConfigTOML binds every listener (API and the telemetry scrape
// endpoint) to ephemeral loopback ports so parallel test runs never
// collide.
const serveConfigTOML = `
[api]
grpc = "127.0.0.1:0"
http = "127.0.0.1:0"

[telemetry]
enabled    = true
prometheus = "127.0.0.1:0"
`

// serveHelloTOML is the workflow the smoke test fires over gRPC so the
// metric catalog has a run to count.
const serveHelloTOML = `
[workflow]
name = "hello"

[action.greet]
runner = "mock"
params = { output = "hello" }
`

// grpcAddrPattern pulls the bound gRPC address out of serve's output.
var grpcAddrPattern = regexp.MustCompile(`gRPC listening on (\S+)`)

// metricsAddrPattern pulls the bound /metrics address out of serve's
// output.
var metricsAddrPattern = regexp.MustCompile(`metrics listening on (\S+)`)

// signalWriter buffers output and closes ready once the marker appears, so
// the smoke test waits on the server's own readiness line.
type signalWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	ready  chan struct{}
	closed bool
}

func newSignalWriter() *signalWriter {
	return &signalWriter{ready: make(chan struct{})}
}

func (w *signalWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	written, err := w.buf.Write(data)

	if !w.closed && strings.Contains(w.buf.String(), serveReadyMarker) {
		w.closed = true
		close(w.ready)
	}

	if err != nil {
		return written, err //nolint:wrapcheck // io.Writer contract: pass the sink's error through untouched
	}

	return written, nil
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buf.String()
}

// TestServeSmoke boots the full serve command on ephemeral ports, proves
// Health answers over gRPC, fires one workflow run, scrapes /metrics for a
// catalog name, and interrupts it; a clean exit code closes the loop.
// Every wait is event-driven: the ready marker, the run stream's EOF, and
// the exit channel.
func TestServeSmoke(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(serveConfigTOML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	workflowsDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o750); err != nil {
		t.Fatalf("create workflows dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workflowsDir, "hello.toml"), []byte(serveHelloTOML), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stdout := newSignalWriter()

	var stderr bytes.Buffer

	exit := make(chan int, 1)

	go func() {
		exit <- cli.New(stdout, &stderr).Run(ctx, []string{"serve", flagDirArg, dir})
	}()

	select {
	case <-stdout.ready:
	case code := <-exit:
		t.Fatalf("serve exited early with %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	grpcAddr := outputAddr(t, stdout, grpcAddrPattern, "gRPC")
	metricsAddr := outputAddr(t, stdout, metricsAddrPattern, "metrics")

	checkHealth(t, grpcAddr)
	fireRun(t, grpcAddr)
	checkMetrics(t, metricsAddr)
	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit code = %d, want %d\nstderr: %s", code, cli.ExitOK, stderr.String())
	}
}

// outputAddr extracts one bound address from serve's startup output.
func outputAddr(t *testing.T, stdout *signalWriter, pattern *regexp.Regexp, label string) string {
	t.Helper()

	match := pattern.FindStringSubmatch(stdout.String())
	if match == nil {
		t.Fatalf("serve output %q never printed the %s address", stdout.String(), label)
	}

	return match[1]
}

// checkHealth calls AdminService.Health over a fresh gRPC connection.
func checkHealth(t *testing.T, addr string) {
	t.Helper()

	conn := dialServe(t, addr)

	health, err := cairnosv1.NewAdminServiceClient(conn).Health(t.Context(), &cairnosv1.HealthRequest{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	if health.GetStatus() != "ok" {
		t.Fatalf("health status = %q, want ok", health.GetStatus())
	}
}

// fireRun starts the hello workflow over gRPC and drains its event stream
// to completion, so the run is terminal before the metrics scrape.
func fireRun(t *testing.T, addr string) {
	t.Helper()

	conn := dialServe(t, addr)

	stream, err := cairnosv1.NewWorkflowServiceClient(conn).RunWorkflow(t.Context(),
		&cairnosv1.RunWorkflowRequest{WorkflowName: "hello"})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	for {
		_, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return
		}

		if recvErr != nil {
			t.Fatalf("recv run event: %v", recvErr)
		}
	}
}

// checkMetrics scrapes /metrics and asserts the run that just finished
// shows up under its catalog name.
func checkMetrics(t *testing.T, addr string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", http.NoBody)
	if err != nil {
		t.Fatalf("build scrape request: %v", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("scrape /metrics: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}

	if !strings.Contains(string(body), "cairnos_runs_total") {
		t.Fatalf("/metrics is missing cairnos_runs_total after a run:\n%s", body)
	}
}

// dialServe opens a gRPC client connection cleaned up with the test.
func dialServe(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// TestServeRefusesNetworkBind proves the network guard reaches the CLI: a
// non-loopback bind with auth off fails fast with a failure exit code and
// names the flag that would allow it deliberately.
func TestServeRefusesNetworkBind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Telemetry stays off so this fixture never binds the default :9090
	// scrape port, which would collide across parallel test runs.
	wideOpen := "[api]\ngrpc = \"0.0.0.0:0\"\nhttp = \"127.0.0.1:0\"\n\n[telemetry]\nenabled = false\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(wideOpen), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr bytes.Buffer

	code := cli.New(&stdout, &stderr).Run(t.Context(), []string{"serve", flagDirArg, dir})
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d, want %d for a refused network bind", code, cli.ExitFailure)
	}

	if !strings.Contains(stderr.String(), "serve_without_auth_on_network") {
		t.Fatalf("stderr %q does not name the explicit open-API flag", stderr.String())
	}
}
