package cli

import (
	"log/slog"

	"github.com/chadit/CairnOS/internal/reach"
	"github.com/chadit/CairnOS/internal/scheduler"
)

// ConfigFileName is the config file name WriteConfigBlock edits.
const ConfigFileName = configFileName

// LocalRunnerHeader is the TOML section header a model setup writes.
const LocalRunnerHeader = localRunnerHeader

// FlagPort, FlagThreads, and FlagCtxSize are the model subcommand's numeric
// flag names.
const (
	FlagPort    = flagPort
	FlagThreads = flagThreads
	FlagCtxSize = flagCtxSize
)

// This file exists so internal/cli's _test.go files can live in the external
// cli_test package (cairnlint's externaltestpkg rule) while still driving
// ModelOptions/ModelTarget resolution and the reach-gated queue wiring, which
// have no other public entry point that stays a fast, offline unit test (the
// real CLI path downloads a live runtime and model). It is not itself a
// _test.go file, so it compiles into the package like
// internal/memory/testhelpers.go and its siblings already do elsewhere in
// this repo.

// NewModelOptions builds a ModelOptions with the fields
// TestManualStartHintIsRunnable exercises; the rest stay zero-valued.
func NewModelOptions(dir, model string, port, threads, ctxSize int) *ModelOptions {
	return &ModelOptions{dir: dir, model: model, port: port, threads: threads, ctxSize: ctxSize}
}

// ServerPath returns the resolved target's server binary path.
func (t *ModelTarget) ServerPath() string {
	return t.layout.ServerPath()
}

// ModelPath returns the resolved target's model weights path.
func (t *ModelTarget) ModelPath() string {
	return t.layout.ModelPath(&t.model)
}

// NewDirectPollAwareRunner builds a PollAwareRunner directly from its dependencies,
// bypassing newPollAwareRunner's queue-capability probe, so a reach-gate test
// can drive it against a fake queue and monitor.
func NewDirectPollAwareRunner(inner scheduler.Runner, queue pollQueue, monitor *reach.Monitor, logger *slog.Logger) *PollAwareRunner {
	return &PollAwareRunner{inner: inner, queue: queue, reach: monitor, logger: logger}
}

// Reach returns the reach monitor a PollAwareRunner gates fires on.
func (r *PollAwareRunner) Reach() *reach.Monitor {
	return r.reach
}

// NewQueueRunnerAdapter builds a QueueRunnerAdapter directly, for a
// reach-gate test that drives Run against a fake inner runner.
func NewQueueRunnerAdapter(fire inputsRunner, monitor *reach.Monitor) *QueueRunnerAdapter {
	return &QueueRunnerAdapter{fire: fire, reach: monitor}
}
