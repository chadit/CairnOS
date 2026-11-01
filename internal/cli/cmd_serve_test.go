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

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/nettest"
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
	ready  chan struct{}
	buf    bytes.Buffer
	mu     sync.Mutex
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
		return written, err
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

	exit := make(chan int, oneShotChanSize)
	network := nettest.NewNetwork()

	go func() {
		exit <- cli.New(stdout, &stderr).WithServeTransport(network).Run(ctx, []string{serveCmd, flagDirArg, dir})
	}()

	select {
	case <-stdout.ready:
	case code := <-exit:
		t.Fatalf("serve exited early with %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	grpcAddr := outputAddr(t, stdout, grpcAddrPattern, "gRPC")
	metricsAddr := outputAddr(t, stdout, metricsAddrPattern, "metrics")

	checkHealth(t, network.DialGRPC(t, grpcAddr))
	fireRun(t, network.DialGRPC(t, grpcAddr))
	checkMetrics(t, network.Client(), metricsAddr)
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

// checkHealth calls AdminService.Health over conn.
func checkHealth(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()

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
func fireRun(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()

	stream, err := cairnosv1.NewWorkflowServiceClient(conn).RunWorkflow(t.Context(),
		&cairnosv1.RunWorkflowRequest{WorkflowName: helloWorkflowName})
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
func checkMetrics(t *testing.T, client *http.Client, addr string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}

	if !strings.Contains(string(body), "cairnos_runs_total") {
		t.Fatalf("/metrics is missing cairnos_runs_total after a run:\n%s", body)
	}
}

// bridgeTokenEnv is the env key the Telegram bot token rides, mirrored from the
// wiring's unexported const so the tests can set and clear it.
const bridgeTokenEnv = "CAIRNOS_BRIDGE_TELEGRAM_TOKEN" // #nosec G101 -- env var name, not a credential

// bridgeEnabledWithChatTOML turns the bridge and chat on with one allowlisted
// user, the wiring the running banner proves. The short poll timeout bounds the
// one Bot API call the loop attempts before the test cancels it.
const bridgeEnabledWithChatTOML = `
[api]
grpc = "127.0.0.1:0"
http = "127.0.0.1:0"

[telemetry]
enabled    = true
prometheus = "127.0.0.1:0"

[chat]
enabled = true
driver  = "openai"

[bridge.telegram]
enabled      = true
poll_timeout = "1s"

[bridge.telegram.users.123456789]
workspace = "default"
`

// bridgeEnabledNoChatTOML turns the bridge on while chat stays off, the
// cross-check serve must reject at wiring once the token is set.
const bridgeEnabledNoChatTOML = `
[api]
grpc = "127.0.0.1:0"
http = "127.0.0.1:0"

[telemetry]
enabled = false

[bridge.telegram]
enabled = true

[bridge.telegram.users.123456789]
workspace = "default"
`

// bridgeEnabledNoUsersTOML turns the bridge on with an empty allowlist, which
// config load rejects before serve wiring is reached.
const bridgeEnabledNoUsersTOML = `
[api]
grpc = "127.0.0.1:0"
http = "127.0.0.1:0"

[telemetry]
enabled = false

[bridge.telegram]
enabled = true
`

// bridgeServeDir writes cfgTOML into a fresh home with a workflows directory, so
// serve boots against a real layout.
func bridgeServeDir(t *testing.T, cfgTOML string) string {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfgTOML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o750); err != nil {
		t.Fatalf("create workflows dir: %v", err)
	}

	return dir
}

// serveUntilReady boots serve on a network of its own, for a case that never
// dials it.
func serveUntilReady(
	t *testing.T, dir string, env map[string]string,
) (*signalWriter, *bytes.Buffer, chan int, context.CancelFunc) {
	t.Helper()

	return serveUntilReadyOn(t, nettest.NewNetwork(), dir, env)
}

// serveUntilReadyOn boots serve in dir on a goroutine and blocks until the
// ready marker prints or serve exits early. The returned buffers are safe to
// read once exit is drained, since the exit send happens-after every serve
// write. Its listeners sit on network, and every bridge talks to
// silentBridgeHost.
func serveUntilReadyOn(
	t *testing.T, network *nettest.Network, dir string, env map[string]string,
) (*signalWriter, *bytes.Buffer, chan int, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	stdout := newSignalWriter()
	stderr := &bytes.Buffer{}
	exit := make(chan int, oneShotChanSize)

	go func() {
		app := cli.New(stdout, stderr).WithEnv(environ.Map(env)).
			WithBridgeTransport(silentBridgeHost()).
			WithServeTransport(network)
		exit <- app.Run(ctx, []string{serveCmd, flagDirArg, dir})
	}()

	select {
	case <-stdout.ready:
	case code := <-exit:
		cancel()
		t.Fatalf("serve exited early with %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	return stdout, stderr, exit, cancel
}

// silentBridgeHost stands in for every host a serve case's bridges call. It
// holds each request until the caller gives up and opens no socket. A default
// transport would resolve mm.example.test, which takes seconds to fail and
// outlives serve, or poll the real Telegram API and keep that connection pooled.
func silentBridgeHost() http.RoundTripper {
	return nettest.Transport(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
}

// TestServeStartsBridgeWhenEnabled proves the bridge wiring end to end: enabled
// with a token and chat prints the running banner, and serve exits cleanly on
// interrupt after draining the poll loop (acceptance 5, the started case).
func TestServeStartsBridgeWhenEnabled(t *testing.T) {
	t.Parallel()

	dir := bridgeServeDir(t, bridgeEnabledWithChatTOML)

	stdout, _, exit, cancel := serveUntilReady(t, dir, tokenEnv(bridgeTokenEnv, fakeBridgeToken))

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d", code, cli.ExitOK)
	}

	if !strings.Contains(stdout.String(), "telegram bridge running") {
		t.Fatalf("stdout missing the bridge banner:\n%s", stdout.String())
	}
}

// TestServeSkipsBridgeWhenDisabled proves the default posture: no bridge block
// starts no goroutine and logs one clear line, while serve runs normally.
func TestServeSkipsBridgeWhenDisabled(t *testing.T) {
	t.Parallel()

	dir := bridgeServeDir(t, serveConfigTOML)

	stdout, stderr, exit, cancel := serveUntilReady(t, dir, nil)

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d", code, cli.ExitOK)
	}

	if strings.Contains(stdout.String(), "telegram bridge running") {
		t.Fatalf("bridge banner printed with the bridge disabled:\n%s", stdout.String())
	}

	if !strings.Contains(stderr.String(), "telegram bridge disabled") {
		t.Fatalf("stderr missing the disabled log line:\n%s", stderr.String())
	}
}

// TestServeSkipsBridgeWhenTokenUnset proves an enabled bridge with an unset token
// is treated as off: one log line, no goroutine, serve runs normally. The chat
// cross-check never fires because the token gate short-circuits first.
func TestServeSkipsBridgeWhenTokenUnset(t *testing.T) {
	t.Parallel()

	dir := bridgeServeDir(t, bridgeEnabledNoChatTOML)

	stdout, stderr, exit, cancel := serveUntilReady(t, dir, nil)

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d", code, cli.ExitOK)
	}

	if strings.Contains(stdout.String(), "telegram bridge running") {
		t.Fatalf("bridge banner printed with the token unset:\n%s", stdout.String())
	}

	if !strings.Contains(stderr.String(), "token env unset") {
		t.Fatalf("stderr missing the token-unset log line:\n%s", stderr.String())
	}
}

// TestServeFailsWhenBridgeWithoutChat proves the cross-check: an enabled bridge
// with a token but chat off fails serve startup with config.ErrInvalidConfig,
// since the bridge has no agent to drive (acceptance 5).
func TestServeFailsWhenBridgeWithoutChat(t *testing.T) {
	t.Parallel()

	dir := bridgeServeDir(t, bridgeEnabledNoChatTOML)

	code, _, stderr := runEnvCLI(t, tokenEnv(bridgeTokenEnv, fakeBridgeToken), serveCmd, flagDirArg, dir)

	if code != cli.ExitFailure {
		t.Fatalf("serve exit = %d, want failure\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "requires [chat] enabled") {
		t.Fatalf("stderr missing the cross-check error:\n%s", stderr)
	}
}

// TestServeFailsWhenBridgeWithoutUsers proves the empty-allowlist guard: an
// enabled bridge with no users fails at config load, before serve wiring.
func TestServeFailsWhenBridgeWithoutUsers(t *testing.T) {
	t.Parallel()

	dir := bridgeServeDir(t, bridgeEnabledNoUsersTOML)

	code, _, stderr := runCLI(t, serveCmd, flagDirArg, dir)

	if code != cli.ExitFailure {
		t.Fatalf("serve exit = %d, want failure\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "must list at least one user") {
		t.Fatalf("stderr missing the empty-allowlist error:\n%s", stderr)
	}
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

	code := cli.New(&stdout, &stderr).Run(t.Context(), []string{serveCmd, flagDirArg, dir})
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d, want %d for a refused network bind", code, cli.ExitFailure)
	}

	if !strings.Contains(stderr.String(), "serve_without_auth_on_network") {
		t.Fatalf("stderr %q does not name the explicit open-API flag", stderr.String())
	}
}

// TestServeFailsWhenSourceCannotClone proves the boot-time source guard: a
// configured tree whose repository does not exist stops serve before the runner
// registry is built, and the stage name says which step refused.
func TestServeFailsWhenSourceCannotClone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// dest sits outside dir because a source inside the CairnOS home is refused
	// at config load, which would test the wrong guard.
	dest := filepath.Join(t.TempDir(), "automation")
	repo := filepath.Join(t.TempDir(), "no-such-repo.git")

	cfgTOML := "[api]\ngrpc = \"127.0.0.1:0\"\nhttp = \"127.0.0.1:0\"\n\n" +
		"[telemetry]\nenabled = false\n\n" +
		"[source.automation]\nrepo = \"" + repo + "\"\ndest = \"" + dest + "\"\n"

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfgTOML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	code, _, stderr := runCLI(t, serveCmd, flagDirArg, dir)

	if code != cli.ExitFailure {
		t.Fatalf("serve exit = %d, want failure\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "sources") {
		t.Fatalf("stderr does not name the sources stage:\n%s", stderr)
	}

	// The block name alone would also match the dest path, so this asserts the
	// prefix EnsureAll puts in front of it.
	if !strings.Contains(stderr, "source automation") {
		t.Fatalf("stderr does not name the failing source:\n%s", stderr)
	}
}
