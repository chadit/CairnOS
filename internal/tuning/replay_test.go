package tuning_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/chadit/CairnOS/internal/registry"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/tuning"
	"github.com/chadit/CairnOS/pkg/runner"
)

// errScripted is the failure the scripted runner returns on demand.
var errScripted = errors.New("scripted runner failure")

// scriptedRunner is a runner.Runner that returns a canned output (or error)
// and records every task it received, so replay tests can assert the
// proposed params actually reached the runner.
type scriptedRunner struct {
	output  any
	execErr error
	tasks   []*runner.Task
	mu      sync.Mutex
}

// Name implements runner.Runner.
func (*scriptedRunner) Name() string { return runnerAnthropic }

// Execute implements runner.Runner.
func (r *scriptedRunner) Execute(_ context.Context, task *runner.Task) (runner.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tasks = append(r.tasks, task)

	if r.execErr != nil {
		return runner.Result{}, r.execErr
	}

	return runner.Result{Output: r.output}, nil
}

// Validate implements runner.Runner.
func (*scriptedRunner) Validate(*runner.Task) error { return nil }

// seenTasks returns the tasks Execute received.
func (r *scriptedRunner) seenTasks() []*runner.Task {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.tasks
}

// soleRunner is a runner table holding target under the one runner name every
// replay fixture resolves.
func soleRunner(target runner.Runner) *registry.Table[runner.Runner] {
	runners := &registry.Table[runner.Runner]{}
	runners.Add(runnerAnthropic, target)

	return runners
}

// paramSuggestionFixture builds an output-changing suggestion the way the
// detector emits it, with the given starting confidence.
func paramSuggestionFixture(confidence string) *store.Suggestion {
	return &store.Suggestion{
		ID:        "sug-replay",
		Kind:      store.SuggestionParamTuning,
		Target:    verboseWorkflow + "." + actionSummarize,
		Rationale: "max_tokens sits far above observed output.",
		Evidence: map[string]any{
			tuning.EvidenceRunner: runnerAnthropic,
			tuning.EvidenceAction: actionSummarize,
		},
		Proposed: map[string]any{
			tuning.ProposedMaxTokens: int64(128),
		},
		EstSavings: store.Savings{CostUSDPerRun: 0.5, Confidence: confidence},
		Status:     store.SuggestionOpen,
	}
}

// replayEngine wires an engine whose only moving parts are the scripted
// runner and the exact-match comparator.
func replayEngine(target runner.Runner) *tuning.Engine {
	cfg := tuning.DefaultConfig()
	cfg.Runners = soleRunner(target)

	return tuning.New(nil, cfg)
}

func TestReplayIdenticalOutputStands(t *testing.T) {
	t.Parallel()

	scripted := &scriptedRunner{output: sameOutput}
	engine := replayEngine(scripted)
	suggestion := paramSuggestionFixture(store.ConfidenceLow)

	samples := []tuning.Sample{{
		Params:         map[string]any{paramPrompt: promptText, keyMaxTokens: int64(4096)},
		Inputs:         map[string]any{"topic": queryJobs},
		RecordedOutput: sameOutput,
	}}

	if err := engine.Replay(t.Context(), suggestion, samples); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if suggestion.Evidence[tuning.EvidenceReplayOutcome] != tuning.ReplayMatch {
		t.Fatalf("replay outcome = %v, want match", suggestion.Evidence[tuning.EvidenceReplayOutcome])
	}

	if suggestion.Evidence[tuning.EvidenceReplaySamples] != 1 {
		t.Fatalf("replay samples = %v, want 1", suggestion.Evidence[tuning.EvidenceReplaySamples])
	}

	if _, recorded := suggestion.Evidence[tuning.EvidenceReplayReason]; recorded {
		t.Fatal("a matching replay must not record a divergence reason")
	}

	tasks := scripted.seenTasks()
	if len(tasks) != 1 {
		t.Fatalf("runner saw %d tasks, want 1", len(tasks))
	}

	// The proposed cap must override the recorded value while the rest of
	// the recorded params ride along.
	if tasks[0].Params[keyMaxTokens] != int64(128) || tasks[0].Params[paramPrompt] != promptText {
		t.Fatalf("replayed params = %v, want proposed max_tokens over recorded prompt", tasks[0].Params)
	}
}

