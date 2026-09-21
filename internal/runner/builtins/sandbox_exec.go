package builtins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/pkg/runner"
)

// SandboxExecName is the registry name of the sandboxed exec runner.
const SandboxExecName = "sandbox_exec"

// Sandbox config keys. timeout reuses cfgKeyTimeout from http.go (same
// package, same "seconds" convention).
const (
	cfgKeyAllowActions  = "allow_actions"
	cfgKeyImage         = "image"
	cfgKeyMounts        = "mounts"
	cfgKeyNetwork       = "network"
	cfgKeyRuntime       = "runtime"
	cfgKeyHarden        = "harden"
	cfgKeyPidsLimit     = "pids_limit"
	cfgKeyMemory        = "memory"
	cfgKeyCPUs          = "cpus"
	cfgKeyAllowRWMounts = "allow_rw_mounts"
)

// keyWorkdir is both the config default key and the per-call param override:
// each names the container working directory, so one identifier covers both.
const (
	keyWorkdir   = "workdir"
	paramCommand = "command"
)

// Supported container runtimes.
const (
	runtimePodman = "podman"
	runtimeDocker = "docker"
)

// Sandbox defaults. The resource caps ship in v1 (security amendment 2): a
// fork bomb or a memory balloon beats a timeout, so bounding reachability
// (network) and wall-clock (timeout) is not enough on its own.
const (
	defaultSandboxTimeoutSeconds = 300
	defaultSandboxWorkdir        = "/work"
	defaultSandboxPidsLimit      = 128
	defaultSandboxMemory         = "512m"
	defaultSandboxCPUs           = 1.0

	// sandboxTmpfsTarget is the writable scratch mount paired with --read-only,
	// so a hardened container still has somewhere to write temp files.
	sandboxTmpfsTarget = "/tmp"

	// sandboxKillTimeout bounds the best-effort container kill after a run's
	// context dies. Killing the client process does not stop the container.
	sandboxKillTimeout = 5 * time.Second

	// containerRandBytes seeds the container-name suffix; 4 bytes render as 8
	// hex characters, enough to make a leaked container attributable to its run.
	containerRandBytes = 4

	// dockerSocketPath is the well-known Docker API socket the detection probe
	// checks (step 3), matching scripts/integration.sh resolve_runtime.
	dockerSocketPath = "/var/run/docker.sock"
)

// Mount modes. A missing mode renders as read-only (security amendment 4):
// Docker's own default is read-write, which is the wrong default for a sandbox.
const (
	mountModeRO = "ro"
	mountModeRW = "rw"

	mountMinParts = 2
	mountMaxParts = 3
)

// Classified error codes this runner returns. The param codes
// (missing_command, bad_command, bad_workdir) come from requireStringParam /
// optionalStringParam with the SandboxExecName prefix.
const (
	codeSandboxDenied        = SandboxExecName + ".denied"
	codeSandboxNotConfigured = SandboxExecName + ".not_configured"
	codeSandboxNoRuntime     = SandboxExecName + ".no_runtime"
	codeSandboxTimeout       = SandboxExecName + ".timeout"
	codeSandboxExit          = SandboxExecName + ".exit"
	codeSandboxStart         = SandboxExecName + ".start"
	codeSandboxOutput        = SandboxExecName + ".output"
)

// SandboxExec runs one shell command in a locked-down podman/docker container.
// Denied by default and enabled per action name; image, mounts, networking, and
// caps are operator config only.
//
// Isolation boundary (security amendment 6): container isolation,
// --network=none, --cap-drop=ALL, --security-opt=no-new-privileges, resource
// caps, no default mounts, and the action gate. Workflow authors are trusted at
// operator level, since the gate carries no workflow identity.
type SandboxExec struct {
	detectErr error

	// Detection hooks, defaulted in the factory and replaced in tests. No
	// package-level cache (security judge item 2): two instances detect
	// independently.
	lookPath   func(string) (string, error)
	statSocket func(string) bool

	allowActions map[string]struct{}

	// env answers DOCKER_HOST and supplies the PATH the default lookPath
	// searches, so detection depends on injected values rather than on
	// whatever the process environment happens to hold.
	env environ.Lookup

	image      string
	workdir    string
	runtime    string
	runtimeBin string // the resolved path detection found, what launch runs
	runtimeCfg string // "", "podman", or "docker": operator detection override
	memory     string
	mounts     []string // normalized "host:ctr:mode", validated at construction
	pidsLimit  int64
	cpus       float64
	timeout    time.Duration
	detectOnce sync.Once
	harden     bool
	network    bool
}

