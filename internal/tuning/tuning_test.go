package tuning_test

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/tuning"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Fixture literals shared across the detector tests.
const (
	repeatWorkflow  = "repeat-wf"
	burstWorkflow   = "burst-wf"
	verboseWorkflow = "verbose-wf"
	hashRepeat      = "hash-repeat"
	hashBurst       = "hash-burst"
	hashVerbose     = "hash-verbose"
	actionFetch     = "fetch"
	actionSummarize = "summarize"
	runnerAnthropic = "anthropic"
	runnerOpenAI    = "openai"
	sameOutput      = "same answer"
	triggerCLI      = "cli"
	keyMaxTokens    = "max_tokens"
	promptText      = "summarize this"
	queryJobs       = "jobs"
)

// floatTolerance absorbs float64 rounding in savings assertions.
const floatTolerance = 1e-9

// openStore opens a fresh database under t.TempDir, proving the engine reads
// the real persistence layer rather than a shape-alike fake.
func openStore(t *testing.T) *sqlite.Store {
	t.Helper()

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})

	return db
}

// seedRun stages one succeeded run row.
func seedRun(t *testing.T, db *sqlite.Store, id, workflowName, hash string, inputs map[string]any, created time.Time) {
	t.Helper()

	err := db.CreateRun(t.Context(), &store.Run{
		ID:           id,
		WorkflowName: workflowName,
		WorkflowHash: hash,
		Status:       store.RunSucceeded,
		Inputs:       inputs,
		Trigger:      triggerCLI,
		CreatedAt:    created,
	})
	if err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
}

// seedAction stages one succeeded action row.
func seedAction(t *testing.T, db *sqlite.Store, runID, action, runnerName string, output any, usage runner.Usage, startedAt time.Time) {
	t.Helper()

	err := db.UpsertActionResult(t.Context(), &store.ActionResult{
		RunID:      runID,
		ActionName: action,
		Runner:     runnerName,
		Status:     store.ActionSucceeded,
		Output:     output,
		Usage:      usage,
		StartedAt:  startedAt,
		FinishedAt: startedAt.Add(usage.Latency),
	})
	if err != nil {
		t.Fatalf("seed action %s/%s: %v", runID, action, err)
	}
}

// seedCachingCorpus stages runCount runs of the repeat workflow whose fetch
// action always sees the same inputs and returns the same output at the
// given cost.
func seedCachingCorpus(t *testing.T, db *sqlite.Store, runCount int, costPerCall float64) {
	t.Helper()

	base := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	inputs := map[string]any{"query": queryJobs}

	for idx := range runCount {
		runID := fmt.Sprintf("repeat-run-%d", idx)
		created := base.Add(time.Duration(idx) * time.Hour)

		seedRun(t, db, runID, repeatWorkflow, hashRepeat, inputs, created)
		seedAction(t, db, runID, actionFetch, runnerAnthropic, sameOutput, runner.Usage{
			InputTokens:  120,
			OutputTokens: 40,
			CostUSD:      costPerCall,
			Latency:      time.Second,
		}, created.Add(time.Second))
	}
}

func TestCachingDetectorFires(t *testing.T) {
	t.Parallel()

	db := openStore(t)
	seedCachingCorpus(t, db, 4, 0.10)

	engine := tuning.New(db, tuning.DefaultConfig())

	suggestions, err := engine.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(suggestions) != 1 {
		t.Fatalf("got %d suggestions, want 1 caching suggestion", len(suggestions))
	}

	got := suggestions[0]

	if got.Kind != store.SuggestionCaching {
		t.Fatalf("kind = %s, want caching", got.Kind)
	}

	if got.Target != repeatWorkflow+"."+actionFetch {
		t.Fatalf("target = %s, want %s.%s", got.Target, repeatWorkflow, actionFetch)
	}

	if got.ID == "" || got.Status != store.SuggestionOpen {
		t.Fatalf("suggestion not stamped for review: id=%q status=%q", got.ID, got.Status)
	}

	if got.Evidence[tuning.EvidenceRepeatCount] != 4 {
		t.Fatalf("repeat_count = %v, want 4", got.Evidence[tuning.EvidenceRepeatCount])
	}

	costPerRepeat, _ := got.Evidence[tuning.EvidenceCostPerRepeat].(float64)
	if math.Abs(costPerRepeat-0.10) > floatTolerance {
		t.Fatalf("cost_per_repeat = %v, want 0.10", costPerRepeat)
	}

	// Three of the four repeats become cache hits: 0.10 * 3/4 per run.
	if math.Abs(got.EstSavings.CostUSDPerRun-0.075) > floatTolerance {
		t.Fatalf("savings = %v, want 0.075", got.EstSavings.CostUSDPerRun)
	}

	if got.EstSavings.Confidence != store.ConfidenceHigh {
		t.Fatalf("confidence = %s, want high", got.EstSavings.Confidence)
	}

	if got.Rationale == "" {
		t.Fatal("rationale is empty; every suggestion must explain itself")
	}
}