func TestReplayDivergenceDowngradesWithReason(t *testing.T) {
	t.Parallel()

	scripted := &scriptedRunner{output: "a different answer"}
	engine := replayEngine(scripted)

	// Medium going in makes the downgrade observable; the detector itself
	// never emits param tuning above low.
	suggestion := paramSuggestionFixture(store.ConfidenceMedium)

	samples := []tuning.Sample{{
		Params:         map[string]any{paramPrompt: promptText},
		RecordedOutput: sameOutput,
	}}

	if err := engine.Replay(t.Context(), suggestion, samples); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if suggestion.Evidence[tuning.EvidenceReplayOutcome] != tuning.ReplayDiverged {
		t.Fatalf("replay outcome = %v, want diverged", suggestion.Evidence[tuning.EvidenceReplayOutcome])
	}

	if suggestion.EstSavings.Confidence != store.ConfidenceLow {
		t.Fatalf("confidence = %s, want low after divergence", suggestion.EstSavings.Confidence)
	}

	reason, _ := suggestion.Evidence[tuning.EvidenceReplayReason].(string)
	if reason == "" {
		t.Fatal("divergence must record its reason on the suggestion")
	}

	if !strings.Contains(suggestion.Rationale, "diverged") {
		t.Fatalf("rationale does not mention the divergence: %q", suggestion.Rationale)
	}
}

func TestReplayExecutionFailureCountsAsDivergence(t *testing.T) {
	t.Parallel()

	scripted := &scriptedRunner{execErr: errScripted}
	engine := replayEngine(scripted)
	suggestion := paramSuggestionFixture(store.ConfidenceLow)

	samples := []tuning.Sample{{RecordedOutput: sameOutput}}

	if err := engine.Replay(t.Context(), suggestion, samples); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if suggestion.Evidence[tuning.EvidenceReplayOutcome] != tuning.ReplayDiverged {
		t.Fatalf("replay outcome = %v, want diverged on execution failure", suggestion.Evidence[tuning.EvidenceReplayOutcome])
	}

	reason, _ := suggestion.Evidence[tuning.EvidenceReplayReason].(string)
	if !strings.Contains(reason, "replay execution failed") {
		t.Fatalf("reason = %q, want the execution failure surfaced", reason)
	}
}

func TestReplayWithoutSamplesIsSkipped(t *testing.T) {
	t.Parallel()

	scripted := &scriptedRunner{output: sameOutput}
	engine := replayEngine(scripted)
	suggestion := paramSuggestionFixture(store.ConfidenceLow)

	if err := engine.Replay(t.Context(), suggestion, nil); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if suggestion.Evidence[tuning.EvidenceReplayOutcome] != tuning.ReplaySkipped {
		t.Fatalf("replay outcome = %v, want skipped", suggestion.Evidence[tuning.EvidenceReplayOutcome])
	}

	if len(scripted.seenTasks()) != 0 {
		t.Fatal("a skipped replay must not invoke the runner")
	}
}

func TestReplayCapsSamples(t *testing.T) {
	t.Parallel()

	scripted := &scriptedRunner{output: sameOutput}
	engine := replayEngine(scripted)
	suggestion := paramSuggestionFixture(store.ConfidenceLow)

	// Five recorded samples against the default cap of three: replay spends
	// real tokens per sample, so the cap is a cost guard.
	const recordedSampleCount = 5

	samples := make([]tuning.Sample, recordedSampleCount)
	for idx := range samples {
		samples[idx] = tuning.Sample{RecordedOutput: sameOutput}
	}

	if err := engine.Replay(t.Context(), suggestion, samples); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if got := len(scripted.seenTasks()); got != 3 {
		t.Fatalf("runner saw %d tasks, want the default cap of 3", got)
	}

	if suggestion.Evidence[tuning.EvidenceReplaySamples] != 3 {
		t.Fatalf("replay samples = %v, want 3", suggestion.Evidence[tuning.EvidenceReplaySamples])
	}
}

func TestReplayRefusals(t *testing.T) {
	t.Parallel()

	scripted := &scriptedRunner{output: sameOutput}

	tests := []struct {
		wantErr    error
		engine     *tuning.Engine
		suggestion *store.Suggestion
		name       string
	}{
		{
			name:   "output-neutral kinds are not replayable",
			engine: replayEngine(scripted),
			suggestion: &store.Suggestion{
				Kind:     store.SuggestionCaching,
				Evidence: map[string]any{tuning.EvidenceRunner: runnerAnthropic},
			},
			wantErr: tuning.ErrReplayNotApplicable,
		},
		{
			name:       "an engine without a runner source refuses replay",
			engine:     tuning.New(nil, tuning.DefaultConfig()),
			suggestion: paramSuggestionFixture(store.ConfidenceLow),
			wantErr:    tuning.ErrReplayNoRunnerSource,
		},
		{
			name:   "missing runner evidence is an error",
			engine: replayEngine(scripted),
			suggestion: &store.Suggestion{
				Kind:     store.SuggestionParamTuning,
				Evidence: map[string]any{},
			},
			wantErr: tuning.ErrReplayRunnerMissing,
		},
		{
			name:   "an unregistered runner is an error",
			engine: replayEngine(scripted),
			suggestion: &store.Suggestion{
				Kind:     store.SuggestionParamTuning,
				Evidence: map[string]any{tuning.EvidenceRunner: "unknown-runner"},
			},
			wantErr: tuning.ErrReplayRunnerUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			samples := []tuning.Sample{{RecordedOutput: sameOutput}}

			err := tt.engine.Replay(t.Context(), tt.suggestion, samples)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("replay err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