// SandboxProbe supplies the host-environment lookups runtime detection needs.
// NewSandboxExec wires the real os/exec calls; NewSandboxExecWithProbe injects
// fakes so the detection precedence matrix is testable without a container
// runtime installed. A nil field falls back to the real lookup.
type SandboxProbe struct {
	LookPath   func(string) (string, error)
	Env        environ.Lookup
	StatSocket func(string) bool
}

// NewSandboxExec decodes the runner config block with the real host probe. An
// empty block still builds (the registry constructs every built-in at startup);
// Validate reports the deny-by-default gate and a missing image when a workflow
// references it, and runtime detection is deferred to first Execute so BuildAll
// on a runtime-less host stays green.
func NewSandboxExec(cfg map[string]any) (*SandboxExec, error) {
	return NewSandboxExecWithProbe(cfg, SandboxProbe{})
}

// NewSandboxExecWithProbe is NewSandboxExec with an injectable detection probe,
// the seam the detection tests drive.
func NewSandboxExecWithProbe(cfg map[string]any, probe SandboxProbe) (*SandboxExec, error) {
	allowList, err := configStrings(cfg, cfgKeyAllowActions)
	if err != nil {
		return nil, wrapConfigErr(SandboxExecName, err)
	}

	image, err := configValue(cfg, cfgKeyImage, "")
	if err != nil {
		return nil, wrapConfigErr(SandboxExecName, err)
	}

	workdir, err := configValue(cfg, keyWorkdir, defaultSandboxWorkdir)
	if err != nil {
		return nil, wrapConfigErr(SandboxExecName, err)
	}

	network, err := configValue(cfg, cfgKeyNetwork, false)
	if err != nil {
		return nil, wrapConfigErr(SandboxExecName, err)
	}

	harden, err := configValue(cfg, cfgKeyHarden, true)
	if err != nil {
		return nil, wrapConfigErr(SandboxExecName, err)
	}

	allowRW, err := configValue(cfg, cfgKeyAllowRWMounts, false)
	if err != nil {
		return nil, wrapConfigErr(SandboxExecName, err)
	}

	settings, err := decodeSandboxLimits(cfg)
	if err != nil {
		return nil, wrapConfigErr(SandboxExecName, err)
	}

	mounts, err := decodeMounts(cfg, allowRW)
	if err != nil {
		return nil, wrapConfigErr(SandboxExecName, err)
	}

	sandbox := &SandboxExec{
		allowActions: toSet(allowList),
		image:        image,
		workdir:      workdir,
		mounts:       mounts,
		network:      network,
		timeout:      settings.timeout,
		runtimeCfg:   settings.runtimeCfg,
		harden:       harden,
		pidsLimit:    settings.pidsLimit,
		memory:       settings.memory,
		cpus:         settings.cpus,
	}

	sandbox.applyProbe(probe)

	return sandbox, nil
}

// SandboxExecFactory adapts the runner to the registry contract.
func SandboxExecFactory(cfg map[string]any) (runner.Runner, error) {
	return NewSandboxExec(cfg)
}

// sandboxLimits is the decoded set of value-typed caps and the runtime override.
type sandboxLimits struct {
	runtimeCfg string
	memory     string
	timeout    time.Duration
	pidsLimit  int64
	cpus       float64
}

// decodeSandboxLimits validates the runtime override, timeout, and the three
// resource caps. Invalid values fail construction with ErrConfigValue rather
// than becoming a silently unbounded container (security amendment 2).
func decodeSandboxLimits(cfg map[string]any) (sandboxLimits, error) {
	runtimeCfg, err := decodeRuntime(cfg)
	if err != nil {
		return sandboxLimits{}, err
	}

	timeout, err := decodeSandboxTimeout(cfg)
	if err != nil {
		return sandboxLimits{}, err
	}

	pidsLimit, err := decodePositiveInt(cfg, cfgKeyPidsLimit, defaultSandboxPidsLimit)
	if err != nil {
		return sandboxLimits{}, err
	}

	memory, err := decodeMemory(cfg)
	if err != nil {
		return sandboxLimits{}, err
	}

	cpus, err := decodeCPUs(cfg)
	if err != nil {
		return sandboxLimits{}, err
	}

	return sandboxLimits{
		timeout:    timeout,
		runtimeCfg: runtimeCfg,
		pidsLimit:  pidsLimit,
		memory:     memory,
		cpus:       cpus,
	}, nil
}