func TestCachingDetectorRequiresIdenticalOutputs(t *testing.T) {
	t.Parallel()

	db := openStore(t)

	base := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	inputs := map[string]any{"query": queryJobs}

	for idx := range 4 {
		runID := fmt.Sprintf("vary-run-%d", idx)
		created := base.Add(time.Duration(idx) * time.Hour)

		seedRun(t, db, runID, repeatWorkflow, hashRepeat, inputs, created)
		seedAction(t, db, runID, actionFetch, runnerAnthropic, fmt.Sprintf("answer %d", idx), runner.Usage{
			CostUSD: 0.10,
			Latency: time.Second,
		}, created.Add(time.Second))
	}

	engine := tuning.New(db, tuning.DefaultConfig())

	suggestions, err := engine.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(suggestions) != 0 {
		t.Fatalf("got %d suggestions, want none: varying outputs are not cacheable repeat work", len(suggestions))
	}
}

func TestBatchingDetectorFires(t *testing.T) {
	t.Parallel()

	db := openStore(t)

	created := time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)
	seedRun(t, db, "burst-run", burstWorkflow, hashBurst, nil, created)

	for idx := range 6 {
		seedAction(t, db, "burst-run", fmt.Sprintf("call-%d", idx), runnerOpenAI,
			fmt.Sprintf("chunk %d", idx), runner.Usage{
				CostUSD: 0.02,
				Latency: 200 * time.Millisecond,
			}, created.Add(time.Duration(idx)*5*time.Second))
	}

	engine := tuning.New(db, tuning.DefaultConfig())

	suggestions, err := engine.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(suggestions) != 1 {
		t.Fatalf("got %d suggestions, want 1 batching suggestion", len(suggestions))
	}

	got := suggestions[0]

	if got.Kind != store.SuggestionBatching {
		t.Fatalf("kind = %s, want batching", got.Kind)
	}

	if got.Target != burstWorkflow {
		t.Fatalf("target = %s, want %s", got.Target, burstWorkflow)
	}

	if got.Evidence[tuning.EvidenceCallCount] != 6 {
		t.Fatalf("call_count = %v, want 6", got.Evidence[tuning.EvidenceCallCount])
	}

	if got.Evidence[tuning.EvidenceRunsAffected] != 1 || got.Evidence[tuning.EvidenceRunsObserved] != 1 {
		t.Fatalf("runs evidence = %v/%v, want 1/1",
			got.Evidence[tuning.EvidenceRunsAffected], got.Evidence[tuning.EvidenceRunsObserved])
	}

	overhead, _ := got.Evidence[tuning.EvidenceCallOverhead].(float64)
	if math.Abs(overhead-0.02) > floatTolerance {
		t.Fatalf("per_call_overhead_usd = %v, want 0.02", overhead)
	}

	// Five of the six calls fold into the batch: 5 * 0.02 in the only run.
	if math.Abs(got.EstSavings.CostUSDPerRun-0.10) > floatTolerance {
		t.Fatalf("savings = %v, want 0.10", got.EstSavings.CostUSDPerRun)
	}

	if got.EstSavings.Confidence != store.ConfidenceMedium {
		t.Fatalf("confidence = %s, want medium", got.EstSavings.Confidence)
	}
}

// seedParamCorpus stages runCount runs of the verbose workflow whose
// summarize action emits small outputs with distinct text, so only the param
// detector has anything to say.
func seedParamCorpus(t *testing.T, db *sqlite.Store, runCount int) {
	t.Helper()

	base := time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC)

	for idx := range runCount {
		runID := fmt.Sprintf("verbose-run-%d", idx)
		created := base.Add(time.Duration(idx) * time.Hour)

		seedRun(t, db, runID, verboseWorkflow, hashVerbose, nil, created)
		seedAction(t, db, runID, actionSummarize, runnerAnthropic,
			fmt.Sprintf("summary %d", idx), runner.Usage{
				InputTokens:  200,
				OutputTokens: 50,
				CostUSD:      0.05,
				Latency:      time.Second,
			}, created.Add(time.Second))
	}
}

