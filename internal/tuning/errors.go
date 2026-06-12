package tuning

import "errors"

// Sentinel errors for the tuning engine.
var (
	// ErrReplayNotApplicable reports a Replay call against a suggestion kind
	// that does not change output. Caching and batching are output-neutral by
	// construction, so replaying them would spend runner tokens to prove
	// nothing.
	ErrReplayNotApplicable = errors.New("replay applies only to output-changing suggestion kinds")

	// ErrReplayNoRunnerSource reports a Replay call on an engine built
	// without a runner source. Replay re-invokes real runners, so the caller
	// must wire one explicitly (the CLI does this only under --replay).
	ErrReplayNoRunnerSource = errors.New("replay requires a runner source")

	// ErrReplayRunnerMissing reports a suggestion whose evidence carries no
	// runner name, leaving Replay nothing to re-invoke.
	ErrReplayRunnerMissing = errors.New("suggestion evidence carries no runner name")

	// ErrReplayRunnerUnknown reports a runner named in the suggestion's
	// evidence that the runner source does not know.
	ErrReplayRunnerUnknown = errors.New("replay runner is not registered")

	// ErrEmbeddingDimMismatch reports two embedding vectors of different
	// lengths, which the cosine comparator cannot compare. It points at a
	// misconfigured embedder (two models, or a truncated vector).
	ErrEmbeddingDimMismatch = errors.New("embedding vectors have different dimensions")

	// ErrEmbeddingEmpty reports an empty embedding vector, leaving nothing to
	// compare.
	ErrEmbeddingEmpty = errors.New("embedding vector is empty")
)
