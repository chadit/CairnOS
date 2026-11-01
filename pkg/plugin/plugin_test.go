package plugin_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/plugin/v1"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/plugin"
)

const (
	// helperPluginEnv marks the test binary these tests re-execute as a plugin.
	helperPluginEnv = "CAIRNOS_TEST_SDK_PLUGIN"

	// describeTimeout bounds the wait for the plugin to bind its socket and
	// answer the handshake.
	describeTimeout = 10 * time.Second

	// stopTimeout bounds how long Serve may take to return once told to stop.
	stopTimeout = 10 * time.Second

	// heldCallParams makes the mock answer long after stopTimeout, so a drain
	// that waited for it would fail the test.
	heldCallParams = `{"delay_ms": 60000}`

	// handoffBufferSize gives the Wait goroutine a slot for the exit status, so
	// it can finish after the test has already failed on the timer.
	handoffBufferSize = 1
)

// TestHelperPlugin is the plugin process, not a test of its own: it serves the
// mock runner and passes only when Serve returns cleanly.
func TestHelperPlugin(t *testing.T) {
	t.Parallel()

	if os.Getenv(helperPluginEnv) == "" {
		t.Skip("plugin process for the Serve stop tests")
	}

	if err := plugin.Serve(builtins.NewMock()); err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// TestServeStopsWhenStdinCloses closes a serving plugin's stdin pipe, which is
// what a dead host does, and expects Serve to return without any signal and
// without waiting for a call nobody is left to receive.
func TestServeStopsWhenStdinCloses(t *testing.T) {
	t.Parallel()

	proc, socket, output := pluginCommand(t)

	stdin, err := proc.StdinPipe()
	if err != nil {
		t.Fatalf("open plugin stdin: %v", err)
	}

	if err := proc.Start(); err != nil {
		t.Fatalf("start plugin: %v", err)
	}

	conn := dialPlugin(t, socket)

	// The Describe below shares the held call's connection and reaches the
	// server after it, so once Describe answers, the held call is in flight.
	holdCall(t, conn)
	requireServing(t, conn)

	if err := stdin.Close(); err != nil {
		t.Fatalf("close plugin stdin: %v", err)
	}

	requireCleanExit(t, proc, output, "stdin closed")
}

// TestServeIgnoresNullStdin starts a plugin with stdin on the null device, as
// systemd or nohup would, and expects it to keep serving until SIGTERM.
func TestServeIgnoresNullStdin(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot send SIGTERM to another process")
	}

	// A nil Stdin is os.DevNull, which reads EOF at once.
	proc, socket, output := pluginCommand(t)

	if err := proc.Start(); err != nil {
		t.Fatalf("start plugin: %v", err)
	}

	requireServing(t, dialPlugin(t, socket))

	if err := proc.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal plugin: %v", err)
	}

	requireCleanExit(t, proc, output, "SIGTERM")
}

// pluginCommand builds, without starting, a re-execution of this test binary
// as a plugin on a socket in the test's temp dir. Its output lands in the
// returned buffer, which is safe to read once Wait has returned.
func pluginCommand(t *testing.T) (*exec.Cmd, string, *bytes.Buffer) {
	t.Helper()

	socket := filepath.Join(t.TempDir(), "p.sock")

	proc := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHelperPlugin$")

	proc.Env = append(os.Environ(), helperPluginEnv+"=1", plugin.SocketEnv+"="+socket)

	var output bytes.Buffer

	proc.Stdout = &output
	proc.Stderr = &output

	return proc, socket, &output
}

// dialPlugin connects to the plugin's socket for the rest of the test. The
// target is unix:<path> rather than unix://<path> because a Windows drive
// letter does not parse as a URL authority.
func dialPlugin(t *testing.T, socket string) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient("unix:"+filepath.ToSlash(socket), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial plugin socket: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// holdCall starts an Execute the mock answers only after heldCallParams' delay
// and leaves it running.
func holdCall(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()

	request := &pluginv1.ExecuteRequest{Task: &pluginv1.Task{ParamsJson: heldCallParams, Resolved: true}}

	if _, err := pluginv1.NewRunnerPluginClient(conn).Execute(t.Context(), request, grpc.WaitForReady(true)); err != nil {
		t.Fatalf("start held execute: %v", err)
	}
}

// requireServing completes a Describe, which only a running Serve answers.
func requireServing(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), describeTimeout)
	defer cancel()

	resp, err := pluginv1.NewRunnerPluginClient(conn).Describe(ctx, &pluginv1.DescribeRequest{}, grpc.WaitForReady(true))
	if err != nil {
		t.Fatalf("describe plugin: %v", err)
	}

	if resp.GetName() != builtins.MockName {
		t.Fatalf("plugin name = %q, want %q", resp.GetName(), builtins.MockName)
	}
}

// requireCleanExit waits for the plugin to exit with status 0 after the named
// stop. A plugin still running at the deadline is killed when the test ends.
func requireCleanExit(t *testing.T, proc *exec.Cmd, output *bytes.Buffer, stop string) {
	t.Helper()

	exited := make(chan error, handoffBufferSize)

	go func() { exited <- proc.Wait() }()

	timer := time.NewTimer(stopTimeout)
	defer timer.Stop()

	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("plugin exit = %v after %s, want a clean stop\n%s", err, stop, output.String())
		}
	case <-timer.C:
		t.Fatalf("plugin still serving %v after %s", stopTimeout, stop)
	}
}
