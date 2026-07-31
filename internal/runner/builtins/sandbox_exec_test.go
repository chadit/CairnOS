package builtins_test

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

const (
	rtPodman       = "podman"
	rtDocker       = "docker"
	sandboxImage   = "docker.io/library/busybox:1.36.1"
	actionBuild    = "build_project"
	cmdKey         = "command"
	wdKey          = "workdir"
	cannedOutput   = "sandbox-canned-output"
	sampleCmd      = "echo hello"
	testDockerHost = "tcp://x"

	cfgAllow    = "allow_actions"
	cfgImageKey = "image"
	cfgRuntime  = "runtime"
	cfgNetwork  = "network"
	cfgHarden   = "harden"
	cfgMounts   = "mounts"
	cfgTimeout  = "timeout"
	cfgPids     = "pids_limit"
	cfgMemory   = "memory"
	cfgCPUs     = "cpus"
	cfgAllowRW  = "allow_rw_mounts"

	codeDenied        = builtins.SandboxExecName + ".denied"
	codeNotConfigured = builtins.SandboxExecName + ".not_configured"
	codeNoRuntime     = builtins.SandboxExecName + ".no_runtime"
	codeTimeout       = builtins.SandboxExecName + ".timeout"
	codeExit          = builtins.SandboxExecName + ".exit"

	// containerNamePattern is the injection-safe shape every generated name
	// must hold: cairnos, a sanitized run id, and 8 hex random characters.
	containerNamePattern = `^cairnos-[A-Za-z0-9_.-]+-[0-9a-f]{8}$`
)

// probeFor builds a detection probe from a set of on-PATH binaries, a
// DOCKER_HOST value, and whether the docker socket probe succeeds.
func probeFor(onPath []string, dockerHost string, socket bool) builtins.SandboxProbe {
	present := make(map[string]struct{}, len(onPath))
	for _, binary := range onPath {
		present[binary] = struct{}{}
	}

	return builtins.SandboxProbe{
		LookPath: func(name string) (string, error) {
			if _, found := present[name]; found {
				return "/usr/bin/" + name, nil
			}

			return "", exec.ErrNotFound
		},
		Getenv: func(key string) string {
			if key == "DOCKER_HOST" {
				return dockerHost
			}

			return ""
		},
		StatSocket: func(string) bool { return socket },
	}
}

// sandboxConfig builds a runnable config (podman forced, one allowed action,
// an image) and overlays any extra keys.
func sandboxConfig(extra map[string]any) map[string]any {
	cfg := map[string]any{
		cfgAllow:    []any{actionBuild},
		cfgImageKey: sandboxImage,
		cfgRuntime:  rtPodman,
	}

	maps.Copy(cfg, extra)

	return cfg
}

// sandboxConfigNoRuntime is sandboxConfig without the runtime override, so
// detection runs against an injected probe.
func sandboxConfigNoRuntime() map[string]any {
	return map[string]any{
		cfgAllow:    []any{actionBuild},
		cfgImageKey: sandboxImage,
	}
}

// sandboxTask builds a resolved task for the allowed action.
func sandboxTask() *runner.Task {
	return sandboxTaskRun(testRunID)
}

// sandboxTaskRun builds a resolved task with an explicit run id, for the
// kill-argv injection check.
func sandboxTaskRun(runID string) *runner.Task {
	return &runner.Task{
		RunID:      runID,
		ActionName: actionBuild,
		Params:     map[string]any{cmdKey: sampleCmd},
		Resolved:   true,
	}
}

// recordingStubBody returns a stub that records its run argv to argvFile and
// prints a canned line, so tests can inspect the exact argv the runner built.
func recordingStubBody(argvFile string) string {
	return `if [ "$1" = "kill" ]; then exit 0; fi
printf '%s\n' "$@" > "` + argvFile + `"
echo "` + cannedOutput + `"
exit 0`
}

// stageRecordingStub writes a recording "podman" stub into a fresh dir and
// returns the dir and its argv record file. The caller installs PATH so the
// paralleltest linter sees the env mutation in the test body.
func stageRecordingStub(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	writeShim(t, dir, rtPodman, recordingStubBody(argvFile))

	return dir, argvFile
}

