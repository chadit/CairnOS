package tuning

import (
	"context"
	"fmt"
	"math"
)

// Embedder turns text into a vector. It is the seam the embedding comparator
// depends on, so any embedding source (an OpenAI embeddings client, a local
// model) plugs in without this package importing a runner. Embed runs once per
// output per replay sample, so an implementation should reuse one HTTP client
// rather than dialing per call.
type Embedder interface {
	// Embed returns the embedding vector for text. Two calls must return
	// equal-length vectors, or the comparator cannot compare them.
	Embed(ctx context.Context, text string) ([]float64, error)
}

// defaultEquivalenceThreshold is the conservative starting cosine threshold:
// high enough that only near-paraphrases pass, because a false "equivalent" is
// the dangerous direction (it could let a real quality regression through). It
// is a starting point to calibrate from, not a validated constant (R1).
const defaultEquivalenceThreshold = 0.95

// EmbeddingComparator judges equivalence by the cosine similarity of the two
// outputs' embeddings: at or above Threshold is equivalent. It is the R1
// answer for free-text outputs that exact match flags as divergent on harmless
// rephrasing (.agents/spec/14-open-decisions.md, R1). The threshold is a config
// knob, not a constant, because the right value depends on the embedding model
// and the workflow's domain; the calibration scores recorded in the suggestion
// evidence are there to set it against a real corpus.
type EmbeddingComparator struct {
	// embedder produces the vectors.
	embedder Embedder

	// threshold is the cosine similarity at or above which two outputs count
	// as equivalent.
	threshold float64
}

// NewEmbeddingComparator builds an embedding comparator. A threshold outside
// (0, 1] falls back to defaultEquivalenceThreshold, so a misconfigured knob
// fails safe (stricter) rather than letting everything pass.
func NewEmbeddingComparator(embedder Embedder, threshold float64) *EmbeddingComparator {
	if threshold <= 0 || threshold > 1 {
		threshold = defaultEquivalenceThreshold
	}

	return &EmbeddingComparator{embedder: embedder, threshold: threshold}
}

// Compare implements Comparator by cosine similarity of the two outputs'
// embeddings. An output that cannot be rendered to text is a clean
// non-equivalence (the conservative answer); an embedder failure is an error,
// since the guardrail could not run, which the caller treats differently from
// a divergence.
func (c *EmbeddingComparator) Compare(ctx context.Context, recorded, replayed any) (Comparison, error) {
	recordedText, recordedOK := textForEmbedding(recorded)
	replayedText, replayedOK := textForEmbedding(replayed)

	if !recordedOK || !replayedOK {
		return Comparison{Reason: "output could not be rendered to text for embedding comparison"}, nil
	}

	recordedVec, err := c.embedder.Embed(ctx, recordedText)
	if err != nil {
		return Comparison{}, fmt.Errorf("embed recorded output: %w", err)
	}

	replayedVec, err := c.embedder.Embed(ctx, replayedText)
	if err != nil {
		return Comparison{}, fmt.Errorf("embed replayed output: %w", err)
	}

	similarity, err := cosineSimilarity(recordedVec, replayedVec)
	if err != nil {
		return Comparison{}, fmt.Errorf("compare embeddings: %w", err)
	}

	cmp := Comparison{Score: similarity, Threshold: c.threshold, Scored: true}

	if similarity >= c.threshold {
		cmp.Equivalent = true

		return cmp, nil
	}

	cmp.Reason = fmt.Sprintf("embedding cosine similarity %.4f is below the %.4f equivalence threshold",
		similarity, c.threshold)

	return cmp, nil
}

// textForEmbedding renders an output to the text an embedder sees. A string
// output embeds as-is, the natural case for a prose runner; anything else
// embeds as its canonical JSON so structured outputs still compare. The false
// return marks a value that cannot be encoded at all.
func textForEmbedding(value any) (string, bool) {
	if text, isString := value.(string); isString {
		return text, true
	}

	return canonicalJSON(value)
}

// cosineSimilarity returns the cosine of the angle between two equal-length
// vectors, in [-1, 1]. Mismatched lengths are an error (two embedding models or
// a truncated vector); an empty vector is an error too. A zero-magnitude vector
// yields 0, since the angle is undefined and "no similarity" is the safe read.
func cosineSimilarity(vecA, vecB []float64) (float64, error) {
	if len(vecA) != len(vecB) {
		return 0, fmt.Errorf("%w: %d vs %d", ErrEmbeddingDimMismatch, len(vecA), len(vecB))
	}

	if len(vecA) == 0 {
		return 0, ErrEmbeddingEmpty
	}

	var dot, normA, normB float64

	for idx := range vecA {
		dot += vecA[idx] * vecB[idx]
		normA += vecA[idx] * vecA[idx]
		normB += vecB[idx] * vecB[idx]
	}

	if normA == 0 || normB == 0 {
		return 0, nil
	}

	return dot / (math.Sqrt(normA) * math.Sqrt(normB)), nil
}
