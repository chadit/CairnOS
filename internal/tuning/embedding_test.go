package tuning_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/tuning"
	"github.com/chadit/CairnOS/pkg/runner"
)

// errEmbed is the failure the fake embedder returns on demand.
var errEmbed = errors.New("embedder unreachable")

// oldAnswer is the recorded output the replay tests compare a fresh run against.
const oldAnswer = "old answer"

// fakeEmbedder maps an exact output text to a fixed vector, so a test controls
// the cosine similarity by choosing which vectors two outputs map to. An
// unmapped text returns the zero vector, and a set err fails every call.
type fakeEmbedder struct {
	vectors map[string][]float64
	err     error
}

// Embed implements tuning.Embedder.
func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	if f.err != nil {
		return nil, f.err
	}

	if vec, ok := f.vectors[text]; ok {
		return vec, nil
	}

	return []float64{0, 0, 0}, nil
}

// embeddingComparatorFor builds a comparator over a fake embedder seeded with
// the given recorded/replayed text-to-vector mapping.
func embeddingComparatorFor(threshold float64, vectors map[string][]float64) *tuning.EmbeddingComparator {
	return tuning.NewEmbeddingComparator(&fakeEmbedder{vectors: vectors}, threshold)
}

func TestEmbeddingComparatorEquivalentAboveThreshold(t *testing.T) {
	t.Parallel()

	// Identical vectors give cosine 1.0, which clears any sane threshold.
	comparator := embeddingComparatorFor(0.9, map[string][]float64{
		"old wording": {1, 2, 3},
		"new wording": {1, 2, 3},
	})

	cmp, err := comparator.Compare(t.Context(), "old wording", "new wording")
	if err != nil {
		t.Fatalf("compare: %v", err)
	}

	if !cmp.Equivalent {
		t.Fatalf("identical-embedding outputs should be equivalent: %+v", cmp)
	}

	if !cmp.Scored || cmp.Score < 0.999 {
		t.Fatalf("score = %v (scored=%v), want ~1.0", cmp.Score, cmp.Scored)
	}

	if cmp.Threshold != 0.9 {
		t.Fatalf("threshold = %v, want 0.9", cmp.Threshold)
	}
}

func TestEmbeddingComparatorDivergesBelowThreshold(t *testing.T) {
	t.Parallel()

	// Orthogonal vectors give cosine 0, below the threshold.
	comparator := embeddingComparatorFor(0.9, map[string][]float64{
		"the cat sat":       {1, 0, 0},
		"a stock ticker up": {0, 1, 0},
	})

	cmp, err := comparator.Compare(t.Context(), "the cat sat", "a stock ticker up")
	if err != nil {
		t.Fatalf("compare: %v", err)
	}

	if cmp.Equivalent {
		t.Fatal("orthogonal-embedding outputs should not be equivalent")
	}

	if !strings.Contains(cmp.Reason, "below") {
		t.Fatalf("reason = %q, want it to mention the threshold", cmp.Reason)
	}

	if !cmp.Scored {
		t.Fatal("a divergent embedding comparison still carries its score")
	}
}

func TestEmbeddingComparatorEmbedderFailureIsAnError(t *testing.T) {
	t.Parallel()

	// An embedder failure is not a clean divergence: the guardrail could not
	// run, so Compare errors and leaves the suggestion unjudged.
	comparator := tuning.NewEmbeddingComparator(&fakeEmbedder{err: errEmbed}, 0.9)

	_, err := comparator.Compare(t.Context(), "a", "b")
	if !errors.Is(err, errEmbed) {
		t.Fatalf("compare err = %v, want the embedder failure surfaced", err)
	}
}

func TestEmbeddingComparatorDimensionMismatchIsAnError(t *testing.T) {
	t.Parallel()

	comparator := embeddingComparatorFor(0.9, map[string][]float64{
		"left":  {1, 0, 0},
		"right": {1, 0},
	})

	_, err := comparator.Compare(t.Context(), "left", "right")
	if !errors.Is(err, tuning.ErrEmbeddingDimMismatch) {
		t.Fatalf("compare err = %v, want ErrEmbeddingDimMismatch", err)
	}
}

func TestEmbeddingComparatorUnencodableOutputIsCleanNonEquivalence(t *testing.T) {
	t.Parallel()

	comparator := embeddingComparatorFor(0.9, nil)

	// A channel cannot be JSON-encoded, so it cannot be embedded; that is a
	// clean non-equivalence, not an error.
	cmp, err := comparator.Compare(t.Context(), make(chan int), "text")
	if err != nil {
		t.Fatalf("compare: %v", err)
	}

	if cmp.Equivalent || cmp.Scored {
		t.Fatalf("unencodable output should be a clean non-equivalence: %+v", cmp)
	}
}