// decodeRuntime validates the optional detection override. Anything other than
// "podman" or "docker" fails construction, so a typo is not silently ignored.
func decodeRuntime(cfg map[string]any) (string, error) {
	value, err := configValue(cfg, cfgKeyRuntime, "")
	if err != nil {
		return "", err
	}

	switch value {
	case "", runtimePodman, runtimeDocker:
		return value, nil
	default:
		return "", fmt.Errorf("%w: runtime must be %q or %q", ErrConfigValue, runtimePodman, runtimeDocker)
	}
}

// decodeSandboxTimeout reads the timeout backstop in seconds. A non-positive
// timeout is rejected (security amendment 3): a shell runner reachable from
// workflows and agent loops must never run without a wall-clock bound.
func decodeSandboxTimeout(cfg map[string]any) (time.Duration, error) {
	seconds, present, err := configInt(cfg, cfgKeyTimeout)
	if err != nil {
		return 0, err
	}

	if !present {
		return defaultSandboxTimeoutSeconds * time.Second, nil
	}

	if seconds <= 0 {
		return 0, fmt.Errorf("%w: timeout must be a positive number of seconds", ErrConfigValue)
	}

	return time.Duration(seconds) * time.Second, nil
}

// decodePositiveInt reads an optional integer cap, applying fallback when
// absent and rejecting a non-positive override.
func decodePositiveInt(cfg map[string]any, key string, fallback int64) (int64, error) {
	value, present, err := configInt(cfg, key)
	if err != nil {
		return 0, err
	}

	if !present {
		return fallback, nil
	}

	if value <= 0 {
		return 0, fmt.Errorf("%w: %s must be positive", ErrConfigValue, key)
	}

	return value, nil
}

// decodeMemory reads the memory cap string (e.g. "512m"). An empty override is
// rejected so the --memory flag always carries a value.
func decodeMemory(cfg map[string]any) (string, error) {
	value, err := configValue(cfg, cfgKeyMemory, defaultSandboxMemory)
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf(`%w: memory must be non-empty (e.g. "512m")`, ErrConfigValue)
	}

	return value, nil
}

// decodeCPUs reads the CPU cap. TOML spells 1 as int64 and 1.5 as float64, so
// configFloat accepts either; a non-positive value fails construction.
func decodeCPUs(cfg map[string]any) (float64, error) {
	value, present, err := configFloat(cfg, cfgKeyCPUs)
	if err != nil {
		return 0, err
	}

	if !present {
		return defaultSandboxCPUs, nil
	}

	if value <= 0 {
		return 0, fmt.Errorf("%w: cpus must be positive", ErrConfigValue)
	}

	return value, nil
}

// decodeMounts validates and normalizes the operator mount list. The operator
// is wiring a sandbox for agents to drive, so the rules are strict (security
// amendment 4): absolute paths on both sides, no source resolving to the
// container runtime socket (a one-line host escape), read-only by default, and
// read-write only when allow_rw_mounts is set.
func decodeMounts(cfg map[string]any, allowRW bool) ([]string, error) {
	raw, err := configStrings(cfg, cfgKeyMounts)
	if err != nil {
		return nil, err
	}

	mounts := make([]string, 0, len(raw))

	for _, entry := range raw {
		normalized, normErr := normalizeMount(entry, allowRW)
		if normErr != nil {
			return nil, normErr
		}

		mounts = append(mounts, normalized)
	}

	return mounts, nil
}

