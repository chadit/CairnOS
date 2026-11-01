package tuning

import (
	"context"
	"fmt"
	"maps"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Comparator decides whether a replayed output is equivalent to the recorded
// one. It is the R1 seam: how to judge equivalence is still open, so the method
// is swappable. ExactMatch is the v1 default and EmbeddingComparator drops in
// here. Compare takes a context and may fail because a non-trivial comparator
// calls a model over the network.
type Comparator interface {
	// Compare reports equivalence in a Comparison. A returned error means the
	// comparison could not be run at all (an embedder was unreachable), which
	// the caller treats differently from a clean "not equivalent": it leaves
	// the suggestion unjudged rather than downgrading it on an infrastructure
	// blip.
	Compare(ctx context.Context, recorded, replayed any) (Comparison, error)
}

// Comparison is one comparator verdict. Reason is filled when the outputs
// differ, fit for the suggestion's evidence. Score and Threshold carry the
// calibration signal for a comparator that produces a similarity number (the
// embedding comparator); Scored gates whether they mean anything, since
// exact-match has no score to report.
type Comparison struct {
	Reason     string
	Score      float64
	Threshold  float64
	Equivalent bool
	Scored     bool
}

// ExactMatch is the v1 R1-placeholder comparator: outputs are equivalent only
// when their canonical JSON encodings are byte-identical. It never lets a
// changed output through, at the cost of calling harmless rephrasing a
// divergence. That trade is deliberate: output-changing suggestions are never
// auto-applied, so a false divergence costs a downgrade, not quality.
type ExactMatch struct{}

// reasonPreviewLen bounds how much of each output lands in a divergence
// reason: enough to see what changed, bounded for evidence rows.
const reasonPreviewLen = 80

// Compare implements Comparator by canonical JSON equality. It never calls
// out and never sets a score; the only way it errors is an already-canceled
// context, which it honors so a canceled replay stops here too.
func (ExactMatch) Compare(ctx context.Context, recorded, replayed any) (Comparison, error) {
	if err := ctx.Err(); err != nil {
		return Comparison{}, fmt.Errorf("exact-match compare canceled: %w", err)
	}

	recordedJSON, recordedOK := canonicalJSON(recorded)
	replayedJSON, replayedOK := canonicalJSON(replayed)

	if !recordedOK || !replayedOK {
		return Comparison{Reason: "output could not be canonically encoded for comparison"}, nil
	}

	if recordedJSON == replayedJSON {
		return Comparison{Equivalent: true}, nil
	}

	return Comparison{
		Reason: fmt.Sprintf("outputs differ under exact-match comparison: recorded %s, replayed %s",
			previewText(recordedJSON), previewText(replayedJSON)),
	}, nil
}

// Sample is one recorded execution to replay: the params the action ran
// with (reconstructed by the caller, since params are not persisted), the
// run-level inputs, and the output the original call produced.
type Sample struct {
	Params         map[string]any
	Inputs         map[string]any
	RecordedOutput any
}

// OutputChanging reports whether a suggestion kind can change action output
// and therefore needs the replay guardrail. Caching and batching are
// output-neutral; param tuning is not, and prompt refactoring would join it
// here if it ever ships.
func OutputChanging(kind store.SuggestionKind) bool {
	return kind == store.SuggestionParamTuning
}

// Replay re-executes the suggestion's runner with the proposed params over
// each sample and compares outputs through the Comparator. One divergent
// sample downgrades the suggestion to low confidence; matches are recorded
// too, so reviewers see the guardrail ran.
//
// It is not free: each sample re-invokes the real runner for real money, so
// it runs only behind the CLI's --replay flag.
func (e *Engine) Replay(ctx context.Context, suggestion *store.Suggestion, samples []Sample) error {
	if !OutputChanging(suggestion.Kind) {
		return fmt.Errorf("%w: %s is output-neutral", ErrReplayNotApplicable, suggestion.Kind)
	}

	if e.cfg.Runners == nil {
		return ErrReplayNoRunnerSource
	}

	if len(samples) == 0 {
		markReplay(suggestion, ReplaySkipped, "no recorded samples were available to replay", 0)

		return nil
	}

	runnerName, hasName := suggestion.Evidence[EvidenceRunner].(string)
	if !hasName || runnerName == "" {
		return fmt.Errorf("%w: suggestion %s", ErrReplayRunnerMissing, suggestion.ID)
	}

	target, exists := e.cfg.Runners.Get(runnerName)
	if !exists {
		return fmt.Errorf("%w: %q", ErrReplayRunnerUnknown, runnerName)
	}

	if len(samples) > e.cfg.ReplayMaxSamples {
		samples = samples[:e.cfg.ReplayMaxSamples]
	}

	var (
		scores    []float64
		threshold float64
	)

	for idx := range samples {
		cmp, err := e.replayOne(ctx, target, suggestion, &samples[idx])
		if err != nil {
			return err
		}

		if cmp.Scored {
			scores = append(scores, cmp.Score)
			threshold = cmp.Threshold
		}

		if !cmp.Equivalent {
			recordSimilarity(suggestion, scores, threshold)
			downgrade(suggestion, cmp.Reason, len(samples))

			return nil
		}
	}

	recordSimilarity(suggestion, scores, threshold)
	markReplay(suggestion, ReplayMatch, "", len(samples))

	return nil
}

// replayOne executes one sample under the proposed params and compares. A
// runner failure counts as divergence, not an error: the guardrail cannot
// prove equivalence, so the conservative answer is not-equivalent. A
// comparator failure instead means the guardrail could not run at all, so it
// propagates and leaves the suggestion unjudged. Cancellation propagates
// either way.
func (e *Engine) replayOne(ctx context.Context, target runner.Runner, suggestion *store.Suggestion, sample *Sample) (Comparison, error) {
	merged := maps.Clone(sample.Params)
	if merged == nil {
		merged = map[string]any{}
	}

	maps.Copy(merged, suggestion.Proposed)

	task := &runner.Task{
		ActionName: actionLabel(suggestion),
		Params:     merged,
		Inputs:     sample.Inputs,
		Resolved:   true,
	}

	result, err := target.Execute(ctx, task)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Comparison{}, fmt.Errorf("replay canceled: %w", ctxErr)
		}

		return Comparison{Reason: fmt.Sprintf("replay execution failed: %v", err)}, nil
	}

	cmp, err := e.cfg.Comparator.Compare(ctx, sample.RecordedOutput, result.Output)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Comparison{}, fmt.Errorf("replay canceled: %w", ctxErr)
		}

		return Comparison{}, fmt.Errorf("compare outputs: %w", err)
	}

	return cmp, nil
}

