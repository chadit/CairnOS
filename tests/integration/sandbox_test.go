//go:build integration

package integration_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

const (
	// sandboxImage is fully qualified (registry/namespace/name:tag) so podman and
	// docker both resolve it without an interactive registry-selection prompt.
	// busybox is tiny, so the first-run pull rides comfortably inside the default
	// timeout backstop.
	sandboxImage = "docker.io/library/busybox:1.36.1"

	// sandboxAction is the one allowlisted action name. The deny-by-default gate
	// keys on Task.ActionName, so every task the live tests run must carry this.
	sandboxAction = "echo_probe"

	// sandboxWorkdir overrides the runner's /work default: busybox has no /work
	// directory, and under --read-only the runtime cannot create one, so a
	// missing workdir aborts container start. "/" always exists.
	sandboxWorkdir = "/"

	// codeSandboxExit is the permanent classification a non-zero container exit
	// carries; the live tests assert on it by value rather than importing an
	// unexported constant.
	codeSandboxExit = "sandbox_exec.exit"
)

// newSandboxRunner builds the real runner against the live runtime with no
// `runtime` override, so detection runs for real: podman on the dev machine,
// docker on CI, or the podman-backed docker wrapper when both are on PATH.
func newSandboxRunner(t *testing.T) *builtins.SandboxExec {
	t.Helper()

	sandbox, err := builtins.NewSandboxExec(map[string]any{
		"allow_actions": []any{sandboxAction},
		"image":         sandboxImage,
		"workdir":       sandboxWorkdir,
	})
	if err != nil {
		t.Fatalf("build sandbox runner: %v", err)
	}

	return sandbox
}

// sandboxProbeTask builds a resolved task for the allowlisted action running one
// shell command inside the container.
func sandboxProbeTask(command string) *runner.Task {
	return &runner.Task{
		RunID:      "sandbox-int",
		ActionName: sandboxAction,
		Params:     map[string]any{"command": command},
		Resolved:   true,
	}
}

// requireOutput fails when a result's Output is not the expected trimmed string.
// Output is typed any (the engine persists it), so the assertion unwraps it.
func requireOutput(t *testing.T, result runner.Result, want string) {
	t.Helper()

	text, isString := result.Output.(string)
	if !isString {
		t.Fatalf("Output is %T, want string", result.Output)
	}

	if got := strings.TrimSpace(text); got != want {
		t.Fatalf("Output = %q, want %q", got, want)
	}
}

// TestSandboxExecAllowlistedCommand proves the real runner runs an allowlisted
// command inside the configured image and reports the documented success shape:
// stdout as Output, and Meta carrying exit_code, the detected runtime, and the
// image. Detection runs for real here; both supported runtimes are accepted so
// the test passes on podman, docker, and CI alike.
func TestSandboxExecAllowlistedCommand(t *testing.T) {
	t.Parallel()

	sandbox := newSandboxRunner(t)

	result, err := sandbox.Execute(t.Context(), sandboxProbeTask("echo hello-from-sandbox"))
	if err != nil {
		t.Fatalf("execute allowlisted command: %v", err)
	}

	requireOutput(t, result, "hello-from-sandbox")

	if got := result.Meta["exit_code"]; got != "0" {
		t.Errorf("Meta[exit_code] = %q, want %q", got, "0")
	}

	if runtime := result.Meta["runtime"]; runtime != "podman" && runtime != "docker" {
		t.Errorf("Meta[runtime] = %q, want podman or docker", runtime)
	}

	if got := result.Meta["image"]; got != sandboxImage {
		t.Errorf("Meta[image] = %q, want %q", got, sandboxImage)
	}
}

// TestSandboxExecNonZeroExit proves a non-zero container exit classifies as a
// permanent sandbox_exec.exit carrying the exit code in its message, so the
// engine never retries a command the container reported as failed.
func TestSandboxExecNonZeroExit(t *testing.T) {
	t.Parallel()

	sandbox := newSandboxRunner(t)

	_, err := sandbox.Execute(t.Context(), sandboxProbeTask("exit 3"))

	classified, isRunnerErr := errors.AsType[*runner.Error](err)
	if !isRunnerErr {
		t.Fatalf("error is %T (%v), want *runner.Error", err, err)
	}

	if classified.Class != runner.ClassPermanent {
		t.Errorf("Class = %v, want ClassPermanent", classified.Class)
	}

	if classified.Code != codeSandboxExit {
		t.Errorf("Code = %q, want %q", classified.Code, codeSandboxExit)
	}

	if !strings.Contains(classified.Message, "3") {
		t.Errorf("Message %q does not carry the exit code 3", classified.Message)
	}
}

// TestSandboxExecNetworkOffByDefault proves the --network=none default takes
// effect in a live container, not just in the built argv: with no network the
// container sees only the loopback interface, so /sys/class/net lists exactly
// "lo". This is the runtime half of the isolation boundary: it must hold, not
// merely be requested.
func TestSandboxExecNetworkOffByDefault(t *testing.T) {
	t.Parallel()

	sandbox := newSandboxRunner(t)

	result, err := sandbox.Execute(t.Context(), sandboxProbeTask("ls /sys/class/net"))
	if err != nil {
		t.Fatalf("list network interfaces: %v", err)
	}

	requireOutput(t, result, "lo")
}

// TestSandboxExecReadOnlyRootWithTmpfsScratch proves the hardening flags
// take effect in a live container: --read-only makes the
// root filesystem unwritable while the paired --tmpfs keeps /tmp available for
// scratch. A write outside /tmp is refused (permanent exit), and a write into
// /tmp succeeds.
func TestSandboxExecReadOnlyRootWithTmpfsScratch(t *testing.T) {
	t.Parallel()

	sandbox := newSandboxRunner(t)

	t.Run("write outside tmpfs is refused", func(t *testing.T) {
		t.Parallel()

		_, err := sandbox.Execute(t.Context(), sandboxProbeTask("echo blocked > /etc/probe"))

		classified, isRunnerErr := errors.AsType[*runner.Error](err)
		if !isRunnerErr {
			t.Fatalf("error is %T (%v), want *runner.Error", err, err)
		}

		if classified.Code != codeSandboxExit {
			t.Fatalf("Code = %q, want %q", classified.Code, codeSandboxExit)
		}

		if !strings.Contains(strings.ToLower(classified.Message), "read-only") {
			t.Errorf("Message %q does not mention the read-only filesystem", classified.Message)
		}
	})

	t.Run("tmpfs scratch is writable", func(t *testing.T) {
		t.Parallel()

		result, err := sandbox.Execute(t.Context(),
			sandboxProbeTask("echo scratch-ok > /tmp/probe && cat /tmp/probe"))
		if err != nil {
			t.Fatalf("write tmpfs scratch: %v", err)
		}

		requireOutput(t, result, "scratch-ok")
	})
}