// normalizeMount checks one "host:ctr[:mode]" entry and returns it with an
// explicit mode. See decodeMounts for the rules.
func normalizeMount(entry string, allowRW bool) (string, error) {
	parts := strings.Split(entry, ":")
	if len(parts) < mountMinParts || len(parts) > mountMaxParts {
		return "", fmt.Errorf("%w: mount %q must be host:ctr[:ro]", ErrConfigValue, entry)
	}

	host, container := parts[0], parts[1]

	if !filepath.IsAbs(host) || !filepath.IsAbs(container) {
		return "", fmt.Errorf("%w: mount %q needs absolute host and container paths", ErrConfigValue, entry)
	}

	if resolvesToRuntimeSocket(host) {
		return "", fmt.Errorf("%w: mount source %q resolves to the container runtime socket", ErrConfigValue, host)
	}

	mode := mountModeRO
	if len(parts) == mountMaxParts {
		mode = parts[2]
	}

	switch mode {
	case mountModeRO:
	case mountModeRW:
		if !allowRW {
			return "", fmt.Errorf("%w: mount %q is read-write; set allow_rw_mounts = true to permit it", ErrConfigValue, entry)
		}
	default:
		return "", fmt.Errorf("%w: mount %q mode must be ro or rw", ErrConfigValue, entry)
	}

	return host + ":" + container + ":" + mode, nil
}

// resolvesToRuntimeSocket reports whether host points at a container runtime
// API socket, following symlinks when the path exists. A basename match holds
// even when the file is absent (so the guard works in tests and on hosts where
// the socket has not been created yet); the substring rule catches the podman
// machine socket (podman-machine-default-api.sock and friends).
func resolvesToRuntimeSocket(host string) bool {
	clean := filepath.Clean(host)

	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	}

	base := strings.ToLower(filepath.Base(clean))
	if base == "docker.sock" || base == "podman.sock" {
		return true
	}

	return strings.HasSuffix(base, ".sock") &&
		(strings.Contains(base, "docker") || strings.Contains(base, "podman"))
}

// Name implements runner.Runner.
func (*SandboxExec) Name() string { return SandboxExecName }

// Validate runs in both engine passes. The deny-by-default gate fires
// regardless of Task.Resolved, since ActionName is never a ${{ }} reference: a
// denied action fails at workflow load, before anything runs. An empty
// allow_actions denies everything, so an agent loop cannot self-enable the tool.
func (s *SandboxExec) Validate(task *runner.Task) error {
	if err := s.gate(task); err != nil {
		return err
	}

	if s.image == "" {
		return runner.NewError(runner.ClassPermanent, codeSandboxNotConfigured,
			"runner config must set image to run "+task.ActionName)
	}

	if err := requireStringParam(task, paramCommand, SandboxExecName); err != nil {
		return err
	}

	return optionalStringParam(task, keyWorkdir, SandboxExecName)
}

// Execute implements runner.Runner.
func (s *SandboxExec) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	return s.run(ctx, task, nil)
}

// ExecuteStream implements runner.Streamer, forwarding stdout lines as chunks
// while the transcript still accumulates into Result.Output.
func (s *SandboxExec) ExecuteStream(ctx context.Context, task *runner.Task, out chan<- runner.Chunk) (runner.Result, error) {
	return s.run(ctx, task, out)
}

// Describe implements runner.Describer, so an agent action listing sandbox_exec
// in tools passes the loop's checks. The sandbox shape (image, mounts,
// network) is deliberately absent from the schema: it is config-only.
func (*SandboxExec) Describe() runner.ToolSpec {
	return runner.ToolSpec{
		Name: SandboxExecName,
		Description: "Run a shell command inside a locked-down container. " +
			"No network unless the operator enabled it. Stdout is returned, capped at 1 MiB.",
		InputSchema: map[string]any{
			schemaType: schemaObject,
			schemaProperties: map[string]any{
				paramCommand: map[string]any{schemaType: schemaString},
				keyWorkdir:   map[string]any{schemaType: schemaString},
			},
			schemaRequired: []any{paramCommand},
		},
	}
}

// Runtime resolves and reports the container runtime this instance uses
// ("podman" or "docker"), or a permanent no_runtime error when neither is
// available. The result is memoized per instance; Execute calls it internally,
// and it is the introspection point the detection tests read.
func (s *SandboxExec) Runtime() (string, error) {
	name, _, err := s.detectRuntime()

	return name, err
}