// pathWith prepends dir to the current PATH.
func pathWith(dir string) string {
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// executeRecording builds a runner from cfg, runs task, and returns the argv
// the recording stub saw.
func executeRecording(t *testing.T, cfg map[string]any, task *runner.Task, argvFile string) []string {
	t.Helper()

	sandbox, err := builtins.NewSandboxExec(cfg)
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	if _, execErr := sandbox.Execute(t.Context(), task); execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}

	return readLines(t, argvFile)
}

// readLines reads a newline-delimited record file into a slice.
func readLines(t *testing.T, file string) []string {
	t.Helper()

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read record file: %v", err)
	}

	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}

	return strings.Split(text, "\n")
}

// flagValue returns the argv element following flag.
func flagValue(argv []string, flag string) (string, bool) {
	for idx := range argv {
		if argv[idx] == flag && idx+1 < len(argv) {
			return argv[idx+1], true
		}
	}

	return "", false
}

// assertContainerName fails when name is not the sanitized, injection-safe shape.
func assertContainerName(t *testing.T, name string) {
	t.Helper()

	if !regexp.MustCompile(containerNamePattern).MatchString(name) {
		t.Fatalf("container name %q does not match %q", name, containerNamePattern)
	}
}

// sandboxError asserts err is a classified runner error and returns it.
func sandboxError(t *testing.T, err error) *runner.Error {
	t.Helper()

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	return classified
}

func TestSandboxDetectionPrecedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		configRT    string
		onPath      []string
		dockerHost  string
		socket      bool
		wantRuntime string
		wantErr     bool
	}{
		{name: "config podman wins over everything", configRT: rtPodman, onPath: []string{rtPodman, rtDocker}, dockerHost: testDockerHost, socket: true, wantRuntime: rtPodman},
		{name: "config docker with missing binary is no_runtime", configRT: rtDocker, onPath: []string{rtPodman}, wantErr: true},
		{name: "docker host plus docker resolves docker", onPath: []string{rtDocker, rtPodman}, dockerHost: testDockerHost, wantRuntime: rtDocker},
		{name: "docker host but docker missing falls through to podman", onPath: []string{rtPodman}, dockerHost: testDockerHost, wantRuntime: rtPodman},
		{name: "socket plus docker resolves docker", onPath: []string{rtDocker, rtPodman}, socket: true, wantRuntime: rtDocker},
		{name: "socket but docker missing falls to podman", onPath: []string{rtPodman}, socket: true, wantRuntime: rtPodman},
		{name: "podman only resolves podman", onPath: []string{rtPodman}, wantRuntime: rtPodman},
		{name: "docker only resolves docker", onPath: []string{rtDocker}, wantRuntime: rtDocker},
		{name: "nothing resolves no_runtime", wantErr: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := map[string]any{}
			if testCase.configRT != "" {
				cfg[cfgRuntime] = testCase.configRT
			}

			probe := probeFor(testCase.onPath, testCase.dockerHost, testCase.socket)

			sandbox, err := builtins.NewSandboxExecWithProbe(cfg, probe)
			if err != nil {
				t.Fatalf("new sandbox: %v", err)
			}

			runtimeName, resolveErr := sandbox.Runtime()

			if testCase.wantErr {
				if code := sandboxError(t, resolveErr).Code; code != codeNoRuntime {
					t.Fatalf("code = %q, want %q", code, codeNoRuntime)
				}

				return
			}

			if resolveErr != nil {
				t.Fatalf("resolve: %v", resolveErr)
			}

			if runtimeName != testCase.wantRuntime {
				t.Fatalf("runtime = %q, want %q", runtimeName, testCase.wantRuntime)
			}
		})
	}
}

func TestSandboxDetectionIsPerInstance(t *testing.T) {
	t.Parallel()

	podmanRunner, err := builtins.NewSandboxExecWithProbe(nil, probeFor([]string{rtPodman}, "", false))
	if err != nil {
		t.Fatalf("new podman runner: %v", err)
	}

	dockerRunner, err := builtins.NewSandboxExecWithProbe(nil, probeFor([]string{rtDocker}, "", false))
	if err != nil {
		t.Fatalf("new docker runner: %v", err)
	}

	podmanName, err := podmanRunner.Runtime()
	if err != nil {
		t.Fatalf("podman runtime: %v", err)
	}

	dockerName, err := dockerRunner.Runtime()
	if err != nil {
		t.Fatalf("docker runtime: %v", err)
	}

	if podmanName != rtPodman || dockerName != rtDocker {
		t.Fatalf("detection shared state: podman=%q docker=%q", podmanName, dockerName)
	}
}

