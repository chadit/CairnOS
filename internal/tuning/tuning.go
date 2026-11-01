// Package tuning is the Phase 3 suggestion engine. It reads persisted run
// history, looks for mechanical waste (repeated identical calls, burst calls
// that could batch, token caps far above observed output), and emits
// evidence-backed suggestions for a human to approve or reject. Nothing here
// changes a workflow: approval renders an annotated diff the operator applies
// by hand, and auto-apply waits for Phase 6.
package tuning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Detector defaults. Each is a starting heuristic, not a researched optimum:
// the spec fixes the inputs and outputs of this engine, and the thresholds
// grow from real data.
const (
	// defaultMinSavingsUSD drops suggestions saving less than a penny per
	// run; below that the review queue is noise.
	defaultMinSavingsUSD = 0.01

	// defaultCacheMinRepeats is how many identical (runner, params, output)
	// repeats it takes before caching is worth suggesting.
	defaultCacheMinRepeats = 3

	// defaultBatchMinCalls is how many same-runner calls inside one window
	// of a single run it takes before batching is worth suggesting.
	defaultBatchMinCalls = 5

	// defaultBatchWindow is the sliding window the batching detector slides
	// over a run's call timeline.
	defaultBatchWindow = time.Minute

	// defaultMaxTokensRatio is how far a configured max_tokens must sit
	// above the observed p95 output length before tightening is suggested.
	defaultMaxTokensRatio = 4.0

	// defaultParamMinSamples is the minimum observed outputs before the
	// param detector trusts a distribution.
	defaultParamMinSamples = 5

	// defaultReplayMaxSamples caps recorded inputs per Replay call, because
	// every sample is a real runner invocation with real cost.
	defaultReplayMaxSamples = 3
)

// Evidence map keys shared by the detectors, the CLI, and the API, so every
// surface reads the same shapes.
const (
	EvidenceWorkflow      = "workflow"
	EvidenceAction        = "action"
	EvidenceRunner        = "runner"
	EvidenceRepeatCount   = "repeat_count"
	EvidenceCostPerRepeat = "cost_per_repeat_usd"
	EvidenceParamsHash    = "params_hash"
	EvidenceCallCount     = "call_count"
	EvidenceCallOverhead  = "per_call_overhead_usd"
	EvidenceWindow        = "window"
	EvidenceRunsAffected  = "runs_affected"
	EvidenceRunsObserved  = "runs_observed"
	EvidenceMaxTokens     = "max_tokens"
	EvidenceOutputTokens  = "output_tokens"
	EvidenceSamples       = "samples"
	EvidenceReplayOutcome = "replay_outcome"
	EvidenceReplayReason  = "replay_reason"
	EvidenceReplaySamples = "replay_samples"

	// EvidenceReplaySimilarity holds the per-sample cosine scores the embedding
	// comparator produced, and EvidenceReplayThreshold the value they were
	// judged against. Together they are the R1 calibration record: the raw
	// numbers an operator reads to tune the threshold on a real corpus. Both are
	// absent under the exact-match comparator, which produces no score.
	EvidenceReplaySimilarity = "replay_similarity"
	EvidenceReplayThreshold  = "replay_threshold"
)

// Replay outcomes recorded under EvidenceReplayOutcome.
const (
	ReplayMatch    = "match"
	ReplayDiverged = "diverged"
	ReplaySkipped  = "skipped"
)

// RunSource is the slice of the store the engine reads. store.Store
// satisfies it; tests may seed a real SQLite file or fake it.
type RunSource interface {
	// ListRuns returns runs newest-first, narrowed by the filter.
	ListRuns(ctx context.Context, filter store.ListFilter) ([]*store.Run, error)

	// ListActionResults returns every action row for a run.
	ListActionResults(ctx context.Context, runID string) ([]*store.ActionResult, error)
}

// RunnerSource resolves runner names for Replay; *runner.Registry satisfies
// it.
type RunnerSource interface {
	Get(name string) (runner.Runner, bool)
}

