package plugin_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/runner/plugin"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture names so goconst stays quiet and typos fail in one place.
const (
	echoName     = "echo"
	testRunID    = "run-1"
	failClassKey = "fail_class"
	messageKey   = "message"
	chunkOne     = "alpha"
	chunkTwo     = "beta"
	chunkThree   = "gamma"
	chunkFour    = "delta"
	chunksKey    = "chunks"
	countKey     = "count"
	greeting     = "hello"
	pingMessage  = "ping"

	// The fail_class values the echo plugin turns into classified errors.
	classTransient = "transient"
	classPermanent = "permanent"

	pluginDownCode = "echo.plugin_down"

	// hostHelperEnv hands the echo binary path to the test binary that
	// TestPluginExitsWhenHostDies re-executes as a stand-in host.
	hostHelperEnv = "CAIRNOS_TEST_PLUGIN_HOST"

	// orphanExitTimeout bounds how long a plugin whose host died may take to
	// read the end of its stdin and exit.
	orphanExitTimeout = 10 * time.Second
)

// buildEchoPlugin compiles the reference plugin into the test's own temp
// dir. Go's build cache makes the repeat compiles cheap, and tying the
// binary to t.TempDir keeps every parallel test self-contained.
func buildEchoPlugin(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), echoName)

	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary,
		"github.com/chadit/CairnOS/examples/plugins/echo")

	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build echo plugin: %v\n%s", err, output)
	}

	return binary
}

// newEchoPlugin spawns one echo plugin process and registers its teardown.
func newEchoPlugin(t *testing.T) *plugin.Plugin {
	t.Helper()

	plug, err := plugin.New(t.Context(), plugin.Config{Command: []string{buildEchoPlugin(t)}, Name: echoName})
	if err != nil {
		t.Fatalf("new echo plugin: %v", err)
	}

	t.Cleanup(func() { _ = plug.Close() })

	return plug
}

// echoTask wraps params in a resolved task.
func echoTask(params map[string]any) *runner.Task {
	return &runner.Task{
		RunID:      testRunID,
		ActionName: echoName,
		Params:     params,
		Resolved:   true,
	}
}

func TestPluginExecuteRoundTripsNativeTypedOutput(t *testing.T) {
	t.Parallel()

	plug := newEchoPlugin(t)

	params := map[string]any{
		messageKey: greeting,
		countKey:   int64(3),
		"ratio":    0.5,
		"flag":     true,
		"list":     []any{chunkOne, chunkTwo},
		"nested":   map[string]any{"key": "value"},
	}

	result, err := plug.Execute(t.Context(), echoTask(params))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Numbers come back float64: the JSON wire encoding keeps the engine's
	// own encoding/json semantics end to end, and that is the documented
	// shape of plugin output.
	want := maps.Clone(params)
	want[countKey] = float64(3)

	if !reflect.DeepEqual(result.Output, want) {
		t.Fatalf("output = %#v, want %#v", result.Output, want)
	}

	if result.Usage.InputTokens != int64(len(params)) {
		t.Fatalf("input tokens = %d, want %d", result.Usage.InputTokens, len(params))
	}

	if _, err := strconv.Atoi(result.Meta["pid"]); err != nil {
		t.Fatalf("meta pid = %q, want a process id", result.Meta["pid"])
	}
}

func TestPluginClassifiedFailuresSurviveTheWire(t *testing.T) {
	t.Parallel()

	plug := newEchoPlugin(t)

	cases := []struct {
		params         map[string]any
		name           string
		wantCode       string
		wantClass      runner.ErrorClass
		wantRetryAfter time.Duration
	}{
		{
			name:      classTransient,
			params:    map[string]any{failClassKey: classTransient},
			wantClass: runner.ClassTransient,
			wantCode:  "echo.transient",
		},
		{
			name:      classPermanent,
			params:    map[string]any{failClassKey: classPermanent},
			wantClass: runner.ClassPermanent,
			wantCode:  "echo.permanent",
		},
		{
			name:           "rate limited keeps the retry-after hint",
			params:         map[string]any{failClassKey: "rate_limited", "retry_after_ms": int64(1500)},
			wantClass:      runner.ClassRateLimited,
			wantCode:       "echo.rate_limited",
			wantRetryAfter: 1500 * time.Millisecond,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := plug.Execute(t.Context(), echoTask(tt.params))

			classified, isClassified := errors.AsType[*runner.Error](err)
			if !isClassified {
				t.Fatalf("error %v is not a runner.Error", err)
			}

			if classified.Class != tt.wantClass || classified.Code != tt.wantCode {
				t.Fatalf("class/code = %v/%q, want %v/%q",
					classified.Class, classified.Code, tt.wantClass, tt.wantCode)
			}

			if classified.RetryAfter != tt.wantRetryAfter {
				t.Fatalf("retry after = %v, want %v", classified.RetryAfter, tt.wantRetryAfter)
			}

			if classified.Message == "" {
				t.Fatal("message is empty, want the plugin's text")
			}
		})
	}
}

