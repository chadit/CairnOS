package service

import "errors"

// ErrUnsupported reports a host with no supervisor this package can drive
// yet, so the caller prints the manual install instructions instead of
// guessing.
var ErrUnsupported = errors.New("service: no supported supervisor detected")

// ErrNeedRoot reports a system-mode request from a non-root process; the
// remedy is re-running under sudo, not degrading to a user install the
// operator did not ask for.
var ErrNeedRoot = errors.New("service: system mode needs root; re-run under sudo")

// ErrRootUserMode reports a user-mode setup running as root. A root-owned
// user unit lands in root's own systemd manager, which is never what a
// machine install wants, so the mix is refused instead of half-working.
var ErrRootUserMode = errors.New("service: refusing a user-mode install as root; pass --system for a system service")

// ErrDarwinSystem reports --system on macOS, where the LaunchDaemon path
// (and the dedicated service account it needs) is not built yet.
var ErrDarwinSystem = errors.New("service: system mode on macOS is not built yet; use user mode")

// ErrEmptyCommand is the executor's guard against a plan carrying a command
// with no argv, which would otherwise panic inside os/exec.
var ErrEmptyCommand = errors.New("service: empty command")

// ErrCommandFailed wraps a required plan command that exited non-zero, with
// the supervisor's own output attached for the operator.
var ErrCommandFailed = errors.New("service: command failed")

// ErrSidecarUnsupported reports a local-model service request on a host whose
// supervisor this package cannot give a second per-user job: Windows, and a
// Linux box running neither systemd nor a supervisor we drive. Saying so beats
// installing something that never starts.
var ErrSidecarUnsupported = errors.New(
	"service: the local model service is not supported on this host; " +
		"start llama-server by hand and point [runner.local] base_url at it",
)