// recordSimilarity stamps the per-sample cosine scores and their threshold
// into the suggestion's evidence. That is the R1 calibration record: an
// operator sees the similarity numbers next to the guardrail verdict, the data
// needed to tune the threshold on a real corpus. Exact-match produces no
// scores, so the call is a no-op there.
func recordSimilarity(suggestion *store.Suggestion, scores []float64, threshold float64) {
	if len(scores) == 0 {
		return
	}

	if suggestion.Evidence == nil {
		suggestion.Evidence = map[string]any{}
	}

	suggestion.Evidence[EvidenceReplaySimilarity] = scores
	suggestion.Evidence[EvidenceReplayThreshold] = threshold
}

// downgrade records a failed replay on the suggestion: confidence drops to
// low (param tuning already sits there; the marker still lands) and the
// reason rides in the evidence so the review queue shows why.
func downgrade(suggestion *store.Suggestion, reason string, sampleCount int) {
	suggestion.EstSavings.Confidence = store.ConfidenceLow
	suggestion.Rationale += " Replay against recorded inputs diverged, so this suggestion is held at low confidence: " + reason

	markReplay(suggestion, ReplayDiverged, reason, sampleCount)
}

// markReplay stamps the replay outcome into the suggestion's evidence.
func markReplay(suggestion *store.Suggestion, outcome, reason string, sampleCount int) {
	if suggestion.Evidence == nil {
		suggestion.Evidence = map[string]any{}
	}

	suggestion.Evidence[EvidenceReplayOutcome] = outcome
	suggestion.Evidence[EvidenceReplaySamples] = sampleCount

	if reason != "" {
		suggestion.Evidence[EvidenceReplayReason] = reason
	}
}

// actionLabel pulls the action name from the evidence for the replay task,
// falling back to the target so the runner always sees a label.
func actionLabel(suggestion *store.Suggestion) string {
	if action, hasAction := suggestion.Evidence[EvidenceAction].(string); hasAction && action != "" {
		return action
	}

	return suggestion.Target
}

// previewText caps text for divergence reasons.
func previewText(text string) string {
	if len(text) <= reasonPreviewLen {
		return text
	}

	return text[:reasonPreviewLen] + "..."
}