// Config bounds one scan. Zero thresholds fall back to the documented
// defaults, so callers only set what they tune.
type Config struct {
	// Since drops runs created before it; zero scans the whole retained
	// history.
	Since time.Time

	// Runners resolves runner names for Replay. Nil disables replay, which
	// is the default: replay spends real tokens and must be asked for.
	Runners RunnerSource

	// Comparator decides output equivalence during replay. Nil means the
	// v1 exact-match comparator (the R1 placeholder; see Comparator).
	Comparator Comparator

	// RunnerConfigs carries the [runner.<name>] config blocks so the param
	// detector can read each runner's configured max_tokens.
	RunnerConfigs map[string]map[string]any

	// NewID mints suggestion IDs; nil means ULIDs.
	NewID func() string

	// MinSavingsUSD is the savings floor: suggestions whose estimated
	// per-run saving falls below it never appear
	// (config [tuning] min_savings_usd).
	MinSavingsUSD float64

	// CacheMinRepeats is the caching detector's N: identical calls across
	// at least this many runs.
	CacheMinRepeats int

	// BatchMinCalls is the batching detector's M: same-runner calls within
	// BatchWindow inside one run.
	BatchMinCalls int

	// BatchWindow is the batching detector's sliding window.
	BatchWindow time.Duration

	// MaxTokensRatio gates the param detector: configured max_tokens must
	// be at least this many times the observed p95 output tokens.
	MaxTokensRatio float64

	// ParamMinSamples is the minimum observed outputs before the param
	// detector trusts a distribution.
	ParamMinSamples int

	// ReplayMaxSamples caps recorded inputs per Replay call.
	ReplayMaxSamples int
}

// DefaultConfig returns the documented detector defaults.
func DefaultConfig() Config {
	return Config{
		MinSavingsUSD:    defaultMinSavingsUSD,
		CacheMinRepeats:  defaultCacheMinRepeats,
		BatchMinCalls:    defaultBatchMinCalls,
		BatchWindow:      defaultBatchWindow,
		MaxTokensRatio:   defaultMaxTokensRatio,
		ParamMinSamples:  defaultParamMinSamples,
		ReplayMaxSamples: defaultReplayMaxSamples,
	}
}

// Engine runs the detectors over persisted run history.
type Engine struct {
	source RunSource
	cfg    Config
}

// runRecord is one run with its action rows, the unit every detector reads.
type runRecord struct {
	run     *store.Run
	results []*store.ActionResult
}

// New builds an engine over a run source. Zero config thresholds fall back
// to the defaults, including the savings floor: a floor of exactly zero is
// not representable, by design, because an unfloored queue is noise.
//
//nolint:gocritic // hugeParam: New runs once per scan and keeps its own Config copy, so the value pass is deliberate
func New(source RunSource, cfg Config) *Engine {
	defaults := DefaultConfig()

	if cfg.MinSavingsUSD <= 0 {
		cfg.MinSavingsUSD = defaults.MinSavingsUSD
	}

	if cfg.CacheMinRepeats <= 0 {
		cfg.CacheMinRepeats = defaults.CacheMinRepeats
	}

	if cfg.BatchMinCalls <= 0 {
		cfg.BatchMinCalls = defaults.BatchMinCalls
	}

	if cfg.BatchWindow <= 0 {
		cfg.BatchWindow = defaults.BatchWindow
	}

	if cfg.MaxTokensRatio <= 0 {
		cfg.MaxTokensRatio = defaults.MaxTokensRatio
	}

	if cfg.ParamMinSamples <= 0 {
		cfg.ParamMinSamples = defaults.ParamMinSamples
	}

	if cfg.ReplayMaxSamples <= 0 {
		cfg.ReplayMaxSamples = defaults.ReplayMaxSamples
	}

	if cfg.Comparator == nil {
		cfg.Comparator = ExactMatch{}
	}

	if cfg.NewID == nil {
		cfg.NewID = newULID
	}

	return &Engine{source: source, cfg: cfg}
}