func TestSandboxArgvHardenedByDefault(t *testing.T) {
	dir, argvFile := stageRecordingStub(t)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	argv := executeRecording(t, sandboxConfig(nil), sandboxTask(), argvFile)

	wantFlags := []string{
		"--network=none",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--read-only",
		"--tmpfs=/tmp",
		"--pids-limit=128",
		"--memory=512m",
		"--cpus=1",
	}

	for _, flag := range wantFlags {
		if !slices.Contains(argv, flag) {
			t.Fatalf("argv missing %q: %v", flag, argv)
		}
	}

	if workdir, found := flagValue(argv, "-w"); !found || workdir != "/work" {
		t.Fatalf("workdir flag = %q (found %v), want /work", workdir, found)
	}

	if !slices.Contains(argv, sandboxImage) {
		t.Fatalf("argv missing image %q: %v", sandboxImage, argv)
	}

	tail := argv[len(argv)-3:]
	if tail[0] != "sh" || tail[1] != "-c" || tail[2] != sampleCmd {
		t.Fatalf("argv tail = %v, want [sh -c %q]", tail, sampleCmd)
	}

	name, found := flagValue(argv, "--name")
	if !found {
		t.Fatalf("argv has no --name: %v", argv)
	}

	assertContainerName(t, name)
}

func TestSandboxArgvNetworkOptIn(t *testing.T) {
	dir, argvFile := stageRecordingStub(t)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	argv := executeRecording(t, sandboxConfig(map[string]any{cfgNetwork: true}), sandboxTask(), argvFile)

	if slices.Contains(argv, "--network=none") {
		t.Fatalf("network=true still emitted --network=none: %v", argv)
	}
}

func TestSandboxArgvHardenOptOut(t *testing.T) {
	dir, argvFile := stageRecordingStub(t)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	argv := executeRecording(t, sandboxConfig(map[string]any{cfgHarden: false}), sandboxTask(), argvFile)

	for _, flag := range []string{"--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only", "--tmpfs=/tmp"} {
		if slices.Contains(argv, flag) {
			t.Fatalf("harden=false still emitted %q: %v", flag, argv)
		}
	}

	// Resource caps ship regardless of the hardening opt-out (amendment 2).
	if !slices.Contains(argv, "--pids-limit=128") {
		t.Fatalf("harden=false dropped the pids cap: %v", argv)
	}
}

func TestSandboxArgvMountRendersReadOnly(t *testing.T) {
	dir, argvFile := stageRecordingStub(t)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	cfg := sandboxConfig(map[string]any{cfgMounts: []any{"/hostsrc:/ctrdst"}})
	argv := executeRecording(t, cfg, sandboxTask(), argvFile)

	mount, found := flagValue(argv, "-v")
	if !found || mount != "/hostsrc:/ctrdst:ro" {
		t.Fatalf("mount = %q (found %v), want /hostsrc:/ctrdst:ro", mount, found)
	}
}

func TestSandboxArgvWorkdirParamOverride(t *testing.T) {
	dir, argvFile := stageRecordingStub(t)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	task := sandboxTask()
	task.Params[wdKey] = "/custom"

	argv := executeRecording(t, sandboxConfig(nil), task, argvFile)

	if workdir, found := flagValue(argv, "-w"); !found || workdir != "/custom" {
		t.Fatalf("workdir flag = %q (found %v), want /custom", workdir, found)
	}
}

func TestSandboxKillArgvCannotBeInjected(t *testing.T) {
	dir, argvFile := stageRecordingStub(t)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	hostileRunID := `run1; rm -rf / $(touch pwned) && echo`
	argv := executeRecording(t, sandboxConfig(nil), sandboxTaskRun(hostileRunID), argvFile)

	name, found := flagValue(argv, "--name")
	if !found {
		t.Fatalf("argv has no --name: %v", argv)
	}

	assertContainerName(t, name)

	for _, meta := range []string{";", " ", "$", "(", ")", "&", "/", "`"} {
		if strings.Contains(name, meta) {
			t.Fatalf("container name %q retained shell metacharacter %q", name, meta)
		}
	}
}