// applyProbe wires the detection hooks, falling back to the real os/exec calls
// for any field the probe leaves nil.
func (s *SandboxExec) applyProbe(probe SandboxProbe) {
	s.env = probe.Env
	if s.env == nil {
		s.env = environ.OS()
	}

	s.lookPath = probe.LookPath
	if s.lookPath == nil {
		// Resolving through s.env rather than exec.LookPath is what lets a test
		// stage a stub binary on its own PATH without touching the process one.
		s.lookPath = func(file string) (string, error) { return environ.LookPath(s.env, file) }
	}

	s.statSocket = probe.StatSocket
	if s.statSocket == nil {
		s.statSocket = statSocket
	}
}

// gate is the deny-by-default check. Absent block, absent key, or an empty list
// all leave allowActions empty, which denies every action. Entries match
// Task.ActionName, so the one gate covers workflow actions and agent-loop tool
// calls (tool tasks carry the agent action's name).
func (s *SandboxExec) gate(task *runner.Task) error {
	if _, allowed := s.allowActions[task.ActionName]; allowed {
		return nil
	}

	return runner.NewError(runner.ClassPermanent, codeSandboxDenied,
		"action "+task.ActionName+" is not in sandbox_exec allow_actions")
}

// run validates, resolves the runtime, builds the argv, and executes one
// container. A nil stream means plain Execute.
func (s *SandboxExec) run(ctx context.Context, task *runner.Task, stream chan<- runner.Chunk) (runner.Result, error) {
	if err := s.Validate(task); err != nil {
		return runner.Result{}, err
	}

	runtimeName, runtimeBin, err := s.detectRuntime()
	if err != nil {
		return runner.Result{}, err
	}

	name, err := containerName(task.RunID)
	if err != nil {
		return runner.Result{}, runner.NewError(runner.ClassPermanent, codeSandboxStart,
			"generate container name: "+err.Error())
	}

	command, _ := task.Params[paramCommand].(string)

	workdir := s.workdir
	if override, isString := task.Params[keyWorkdir].(string); isString && override != "" {
		workdir = override
	}

	argv := buildRunArgv(&runArgs{
		runtime:       runtimeName,
		containerName: name,
		image:         s.image,
		workdir:       workdir,
		command:       command,
		mounts:        s.mounts,
		network:       s.network,
		harden:        s.harden,
		pidsLimit:     s.pidsLimit,
		memory:        s.memory,
		cpus:          s.cpus,
	})

	runCtx, cancel := s.runContext(ctx, task)
	defer cancel()

	return s.launch(ctx, runCtx, &launchArgs{runtime: runtimeName, binary: runtimeBin, container: name}, argv, stream)
}

// runContext derives the effective deadline: Task.Deadline when set, else
// now+timeout as the backstop, the earlier winning. timeout is always positive
// (amendment 3), so a hung container always has a bound.
func (s *SandboxExec) runContext(ctx context.Context, task *runner.Task) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(s.timeout)

	if !task.Deadline.IsZero() && task.Deadline.Before(deadline) {
		deadline = task.Deadline
	}

	return context.WithDeadline(ctx, deadline)
}

// launch runs the container CLI, drains stdout, and classifies the outcome. The
// kill path runs after Wait when the run context died, since killing the client
// process does not stop the container.
func (s *SandboxExec) launch(parent, runCtx context.Context,
	target *launchArgs, argv []string, stream chan<- runner.Chunk,
) (runner.Result, error) {
	start := time.Now()

	piped, err := startPiped(runCtx, func() *exec.Cmd {
		// #nosec G204 -- the runtime binary and argv skeleton come from operator
		// config and this package's code; the only task-supplied value is the
		// command string, which runs via `sh -c` inside the locked-down container
		// (network-off, caps dropped, read-only rootfs, resource-capped), never in a
		// host shell. The container is the trust boundary.
		return exec.CommandContext(runCtx, target.binary, argv[1:]...)
	})
	if err != nil {
		if errors.Is(err, errStdoutPipe) {
			return runner.Result{}, runner.NewError(runner.ClassPermanent, codeSandboxStart, err.Error())
		}

		return runner.Result{}, s.classifyStart(runCtx, err)
	}

	cmd, stderr := piped.cmd, piped.stderr

	stdoutText, scanErr := collectStdout(runCtx, piped.stdout, stream)

	waitErr := cmd.Wait()
	latency := time.Since(start)

	if runCtx.Err() != nil {
		s.killContainer(parent, target)
	}

	if waitErr != nil {
		return runner.Result{}, s.classifyWait(runCtx, waitErr, cmd.ProcessState.ExitCode(), stderr.Text())
	}

	if scanErr != nil {
		return runner.Result{}, runner.NewError(runner.ClassPermanent, codeSandboxOutput,
			"read stdout: "+scanErr.Error())
	}

	return runner.Result{
		Output: stdoutText,
		Usage:  runner.Usage{Latency: latency},
		Meta: map[string]string{
			metaExitCode:   strconv.Itoa(cmd.ProcessState.ExitCode()),
			metaStderrTail: stderr.Text(),
			cfgKeyRuntime:  target.runtime,
			cfgKeyImage:    s.image,
		},
	}, nil
}