// Scan runs every detector over the retained run history and returns the
// suggestions that cleared the savings floor, highest estimated saving
// first. Scan only reads; persisting and replaying are separate, explicit
// steps so nothing here spends tokens or writes rows.
func (e *Engine) Scan(ctx context.Context) ([]*store.Suggestion, error) {
	corpus, err := e.loadCorpus(ctx)
	if err != nil {
		return nil, err
	}

	var suggestions []*store.Suggestion

	suggestions = append(suggestions, e.detectCaching(corpus)...)
	suggestions = append(suggestions, e.detectBatching(corpus)...)
	suggestions = append(suggestions, e.detectParamTuning(corpus)...)

	suggestions = e.applyFloor(suggestions)

	slices.SortFunc(suggestions, compareSuggestions)

	for _, suggestion := range suggestions {
		suggestion.ID = e.cfg.NewID()
		suggestion.Status = store.SuggestionOpen
	}

	return suggestions, nil
}

// loadCorpus reads every retained run (bounded by cfg.Since) with its action
// rows. Results load run by run, matching the cost view's read path: the
// store has no joined reporting query yet and the run list is already
// bounded by retention.
func (e *Engine) loadCorpus(ctx context.Context) ([]*runRecord, error) {
	runs, err := e.source.ListRuns(ctx, store.ListFilter{})
	if err != nil {
		return nil, fmt.Errorf("tuning scan: list runs: %w", err)
	}

	corpus := make([]*runRecord, 0, len(runs))

	for _, run := range runs {
		if !e.cfg.Since.IsZero() && run.CreatedAt.Before(e.cfg.Since) {
			continue
		}

		results, listErr := e.source.ListActionResults(ctx, run.ID)
		if listErr != nil {
			return nil, fmt.Errorf("tuning scan: list action results for run %s: %w", run.ID, listErr)
		}

		corpus = append(corpus, &runRecord{run: run, results: results})
	}

	return corpus, nil
}

// applyFloor drops suggestions whose estimated saving misses the configured
// floor, the spec's "estimated savings must clear a configurable floor"
// guardrail.
func (e *Engine) applyFloor(suggestions []*store.Suggestion) []*store.Suggestion {
	kept := make([]*store.Suggestion, 0, len(suggestions))

	for _, suggestion := range suggestions {
		if suggestion.EstSavings.CostUSDPerRun < e.cfg.MinSavingsUSD {
			continue
		}

		kept = append(kept, suggestion)
	}

	return kept
}

// compareSuggestions orders by estimated saving (largest first), then target
// and kind so equal savings list stably.
func compareSuggestions(left, right *store.Suggestion) int {
	switch {
	case left.EstSavings.CostUSDPerRun > right.EstSavings.CostUSDPerRun:
		return -1
	case left.EstSavings.CostUSDPerRun < right.EstSavings.CostUSDPerRun:
		return 1
	}

	if byTarget := strings.Compare(left.Target, right.Target); byTarget != 0 {
		return byTarget
	}

	return strings.Compare(string(left.Kind), string(right.Kind))
}

// newULID is the default suggestion ID minter: time-sortable and
// coordination-free, matching the run store's IDs.
func newULID() string {
	return ulid.Make().String()
}

// canonicalJSON renders a value in its canonical JSON form: encoding/json
// sorts map keys, so equal values always encode to equal bytes. The false
// return marks values that cannot encode (non-JSON outputs), which the
// detectors skip rather than guess about.
func canonicalJSON(value any) (string, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", false
	}

	return string(encoded), true
}

// shortHash fingerprints a canonical string for evidence display.
func shortHash(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))

	// 8 bytes of a SHA-256 is plenty for a display fingerprint; this is
	// evidence labeling, not a security boundary.
	return hex.EncodeToString(sum[:8])
}

// percentile bases for the output-token distribution evidence.
const (
	percentileMedian = 50
	percentileTail   = 95
	percentBase      = 100
)

// percentileOf returns the pct-th percentile of values using the
// nearest-rank method on a sorted copy.
func percentileOf(values []int64, pct int) int64 {
	if len(values) == 0 {
		return 0
	}

	sorted := slices.Clone(values)
	slices.Sort(sorted)

	rank := max((len(sorted)*pct+percentBase-1)/percentBase, 1)

	return sorted[rank-1]
}
