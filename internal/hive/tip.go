// Package hive is the Phase 6 Hive client and privacy filter. Nodes subscribe
// to content-free tips and, once a documented privacy review passes (R2) and a
// host commits to running the service (R3), publish their own. This package
// ships the mechanism: the allowlist transform that strips a local suggestion
// down to a shareable Tip, an HTTP client over a hive API, and the ingest path
// that lands received tips in the standard review queue. Publish is gated;
// subscribe is always safe.
//
// The hard rule that runs through every type here: only allowlisted,
// content-free fields leave the node. Raw prompt text, run inputs and outputs,
// API keys, endpoints, file paths, and identities never do. The transform
// drops anything off the allowlist rather than scrubbing it, because scrubbing
// leaks.
package hive

import "github.com/chadit/CairnOS/internal/store"

// Category is a coarse, content-free tag from a fixed set. A node opts in per
// category, and only tips tagged with an opted-in category are published or
// received. The set is fixed on purpose: a free-text category would be a
// channel for content to leak, so an unrecognized tag is dropped, not passed
// through.
type Category string

// The fixed category set. Adding a category is a deliberate code change, not a
// value a workflow can inject.
const (
	CategoryJobSearch  Category = "job-search"
	CategoryCodeGen    Category = "code-gen"
	CategoryResearch   Category = "research"
	CategoryAutomation Category = "automation"
	CategoryGeneral    Category = "general"
)

// validCategories is the membership set the transform and the config validator
// check a category against. A category not in this set never rides on a tip.
var validCategories = map[Category]struct{}{ //nolint:gochecknoglobals // a fixed lookup set, the content-free counterpart to an enum; it carries no state
	CategoryJobSearch:  {},
	CategoryCodeGen:    {},
	CategoryResearch:   {},
	CategoryAutomation: {},
	CategoryGeneral:    {},
}

// ValidCategory reports whether tag is one of the fixed, shareable categories.
func ValidCategory(tag Category) bool {
	_, ok := validCategories[tag]

	return ok
}

// RunnerType is the coarse runner kind a tip names, an enum rather than a
// runner instance name ("Runner type (e.g. 'openai')" is shareable; the
// instance's configured endpoint and key are not). The transform maps a local
// runner name onto one of these or refuses the tip.
type RunnerType string

// The recognized runner types. An unrecognized local runner maps to
// RunnerTypeOther so a tip never carries a node-specific runner name.
const (
	RunnerTypeOpenAI    RunnerType = "openai"
	RunnerTypeAnthropic RunnerType = "anthropic"
	RunnerTypeLocalCLI  RunnerType = "local_cli"
	RunnerTypeOther     RunnerType = "other"
)

// ParamShape describes the direction of a parameter change without any value
// tied to content. It records which parameter moved and which way, never the
// new value: "lowered max_tokens" is shareable, "max_tokens = 512" is not,
// because the value can be a fingerprint of one node's prompt.
type ParamShape struct {
	// Param is the parameter name that changed (e.g. "max_tokens",
	// "temperature"). Parameter names are part of the runner's public schema,
	// not content.
	Param string `json:"param"`

	// Direction is "lowered", "raised", or "enabled": the shape of the change,
	// never the magnitude.
	Direction string `json:"direction"`
}

// Param-change directions. The transform emits only these; a numeric value
// never reaches a tip.
const (
	DirectionLowered = "lowered"
	DirectionRaised  = "raised"
	DirectionEnabled = "enabled"
)

// A tip says "for openai actions with stable outputs, lowering max_tokens cut
// cost into the ~20% bucket", and nothing more.

// Tip is a content-free recommendation built only from allowlisted fields.
// Every field is an enum, a coarse bucket, or a structured shape with no value
// tied to content. There is deliberately no free-text field, no params map, no
// id, and no target. The JSON tags are the wire contract with the hive API.
type Tip struct {
	// ParamShape carries the direction of a param change for param_tuning tips;
	// nil for output-neutral kinds that change no parameter.
	ParamShape *ParamShape `json:"param_shape,omitempty"`

	// Kind is the suggestion kind enum (caching, batching, param_tuning). The
	// hive never carries prompt_refactor: that kind stays node-local because
	// its content cannot be reduced to a safe shape.
	Kind store.SuggestionKind `json:"kind"`

	// Category is the opt-in tag from the fixed set.
	Category Category `json:"category"`

	// RunnerType is the coarse runner kind, an enum, never an instance name.
	RunnerType RunnerType `json:"runner_type"`

	// CostBucket is the coarse cost-saving band (e.g. "20%"): the percentage
	// rounded to a wide bucket so the exact figure, which could fingerprint one
	// node's spend, never leaves.
	CostBucket string `json:"cost_bucket"`

	// LatencyBucket is the coarse latency-saving band, bucketed the same way.
	LatencyBucket string `json:"latency_bucket"`
}