// killContainer best-effort stops a container the client-process SIGKILL left
// running. It runs under its own short context, detached from the dead run
// context, and ignores errors: --rm handles removal and an already-gone
// container is not a failure. runtimeName comes from detection and name is
// sanitized, so a hostile RunID cannot steer the kill argv.
func (*SandboxExec) killContainer(parent context.Context, target *launchArgs) {
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), sandboxKillTimeout)
	defer cancel()

	// #nosec G204 -- the binary is the path detection resolved and the container
	// name is this package's sanitized one; both are single argv elements, so no
	// shell parsing occurs.
	_ = exec.CommandContext(killCtx, target.binary, "kill", target.container).Run()
}

// launchArgs names the resolved runtime one run uses: the reported name, the
// binary both the run and its kill fallback execute, and the container name.
type launchArgs struct {
	runtime   string
	binary    string
	container string
}

// classifyStart maps a launch failure. A missing runtime binary is permanent
// no_runtime (the class BuildAll must never hit); anything else at start is
// permanent start. A deadline expiring mid-launch is the exception: it is the
// transient timeout classifyWait reports once the container runs, and calling it
// permanent would deny a retry a fresh deadline could clear.
func (*SandboxExec) classifyStart(runCtx context.Context, err error) error {
	if runCtx.Err() != nil {
		return runner.NewError(runner.ClassTransient, codeSandboxTimeout,
			"container run canceled or timed out")
	}

	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return runner.NewError(runner.ClassPermanent, codeSandboxNoRuntime,
			"container runtime not found: "+err.Error())
	}

	return runner.NewError(runner.ClassPermanent, codeSandboxStart,
		"start container runtime: "+err.Error())
}

// classifyWait maps a finished-but-failed run. A dead run context wins as a
// transient timeout, since CommandContext kills the client on cancellation and
// the kill path already ran. A non-zero exit is permanent, carrying the stderr
// tail. First-run image pulls ride the deadline, so a pull failure lands here as
// a permanent exit (accepted for v1; the docs advise pre-pulling images).
func (*SandboxExec) classifyWait(runCtx context.Context, waitErr error, exitCode int, stderrTail string) error {
	if runCtx.Err() != nil {
		return runner.NewError(runner.ClassTransient, codeSandboxTimeout,
			"container run canceled or timed out")
	}

	if _, isExit := errors.AsType[*exec.ExitError](waitErr); isExit {
		message := fmt.Sprintf("container exited with code %d", exitCode)
		if stderrTail != "" {
			message += ": " + stderrTail
		}

		return runner.NewError(runner.ClassPermanent, codeSandboxExit, message)
	}

	return runner.NewError(runner.ClassPermanent, codeSandboxStart,
		"wait for container runtime: "+waitErr.Error())
}

// detectRuntime resolves which container CLI to shell out to, memoized per
// instance. No package-level cache (judge item 2): two instances detect
// independently.
func (s *SandboxExec) detectRuntime() (string, string, error) {
	s.detectOnce.Do(func() {
		s.runtime, s.runtimeBin, s.detectErr = s.resolveRuntime()
	})

	return s.runtime, s.runtimeBin, s.detectErr
}