func TestPluginStreamsChunksInOrder(t *testing.T) {
	t.Parallel()

	plug := newEchoPlugin(t)

	out := make(chan runner.Chunk, 16)

	_, err := plug.ExecuteStream(t.Context(), echoTask(map[string]any{
		chunksKey: []any{chunkOne, chunkTwo, chunkThree},
	}), out)
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}

	close(out)

	var lines []string

	for chunk := range out {
		lines = append(lines, chunk.Data)

		if chunk.Time.IsZero() {
			t.Fatal("chunk time is zero, want the plugin's timestamp")
		}
	}

	if want := []string{chunkOne, chunkTwo, chunkThree}; !slices.Equal(lines, want) {
		t.Fatalf("chunks = %v, want %v", lines, want)
	}
}

func TestPluginValidateTranslatesVerdicts(t *testing.T) {
	t.Parallel()

	plug := newEchoPlugin(t)

	cases := []struct {
		name     string
		params   map[string]any
		wantCode string
	}{
		{name: "clean params pass", params: map[string]any{messageKey: "fine"}},
		{name: "rejection crosses the wire classified", params: map[string]any{"bad_param": true}, wantCode: "echo.bad_param"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := plug.Validate(echoTask(tt.params))

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("validate: %v, want nil", err)
				}

				return
			}

			classified, isClassified := errors.AsType[*runner.Error](err)
			if !isClassified {
				t.Fatalf("error %v is not a runner.Error", err)
			}

			if classified.Class != runner.ClassPermanent || classified.Code != tt.wantCode {
				t.Fatalf("class/code = %v/%q, want permanent/%q", classified.Class, classified.Code, tt.wantCode)
			}
		})
	}
}

// pluginPID runs one harmless echo call and reads the process id the plugin
// reports in result meta, so a later test can signal the process directly.
func pluginPID(t *testing.T, plug *plugin.Plugin) int {
	t.Helper()

	result, err := plug.Execute(t.Context(), echoTask(map[string]any{messageKey: pingMessage}))
	if err != nil {
		t.Fatalf("probe execute: %v", err)
	}

	pid, err := strconv.Atoi(result.Meta["pid"])
	if err != nil {
		t.Fatalf("meta pid = %q, want a process id", result.Meta["pid"])
	}

	return pid
}

// streamErr carries an ExecuteStream's error back from its goroutine.
type streamErr struct {
	err error
}

func TestPluginKilledMidExecuteIsTransientPluginDown(t *testing.T) {
	t.Parallel()

	plug := newEchoPlugin(t)

	// Learn the pid from a clean call first, so the kill below has a process
	// tree to address.
	pid := pluginPID(t, plug)

	// An unbuffered consumer is the synchronization point. The plugin streams
	// chunks before its terminal result; once the test takes the first chunk
	// and then stops reading, the host parks forwarding the second chunk and
	// cannot reach the result behind it. Killing the process group there
	// breaks the in-flight call before any terminal event lands, every run.
	out := make(chan runner.Chunk)
	done := make(chan streamErr, 1)

	go func() {
		_, err := plug.ExecuteStream(t.Context(), echoTask(map[string]any{
			chunksKey: []any{chunkOne, chunkTwo, chunkThree, chunkFour},
		}), out)
		done <- streamErr{err: err}
	}()

	// Take the first chunk, then stop reading: the host is now blocked
	// forwarding the next chunk, mid-execute and ahead of the result.
	<-out

	killProcessTree(t, pid)

	outcome := <-done

	classified, isClassified := errors.AsType[*runner.Error](outcome.err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", outcome.err)
	}

	if classified.Class != runner.ClassTransient || classified.Code != pluginDownCode {
		t.Fatalf("class/code = %v/%q, want transient/%s", classified.Class, classified.Code, pluginDownCode)
	}

	// The host saw the process die, so the cause says how it exited.
	exitErr, exited := errors.AsType[*exec.ExitError](outcome.err)
	if !exited || exitErr.Success() {
		t.Fatalf("error %v does not carry the plugin's failed exit status", outcome.err)
	}

	// The dead process must not wedge later calls either: they fail fast
	// with the same classified error instead of hanging.
	_, err := plug.Execute(t.Context(), echoTask(map[string]any{messageKey: "after crash"}))

	postCrash, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || postCrash.Code != pluginDownCode {
		t.Fatalf("post-crash error = %v, want echo.plugin_down", err)
	}
}

