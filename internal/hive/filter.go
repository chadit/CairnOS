package hive

import (
	"fmt"
	"strconv"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/tuning"
)

// bucketWidth is the percentage band coarse savings round down into: a 23%
// saving and a 27% saving both become "20%", so the exact figure (which can
// fingerprint one node's spend) never leaves. Wider buckets leak less; this is
// a starting width, not a researched optimum, and the privacy review may
// widen it.
const bucketWidth = 10

// percentScale turns a fractional saving (0.23) into a percentage (23) before
// bucketing.
const percentScale = 100

// Transformer reduces a local suggestion to a content-free Tip by the
// allowlist (free text is dropped, not scrubbed). It maps a node-local runner
// name onto a coarse RunnerType and tags the tip with a category, both supplied
// by the node. It holds no per-suggestion state, so one value serves the node.
type Transformer struct {
	// runnerTypes maps a local runner name to its coarse type. A name absent
	// here maps to RunnerTypeOther, so a node-specific runner name never rides
	// on a tip.
	runnerTypes map[string]RunnerType

	// category tags every tip this transformer builds. It is the node's
	// declared category for the suggestion's workflow, from the fixed set.
	category Category
}

// NewTransformer builds a transformer for one category. An unrecognized
// category is rejected here rather than at transform time, so a misconfigured
// node fails before it ever shapes a tip.
func NewTransformer(category Category) (*Transformer, error) {
	if !ValidCategory(category) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidCategory, category)
	}

	return &Transformer{
		runnerTypes: defaultRunnerTypes(),
		category:    category,
	}, nil
}

// defaultRunnerTypes maps the built-in runner names onto coarse types. Only
// the type leaves the node; the instance's endpoint and key never do.
func defaultRunnerTypes() map[string]RunnerType {
	return map[string]RunnerType{
		"openai":    RunnerTypeOpenAI,
		"anthropic": RunnerTypeAnthropic,
		"codex":     RunnerTypeLocalCLI,
		"qwen":      RunnerTypeLocalCLI,
	}
}

// Transform reduces suggestion to a Tip built only from allowlisted fields,
// reporting ok=false otherwise: a published tip carries only allowlisted,
// content-free fields. It strips, never scrubs: Rationale, Evidence, Proposed,
// Target, and ID only derive enum and bucket values. A prompt_refactor
// suggestion always returns ok=false.
//
//nolint:gocritic // hugeParam: Suggestion is read by value so Transform cannot mutate it
func (t *Transformer) Transform(suggestion store.Suggestion) (Tip, bool) {
	if !shareableKind(suggestion.Kind) {
		return Tip{}, false
	}

	runnerType, runnerOK := t.runnerTypeOf(&suggestion)
	if !runnerOK {
		return Tip{}, false
	}

	tip := Tip{
		Kind:          suggestion.Kind,
		Category:      t.category,
		RunnerType:    runnerType,
		CostBucket:    bucketUSDSaving(&suggestion),
		LatencyBucket: bucketLatencySaving(&suggestion),
	}

	if suggestion.Kind == store.SuggestionParamTuning {
		shape, shapeOK := paramShapeOf(&suggestion)
		if !shapeOK {
			return Tip{}, false
		}

		tip.ParamShape = &shape
	}

	return tip, true
}

// shareableKind reports whether a suggestion kind can ever become a tip.
// prompt_refactor never can: it is raw prompt text, which the allowlist
// excludes outright.
func shareableKind(kind store.SuggestionKind) bool {
	switch kind {
	case store.SuggestionCaching, store.SuggestionBatching, store.SuggestionParamTuning:
		return true
	default:
		return false
	}
}

// runnerTypeOf reads the suggestion's runner name from its evidence and maps it
// to a coarse type. A missing runner name fails the transform: a tip with no
// runner type is not useful, and guessing one would be wrong. An unrecognized
// name maps to RunnerTypeOther rather than failing, since the type is coarse by
// design.
func (t *Transformer) runnerTypeOf(suggestion *store.Suggestion) (RunnerType, bool) {
	name, ok := suggestion.Evidence[tuning.EvidenceRunner].(string)
	if !ok || name == "" {
		return "", false
	}

	mapped, known := t.runnerTypes[name]
	if !known {
		return RunnerTypeOther, true
	}

	return mapped, true
}

// bucketUSDSaving turns the suggestion's estimated cost saving into a coarse
// percentage band relative to a one-dollar reference, so the exact spend never
// leaves. The cost saving is an absolute dollar figure, not a ratio, so the
// reference keeps the bucket comparable across nodes without carrying the raw
// number. A zero or negative saving buckets to "0%".
func bucketUSDSaving(suggestion *store.Suggestion) string {
	const costReferenceUSD = 1.0

	ratio := suggestion.EstSavings.CostUSDPerRun / costReferenceUSD

	return bucketPercent(ratio)
}

// bucketLatencySaving turns the suggestion's estimated latency delta into a
// coarse band relative to a one-second reference. Like the cost bucket, this
// keeps the figure comparable without carrying the node's exact latency.
func bucketLatencySaving(suggestion *store.Suggestion) string {
	const latencyReference = float64(1_000_000_000) // one second in nanoseconds

	ratio := float64(suggestion.EstSavings.LatencyDelta) / latencyReference

	return bucketPercent(ratio)
}

// bucketPercent rounds a fractional saving down to a bucketWidth band and
// renders it as a percentage string. Rounding down (never up) keeps a tip from
// overstating a saving, and the wide band keeps the exact figure off the wire.
func bucketPercent(ratio float64) string {
	if ratio <= 0 {
		return "0%"
	}

	percent := int(ratio * percentScale)

	// Floor to the bucket: 23 -> 20, 27 -> 20, 31 -> 30.
	bucketed := (percent / bucketWidth) * bucketWidth

	return strconv.Itoa(bucketed) + "%"
}

// paramShapeOf derives the direction of a param-tuning change without copying
// any value. The tuning detector only ever proposes a tighter max_tokens, so
// the shape is "lowered max_tokens"; an unrecognized proposed change fails the
// transform rather than guessing a direction.
func paramShapeOf(suggestion *store.Suggestion) (ParamShape, bool) {
	if _, ok := suggestion.Proposed[tuning.ProposedMaxTokens]; ok {
		return ParamShape{Param: tuning.ProposedMaxTokens, Direction: DirectionLowered}, true
	}

	return ParamShape{}, false
}