// resolveRuntime mirrors scripts/integration.sh resolve_runtime precedence:
// config override, DOCKER_HOST (with docker on PATH), the docker-socket probe
// (with docker on PATH), podman on PATH, docker on PATH. Nothing found is a
// permanent no_runtime, reported at Execute only.
//
// It answers with the name and the resolved path, so launching cannot land on a
// different binary than detection approved.
func (s *SandboxExec) resolveRuntime() (string, string, error) {
	if s.runtimeCfg != "" {
		path, err := s.lookPath(s.runtimeCfg)
		if err != nil {
			return "", "", runner.NewError(runner.ClassPermanent, codeSandboxNoRuntime,
				"configured runtime "+s.runtimeCfg+" not found on PATH")
		}

		return s.runtimeCfg, path, nil
	}

	dockerPath, dockerOnPath := s.onPath(runtimeDocker)

	if s.env.Get("DOCKER_HOST") != "" && dockerOnPath {
		return runtimeDocker, dockerPath, nil
	}

	if s.statSocket(dockerSocketPath) && dockerOnPath {
		return runtimeDocker, dockerPath, nil
	}

	if podmanPath, found := s.onPath(runtimePodman); found {
		return runtimePodman, podmanPath, nil
	}

	if dockerOnPath {
		return runtimeDocker, dockerPath, nil
	}

	return "", "", runner.NewError(runner.ClassPermanent, codeSandboxNoRuntime,
		"no container runtime found: install podman or docker")
}

// onPath resolves binary through the instance's lookPath hook, reporting the
// path it found and whether the search succeeded.
func (s *SandboxExec) onPath(binary string) (string, bool) {
	path, err := s.lookPath(binary)

	return path, err == nil
}

// runArgs carries the pure inputs to buildRunArgv. A struct keeps the builder
// unit-testable without threading a dozen positional parameters.
type runArgs struct {
	runtime       string
	containerName string
	image         string
	workdir       string
	command       string
	memory        string
	mounts        []string
	pidsLimit     int64
	cpus          float64
	network       bool
	harden        bool
}

// buildRunArgv assembles the container run command. It is pure: no runtime, no
// container, no config lookup, so the security defaults are unit-testable in a
// table. harden gates the hardening flags (amendment 1); the resource caps
// (amendment 2) always ship. When a runtime rejects a flag the container start
// fails and the error surfaces through classification, never a silent drop.
func buildRunArgv(args *runArgs) []string {
	argv := []string{args.runtime, "run", "--rm", "--name", args.containerName}

	if !args.network {
		argv = append(argv, "--network=none")
	}

	if args.harden {
		argv = append(
			argv,
			"--cap-drop=ALL",
			"--security-opt=no-new-privileges",
			"--read-only",
			"--tmpfs="+sandboxTmpfsTarget,
		)
	}

	argv = append(
		argv,
		"--pids-limit="+strconv.FormatInt(args.pidsLimit, 10),
		"--memory="+args.memory,
		"--cpus="+strconv.FormatFloat(args.cpus, 'f', -1, 64),
		"-w", args.workdir,
	)

	for _, mount := range args.mounts {
		argv = append(argv, "-v", mount)
	}

	argv = append(argv, args.image, "sh", "-c", args.command)

	return argv
}

// containerName builds a unique, injection-safe container name:
// cairnos-<sanitized RunID>-<8 hex random>. Sanitizing to [A-Za-z0-9_.-] means
// a hostile RunID cannot inject into the later `<runtime> kill <name>` argv
// (amendment 7); the random suffix keeps a leaked container attributable.
func containerName(runID string) (string, error) {
	suffix := make([]byte, containerRandBytes)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("read random container-name suffix: %w", err)
	}

	return "cairnos-" + sanitizeName(runID) + "-" + hex.EncodeToString(suffix), nil
}

// sanitizeName drops every rune outside [A-Za-z0-9_.-] from the run identifier,
// falling back to "run" when nothing survives so the name always carries a
// middle segment.
func sanitizeName(runID string) string {
	var builder strings.Builder

	builder.Grow(len(runID))

	for _, char := range runID {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '_', char == '.', char == '-':
			builder.WriteRune(char)
		}
	}

	if builder.Len() == 0 {
		return "run"
	}

	return builder.String()
}

// statSocket reports whether path is a Unix domain socket, the default
// statSocket hook. Tests inject their own.
func statSocket(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeSocket != 0
}

// toSet builds a lookup set from the allow list.
func toSet(items []string) map[string]struct{} {
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}

	return set
}