func TestPluginCloseTerminatesProcessAndRemovesSocketDir(t *testing.T) {
	t.Parallel()

	plug := newEchoPlugin(t)

	result, err := plug.Execute(t.Context(), echoTask(map[string]any{messageKey: "alive"}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	pid, err := strconv.Atoi(result.Meta["pid"])
	if err != nil {
		t.Fatalf("meta pid = %q, want a process id", result.Meta["pid"])
	}

	socketDir := plug.SocketDir()

	if err := plug.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	requireProcessGone(t, pid)

	if _, statErr := os.Stat(socketDir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("socket dir %q still present after Close (stat err = %v)", socketDir, statErr)
	}
}

// TestHelperPluginHost is the stand-in host, not a test of its own. It starts
// the echo plugin, prints the plugin's pid and socket directory on two lines,
// then waits on its own stdin, so it lives until it is killed or the test that
// started it goes away.
func TestHelperPluginHost(t *testing.T) {
	t.Parallel()

	echoBinary := os.Getenv(hostHelperEnv)
	if echoBinary == "" {
		t.Skip("stand-in host for TestPluginExitsWhenHostDies")
	}

	plug, err := plugin.New(t.Context(), plugin.Config{Command: []string{echoBinary}, Name: echoName})
	if err != nil {
		t.Fatalf("new echo plugin: %v", err)
	}

	t.Cleanup(func() { _ = plug.Close() })

	fmt.Println(pluginPID(t, plug))
	fmt.Println(plug.SocketDir())

	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("read stand-in host stdin: %v", err)
	}
}

// TestPluginExitsWhenHostDies kills a host that never called Close and expects
// its plugin to exit on its own once the stdin pipe the host held is gone.
func TestPluginExitsWhenHostDies(t *testing.T) {
	t.Parallel()

	// The plugin shares the host's stderr, so this pipe reads EOF only once both
	// have exited. The plugin is not this test's child, so there is no Wait for
	// it, and the pipe works the same on every OS.
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create shared stderr pipe: %v", err)
	}

	t.Cleanup(func() { _ = stderrRead.Close() })

	host, pid := startHelperHost(t, stderrWrite)

	if err := host.Process.Kill(); err != nil {
		t.Fatalf("kill stand-in host: %v", err)
	}

	// A host that exited cleanly ran Close, which would pass this test without
	// the plugin ever noticing a dead host.
	waitErr := host.Wait()
	if exitErr, exited := errors.AsType[*exec.ExitError](waitErr); !exited || exitErr.Success() {
		t.Fatalf("stand-in host exit = %v, want killed", waitErr)
	}

	requirePluginExits(t, stderrRead, pid)
}

// startHelperHost re-executes this test binary as a host whose stderr is
// stderrWrite and returns it once its plugin is serving, with the plugin's pid.
// The host's stdin pipe is never written, so it holds until the host is killed.
// A killed host never runs Close, so the test removes the socket directory.
func startHelperHost(t *testing.T, stderrWrite *os.File) (*exec.Cmd, int) {
	t.Helper()

	host := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHelperPluginHost$")
	host.Env = append(os.Environ(), hostHelperEnv+"="+buildEchoPlugin(t))
	host.Stderr = stderrWrite

	if _, err := host.StdinPipe(); err != nil {
		t.Fatalf("open stand-in host stdin: %v", err)
	}

	stdout, err := host.StdoutPipe()
	if err != nil {
		t.Fatalf("open stand-in host stdout: %v", err)
	}

	if err = host.Start(); err != nil {
		t.Fatalf("start stand-in host: %v", err)
	}

	// Once started, only the host and its plugin may hold the write end, or the
	// EOF the test waits for never comes.
	if err = stderrWrite.Close(); err != nil {
		t.Fatalf("close test copy of shared stderr: %v", err)
	}

	lines := bufio.NewReader(stdout)

	pidLine := readHelperLine(t, lines)

	pid, err := strconv.Atoi(pidLine)
	if err != nil {
		t.Fatalf("stand-in host printed %q, want a plugin pid", pidLine)
	}

	// Only an absolute path came from SocketDir; anything else is stray output
	// that RemoveAll must not be pointed at.
	socketDir := readHelperLine(t, lines)
	if !filepath.IsAbs(socketDir) {
		t.Fatalf("stand-in host printed %q, want an absolute socket directory", socketDir)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Errorf("remove stand-in host socket directory: %v", err)
		}
	})

	return host, pid
}