func TestSandboxGateDeniesByDefault(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		cfg        map[string]any
		actionName string
		resolved   bool
		wantDenied bool
	}{
		{name: "empty config denies at load", cfg: nil, actionName: actionBuild, resolved: false, wantDenied: true},
		{name: "empty config denies at launch", cfg: nil, actionName: actionBuild, resolved: true, wantDenied: true},
		{name: "non-allowlisted action denied", cfg: sandboxConfig(nil), actionName: "other_action", resolved: true, wantDenied: true},
		{name: "allowlisted action passes", cfg: sandboxConfig(nil), actionName: actionBuild, resolved: true, wantDenied: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			sandbox, err := builtins.NewSandboxExec(testCase.cfg)
			if err != nil {
				t.Fatalf("new sandbox: %v", err)
			}

			task := &runner.Task{
				RunID:      testRunID,
				ActionName: testCase.actionName,
				Params:     map[string]any{cmdKey: sampleCmd},
				Resolved:   testCase.resolved,
			}

			err = sandbox.Validate(task)

			if !testCase.wantDenied {
				if err != nil {
					t.Fatalf("allowlisted action rejected: %v", err)
				}

				return
			}

			if code := sandboxError(t, err).Code; code != codeDenied {
				t.Fatalf("code = %q, want %q", code, codeDenied)
			}
		})
	}
}

func TestSandboxExecuteRefusesNonAllowlistedAction(t *testing.T) {
	t.Parallel()

	sandbox, err := builtins.NewSandboxExec(sandboxConfig(nil))
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	task := &runner.Task{
		RunID:      testRunID,
		ActionName: "not_listed",
		Params:     map[string]any{cmdKey: sampleCmd},
		Resolved:   true,
	}

	_, err = sandbox.Execute(t.Context(), task)

	if code := sandboxError(t, err).Code; code != codeDenied {
		t.Fatalf("code = %q, want %q", code, codeDenied)
	}
}

func TestSandboxDenyErrorDoesNotEchoCommand(t *testing.T) {
	t.Parallel()

	sandbox, err := builtins.NewSandboxExec(nil)
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	command := "echo do-not-leak-this-payload"
	task := &runner.Task{
		RunID:      testRunID,
		ActionName: actionBuild,
		Params:     map[string]any{cmdKey: command},
		Resolved:   true,
	}

	validateErr := sandbox.Validate(task)
	if validateErr == nil {
		t.Fatal("expected denial")
	}

	if message := sandboxError(t, validateErr).Message; strings.Contains(message, command) {
		t.Fatalf("deny error leaked the command param: %q", message)
	}
}

func TestSandboxNotConfiguredWithoutImage(t *testing.T) {
	t.Parallel()

	sandbox, err := builtins.NewSandboxExec(map[string]any{cfgAllow: []any{actionBuild}})
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	err = sandbox.Validate(sandboxTask())

	if code := sandboxError(t, err).Code; code != codeNotConfigured {
		t.Fatalf("code = %q, want %q", code, codeNotConfigured)
	}
}

func TestSandboxNoRuntimeAtExecute(t *testing.T) {
	t.Parallel()

	sandbox, err := builtins.NewSandboxExecWithProbe(sandboxConfigNoRuntime(), probeFor(nil, "", false))
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	_, err = sandbox.Execute(t.Context(), sandboxTask())

	sbxErr := sandboxError(t, err)
	if sbxErr.Class != runner.ClassPermanent || sbxErr.Code != codeNoRuntime {
		t.Fatalf("class/code = %v/%q, want permanent %q", sbxErr.Class, sbxErr.Code, codeNoRuntime)
	}
}

func TestSandboxExecuteSuccessShape(t *testing.T) {
	dir, _ := stageRecordingStub(t)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	sandbox, err := builtins.NewSandboxExec(sandboxConfig(nil))
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	result, err := sandbox.Execute(t.Context(), sandboxTask())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if output, isString := result.Output.(string); !isString || output != cannedOutput {
		t.Fatalf("output = %#v, want %q", result.Output, cannedOutput)
	}

	if result.Meta["exit_code"] != "0" {
		t.Fatalf("exit_code = %q, want 0", result.Meta["exit_code"])
	}

	if result.Meta["runtime"] != rtPodman {
		t.Fatalf("runtime meta = %q, want %q", result.Meta["runtime"], rtPodman)
	}

	if result.Meta["image"] != sandboxImage {
		t.Fatalf("image meta = %q, want %q", result.Meta["image"], sandboxImage)
	}

	if result.Usage.Latency <= 0 {
		t.Fatalf("latency = %v, want > 0", result.Usage.Latency)
	}
}