func TestNewEmbeddingComparatorClampsThreshold(t *testing.T) {
	t.Parallel()

	// A threshold above 1 is impossible for cosine; it falls back to the
	// conservative default, so identical vectors (cosine 1.0) still pass.
	comparator := embeddingComparatorFor(5.0, map[string][]float64{
		"x": {1, 1, 1},
		"y": {1, 1, 1},
	})

	cmp, err := comparator.Compare(t.Context(), "x", "y")
	if err != nil {
		t.Fatalf("compare: %v", err)
	}

	if !cmp.Equivalent {
		t.Fatal("an out-of-range threshold must clamp to the default, not reject everything")
	}
}

// embeddingReplayEngine wires an engine whose comparator is the embedding one,
// reusing the scripted runner and solo source from replay_test.go.
func embeddingReplayEngine(target runner.Runner, comparator tuning.Comparator) *tuning.Engine {
	cfg := tuning.DefaultConfig()
	cfg.Runners = &soloSource{name: runnerAnthropic, target: target}
	cfg.Comparator = comparator

	return tuning.New(nil, cfg)
}

func TestReplayEmbeddingRecordsCalibrationScores(t *testing.T) {
	t.Parallel()

	// Recorded and replayed map to the same vector, so cosine is 1.0 and the
	// suggestion stands, with the score and threshold recorded for calibration.
	comparator := embeddingComparatorFor(0.9, map[string][]float64{
		oldAnswer:    {1, 0, 0},
		"new answer": {1, 0, 0},
	})
	scripted := &scriptedRunner{output: "new answer"}
	engine := embeddingReplayEngine(scripted, comparator)
	suggestion := paramSuggestionFixture(store.ConfidenceLow)

	samples := []tuning.Sample{{RecordedOutput: oldAnswer}}

	if err := engine.Replay(t.Context(), suggestion, samples); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if suggestion.Evidence[tuning.EvidenceReplayOutcome] != tuning.ReplayMatch {
		t.Fatalf("outcome = %v, want match", suggestion.Evidence[tuning.EvidenceReplayOutcome])
	}

	scores, ok := suggestion.Evidence[tuning.EvidenceReplaySimilarity].([]float64)
	if !ok || len(scores) != 1 {
		t.Fatalf("calibration scores = %v, want one recorded score", suggestion.Evidence[tuning.EvidenceReplaySimilarity])
	}

	if scores[0] < 0.999 {
		t.Fatalf("recorded score = %v, want ~1.0", scores[0])
	}

	if suggestion.Evidence[tuning.EvidenceReplayThreshold] != 0.9 {
		t.Fatalf("recorded threshold = %v, want 0.9", suggestion.Evidence[tuning.EvidenceReplayThreshold])
	}
}

func TestReplayEmbeddingDivergenceRecordsScoreAndDowngrades(t *testing.T) {
	t.Parallel()

	// Orthogonal vectors give cosine 0, below the threshold, so the suggestion
	// downgrades and the diverging score is still recorded for calibration.
	comparator := embeddingComparatorFor(0.9, map[string][]float64{
		oldAnswer:          {1, 0, 0},
		"unrelated answer": {0, 1, 0},
	})
	scripted := &scriptedRunner{output: "unrelated answer"}
	engine := embeddingReplayEngine(scripted, comparator)
	suggestion := paramSuggestionFixture(store.ConfidenceMedium)

	samples := []tuning.Sample{{RecordedOutput: oldAnswer}}

	if err := engine.Replay(t.Context(), suggestion, samples); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if suggestion.Evidence[tuning.EvidenceReplayOutcome] != tuning.ReplayDiverged {
		t.Fatalf("outcome = %v, want diverged", suggestion.Evidence[tuning.EvidenceReplayOutcome])
	}

	if suggestion.EstSavings.Confidence != store.ConfidenceLow {
		t.Fatalf("confidence = %s, want low after divergence", suggestion.EstSavings.Confidence)
	}

	scores, ok := suggestion.Evidence[tuning.EvidenceReplaySimilarity].([]float64)
	if !ok || len(scores) != 1 {
		t.Fatalf("calibration scores = %v, want the diverging score recorded", suggestion.Evidence[tuning.EvidenceReplaySimilarity])
	}
}