// readHelperLine reads one line the stand-in host printed, without its line
// ending.
func readHelperLine(t *testing.T, lines *bufio.Reader) string {
	t.Helper()

	line, err := lines.ReadString('\n')
	if err != nil {
		t.Fatalf("read stand-in host output: %v (got %q)", err, line)
	}

	return strings.TrimSpace(line)
}

// requirePluginExits reads the shared stderr to EOF, which comes once the
// plugin has exited. A plugin still running at the deadline is killed so the
// failure leaves no process behind.
func requirePluginExits(t *testing.T, stderr io.Reader, pid int) {
	t.Helper()

	drained := make(chan error, 1)

	go func() {
		_, err := io.Copy(io.Discard, stderr)
		drained <- err
	}()

	timer := time.NewTimer(orphanExitTimeout)
	defer timer.Stop()

	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("read shared stderr: %v", err)
		}
	case <-timer.C:
		killProcessTree(t, pid)
		t.Fatalf("plugin %d still running %v after its host died", pid, orphanExitTimeout)
	}
}

func TestPluginDescribeTranslatesToolSpec(t *testing.T) {
	t.Parallel()

	plug := newEchoPlugin(t)

	spec := plug.Describe()

	if spec.Name != echoName {
		t.Fatalf("tool name = %q, want %q", spec.Name, echoName)
	}

	if spec.Description == "" {
		t.Fatal("tool description is empty, want the plugin's text")
	}

	if got := spec.InputSchema["type"]; got != "object" {
		t.Fatalf("schema type = %v, want object", got)
	}
}

// e2eTOML runs one echo action through the full engine path.
const e2eTOML = `
[workflow]
name = "plugin_e2e"

[action.shout]
runner = "echo"
params = { message = "ping" }
`

func TestPluginEngineEndToEnd(t *testing.T) {
	t.Parallel()

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "cairnos.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	registry := runner.NewRegistry()
	if registerErr := registry.Register(echoName, plugin.Factory(t.Context(), echoName, hostEnv())); registerErr != nil {
		t.Fatalf("register plugin factory: %v", registerErr)
	}

	cfgs := map[string]map[string]any{
		echoName: {"type": "plugin", "command": []any{buildEchoPlugin(t)}},
	}

	if buildAllErr := registry.BuildAll(cfgs); buildAllErr != nil {
		t.Fatalf("build runners: %v", buildAllErr)
	}

	t.Cleanup(func() { _ = registry.Close() })

	flow, err := workflow.Parse([]byte(e2eTOML))
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}

	cfg := engine.Config{
		MaxConcurrentActions: 2,
		DefaultRetry:         reliability.Policy{MaxAttempts: 1},
		Breaker: reliability.BreakerConfig{
			FailureThreshold: 1,
			Window:           time.Minute,
			MinRequests:      1000,
			Cooldown:         time.Minute,
		},
	}

	eng := engine.New(cfg, registry, db)

	run, err := eng.RunWorkflow(t.Context(), flow, nil, "test")
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}

	if run.Status != store.RunSucceeded {
		t.Fatalf("run status = %s (error %v), want succeeded", run.Status, run.Error)
	}

	results, err := db.ListActionResults(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("list action results: %v", err)
	}

	if len(results) != 1 || results[0].ActionName != "shout" {
		t.Fatalf("results = %+v, want the one shout action", results)
	}

	output, isMap := results[0].Output.(map[string]any)
	if !isMap || output[messageKey] != pingMessage {
		t.Fatalf("stored output = %#v, want the echoed params", results[0].Output)
	}
}