func TestParamTuningDetectorFires(t *testing.T) {
	t.Parallel()

	db := openStore(t)
	seedParamCorpus(t, db, 5)

	cfg := tuning.DefaultConfig()
	cfg.RunnerConfigs = map[string]map[string]any{
		runnerAnthropic: {keyMaxTokens: int64(4096)},
	}

	engine := tuning.New(db, cfg)

	suggestions, err := engine.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(suggestions) != 1 {
		t.Fatalf("got %d suggestions, want 1 param-tuning suggestion", len(suggestions))
	}

	got := suggestions[0]

	if got.Kind != store.SuggestionParamTuning {
		t.Fatalf("kind = %s, want param_tuning", got.Kind)
	}

	if got.Target != verboseWorkflow+"."+actionSummarize {
		t.Fatalf("target = %s, want %s.%s", got.Target, verboseWorkflow, actionSummarize)
	}

	// p95 of five 50-token outputs is 50; doubled for headroom and rounded
	// up to the next multiple of 64 gives 128.
	if got.Proposed[tuning.ProposedMaxTokens] != int64(128) {
		t.Fatalf("proposed max_tokens = %v, want 128", got.Proposed[tuning.ProposedMaxTokens])
	}

	if got.Evidence[tuning.EvidenceMaxTokens] != int64(4096) {
		t.Fatalf("evidence max_tokens = %v, want 4096", got.Evidence[tuning.EvidenceMaxTokens])
	}

	dist, isMap := got.Evidence[tuning.EvidenceOutputTokens].(map[string]any)
	if !isMap || dist["p95"] != int64(50) || dist["max"] != int64(50) {
		t.Fatalf("output token distribution = %v, want p95 and max of 50", got.Evidence[tuning.EvidenceOutputTokens])
	}

	// Blended price is 0.25/1250 = 0.0002 per token; the avoided cap overrun
	// is (4096-128) tokens of it.
	wantSavings := float64(4096-128) * 0.0002
	if math.Abs(got.EstSavings.CostUSDPerRun-wantSavings) > floatTolerance {
		t.Fatalf("savings = %v, want %v", got.EstSavings.CostUSDPerRun, wantSavings)
	}

	if got.EstSavings.Confidence != store.ConfidenceLow {
		t.Fatalf("confidence = %s, want low: param tuning changes output", got.EstSavings.Confidence)
	}
}

func TestParamTuningRespectsRatioGate(t *testing.T) {
	t.Parallel()

	db := openStore(t)
	seedParamCorpus(t, db, 5)

	// A 150-token cap sits under 4x the 50-token p95, so the gap is too
	// narrow to bother a human with.
	cfg := tuning.DefaultConfig()
	cfg.RunnerConfigs = map[string]map[string]any{
		runnerAnthropic: {keyMaxTokens: int64(150)},
	}

	engine := tuning.New(db, cfg)

	suggestions, err := engine.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(suggestions) != 0 {
		t.Fatalf("got %d suggestions, want none under the ratio gate", len(suggestions))
	}
}

func TestSavingsFloorSuppression(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		costPerCall float64
		floor       float64
		wantCount   int
	}{
		{
			name:        "savings under the default floor never appear",
			costPerCall: 0.001,
			floor:       0,
			wantCount:   0,
		},
		{
			name:        "savings over the default floor appear",
			costPerCall: 0.10,
			floor:       0,
			wantCount:   1,
		},
		{
			name:        "a lower configured floor admits small savings",
			costPerCall: 0.001,
			floor:       0.0001,
			wantCount:   1,
		},
		{
			name:        "a higher configured floor suppresses real savings",
			costPerCall: 0.10,
			floor:       1.00,
			wantCount:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := openStore(t)
			seedCachingCorpus(t, db, 4, tt.costPerCall)

			cfg := tuning.DefaultConfig()
			if tt.floor > 0 {
				cfg.MinSavingsUSD = tt.floor
			}

			engine := tuning.New(db, cfg)

			suggestions, err := engine.Scan(t.Context())
			if err != nil {
				t.Fatalf("scan: %v", err)
			}

			if len(suggestions) != tt.wantCount {
				t.Fatalf("got %d suggestions, want %d", len(suggestions), tt.wantCount)
			}
		})
	}
}

func TestScanSinceWindow(t *testing.T) {
	t.Parallel()

	db := openStore(t)
	seedCachingCorpus(t, db, 4, 0.10)

	cfg := tuning.DefaultConfig()
	// Every seeded run was created in June 2026; a later cutoff hides them.
	cfg.Since = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	engine := tuning.New(db, cfg)

	suggestions, err := engine.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(suggestions) != 0 {
		t.Fatalf("got %d suggestions, want none outside the since window", len(suggestions))
	}
}