// TestSandboxDeadlineAtStartIsTransientTimeout proves a deadline already spent
// when the runtime launches classifies as the transient timeout classifyWait
// reports once the container runs, not a permanent start fault that would deny
// a retry the fresh deadline could clear.
func TestSandboxDeadlineAtStartIsTransientTimeout(t *testing.T) {
	dir, _ := stageRecordingStub(t)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	sandbox, err := builtins.NewSandboxExec(sandboxConfig(nil))
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	task := sandboxTask()
	task.Deadline = time.Now().Add(-time.Second)

	_, err = sandbox.Execute(t.Context(), task)

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || classified.Class != runner.ClassTransient || classified.Code != "sandbox_exec.timeout" {
		t.Fatalf("error = %v, want transient sandbox_exec.timeout", err)
	}
}

func TestSandboxExecuteCapsOutput(t *testing.T) {
	dir := t.TempDir()
	body := `if [ "$1" = "kill" ]; then exit 0; fi
i=0
while [ $i -lt 20000 ]; do
  echo "0123456789012345678901234567890123456789012345678901234567890123"
  i=$((i+1))
done
exit 0`
	writeShim(t, dir, rtPodman, body)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	sandbox, err := builtins.NewSandboxExec(sandboxConfig(nil))
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	result, err := sandbox.Execute(t.Context(), sandboxTask())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isString := result.Output.(string)
	if !isString {
		t.Fatalf("output type = %T", result.Output)
	}

	cap1MiB := 1 << 20
	if len(output) > cap1MiB+128 {
		t.Fatalf("output length %d exceeds 1 MiB cap", len(output))
	}

	if len(output) < cap1MiB/2 {
		t.Fatalf("output length %d suggests the stub under-produced", len(output))
	}
}

func TestSandboxExecuteNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	body := `if [ "$1" = "kill" ]; then exit 0; fi
echo "boom-from-container" >&2
exit 3`
	writeShim(t, dir, rtPodman, body)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	sandbox, err := builtins.NewSandboxExec(sandboxConfig(nil))
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	_, err = sandbox.Execute(t.Context(), sandboxTask())

	sbxErr := sandboxError(t, err)
	if sbxErr.Class != runner.ClassPermanent || sbxErr.Code != codeExit {
		t.Fatalf("class/code = %v/%q, want permanent %q", sbxErr.Class, sbxErr.Code, codeExit)
	}

	if !strings.Contains(sbxErr.Message, "boom-from-container") {
		t.Fatalf("error missing stderr tail: %q", sbxErr.Message)
	}
}

func TestSandboxExecuteTimeoutKillsContainer(t *testing.T) {
	dir := t.TempDir()
	killFile := filepath.Join(dir, "kills.txt")
	body := `if [ "$1" = "kill" ]; then
  printf '%s\n' "$2" >> "` + killFile + `"
  exit 0
fi
exec sleep 30`
	writeShim(t, dir, rtPodman, body)
	t.Setenv("PATH", pathWith(dir))
	t.Setenv("DOCKER_HOST", "")

	sandbox, err := builtins.NewSandboxExec(sandboxConfig(nil))
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	task := sandboxTask()
	task.Deadline = time.Now().Add(100 * time.Millisecond)

	start := time.Now()

	_, err = sandbox.Execute(t.Context(), task)

	sbxErr := sandboxError(t, err)
	if sbxErr.Class != runner.ClassTransient || sbxErr.Code != codeTimeout {
		t.Fatalf("class/code = %v/%q, want transient %q", sbxErr.Class, sbxErr.Code, codeTimeout)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("kill path took %v, want prompt return", elapsed)
	}

	killed := readLines(t, killFile)
	if len(killed) == 0 {
		t.Fatal("kill path did not invoke <runtime> kill <name>")
	}

	assertContainerName(t, killed[0])
}

func TestSandboxConfigDecodeErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{name: "image wrong type", cfg: map[string]any{cfgImageKey: int64(7)}},
		{name: "runtime bad value", cfg: map[string]any{cfgRuntime: "containerd"}},
		{name: "timeout zero rejected", cfg: map[string]any{cfgTimeout: int64(0)}},
		{name: "timeout negative rejected", cfg: map[string]any{cfgTimeout: int64(-5)}},
		{name: "pids limit non-positive rejected", cfg: map[string]any{cfgPids: int64(0)}},
		{name: "cpus non-positive rejected", cfg: map[string]any{cfgCPUs: float64(0)}},
		{name: "cpus wrong type rejected", cfg: map[string]any{cfgCPUs: "fast"}},
		{name: "memory empty rejected", cfg: map[string]any{cfgMemory: "   "}},
		{name: "allow_actions wrong type", cfg: map[string]any{cfgAllow: "not-an-array"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := builtins.NewSandboxExec(testCase.cfg)
			if !errors.Is(err, builtins.ErrConfigValue) {
				t.Fatalf("err = %v, want ErrConfigValue", err)
			}
		})
	}
}

func TestSandboxMountValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mounts  []any
		allowRW bool
		wantErr bool
	}{
		{name: "docker socket source refused", mounts: []any{"/var/run/docker.sock:/sock"}, wantErr: true},
		{name: "podman socket source refused", mounts: []any{"/run/podman/podman.sock:/sock"}, wantErr: true},
		{name: "podman machine socket refused", mounts: []any{"/tmp/podman-machine-default-api.sock:/sock"}, wantErr: true},
		{name: "relative host path refused", mounts: []any{"relative:/ctr"}, wantErr: true},
		{name: "relative container path refused", mounts: []any{"/host:relative"}, wantErr: true},
		{name: "unknown mode refused", mounts: []any{"/host:/ctr:z"}, wantErr: true},
		{name: "too many parts refused", mounts: []any{"/host:/ctr:ro:extra"}, wantErr: true},
		{name: "rw without allow refused", mounts: []any{"/host:/ctr:rw"}, wantErr: true},
		{name: "rw with allow accepted", mounts: []any{"/host:/ctr:rw"}, allowRW: true, wantErr: false},
		{name: "missing mode accepted", mounts: []any{"/host:/ctr"}, wantErr: false},
		{name: "explicit ro accepted", mounts: []any{"/host:/ctr:ro"}, wantErr: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := map[string]any{cfgMounts: testCase.mounts}
			if testCase.allowRW {
				cfg[cfgAllowRW] = true
			}

			_, err := builtins.NewSandboxExec(cfg)

			if testCase.wantErr {
				if !errors.Is(err, builtins.ErrConfigValue) {
					t.Fatalf("err = %v, want ErrConfigValue", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("valid mount rejected: %v", err)
			}
		})
	}
}

func TestSandboxDescribeExposesCommandTool(t *testing.T) {
	t.Parallel()

	sandbox, err := builtins.NewSandboxExec(nil)
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}

	if _, isStreamer := any(sandbox).(runner.Streamer); !isStreamer {
		t.Fatal("sandbox_exec does not implement Streamer")
	}

	describer, isDescriber := any(sandbox).(runner.Describer)
	if !isDescriber {
		t.Fatal("sandbox_exec does not implement Describer")
	}

	spec := describer.Describe()
	if spec.Name != builtins.SandboxExecName {
		t.Fatalf("spec name = %q, want %q", spec.Name, builtins.SandboxExecName)
	}

	properties, isMap := spec.InputSchema["properties"].(map[string]any)
	if !isMap {
		t.Fatalf("schema properties = %#v", spec.InputSchema["properties"])
	}

	if _, hasCommand := properties[cmdKey]; !hasCommand {
		t.Fatal("schema missing command property")
	}

	if _, hasWorkdir := properties[wdKey]; !hasWorkdir {
		t.Fatal("schema missing workdir property")
	}

	required, isList := spec.InputSchema["required"].([]any)
	if !isList || !slices.Contains(required, any(cmdKey)) {
		t.Fatalf("required = %#v, want command required", spec.InputSchema["required"])
	}
}
