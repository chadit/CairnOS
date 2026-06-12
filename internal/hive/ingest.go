package hive

import (
	"context"
	"fmt"

	"github.com/chadit/CairnOS/internal/store"
)

// EvidenceSource marks a suggestion's origin in its evidence; SourceHive
// distinguishes a tip that arrived from the network from one local analysis
// produced (.agents/spec/09-a2a-and-hive.md: "A received tip is just a
// Suggestion that arrived from the network"). The marker is for the reviewer,
// not a bypass: a hive suggestion goes through the same review queue and the
// same replay guardrail as a local one.
const (
	EvidenceSource = "source"
	SourceHive     = "hive"
)

// Evidence keys a hive suggestion carries beyond the source marker. They hold
// only the tip's content-free fields, so a received suggestion never claims a
// node-specific runner name or an exact saving it cannot have.
const (
	EvidenceRunnerType    = "runner_type"
	EvidenceCategory      = "category"
	EvidenceCostBucket    = "cost_bucket"
	EvidenceLatencyBucket = "latency_bucket"
	EvidenceParamShape    = "param_shape"
)

// Ingester lands received tips in the standard suggestion queue. It holds the
// narrow SuggestionStore slice, not all of Store, because ingesting only ever
// writes suggestions.
type Ingester struct {
	store store.SuggestionStore
	newID func() string
}

// NewIngester builds an ingester over a suggestion store and an id minter. A
// nil minter is a programming error the caller must fix, so it is rejected at
// construction rather than producing empty ids later.
func NewIngester(suggestions store.SuggestionStore, newID func() string) (*Ingester, error) {
	if newID == nil {
		return nil, errNilIDMinter
	}

	return &Ingester{store: suggestions, newID: newID}, nil
}

// Ingest converts each tip to an open local suggestion and saves it, so a
// received tip flows through the same human review queue and replay guardrail
// as a local suggestion (.agents/spec/09-a2a-and-hive.md, acceptance: "A
// received tip flows through the standard review queue and replay guardrail").
// It never marks a suggestion applied or approved: a tip from the network is
// open and waits for a human, exactly like a local one. It returns the count
// saved.
func (i *Ingester) Ingest(ctx context.Context, tips []Tip) (int, error) {
	var saved int

	for idx := range tips {
		suggestion := tipToSuggestion(&tips[idx], i.newID())

		if err := i.store.SaveSuggestion(ctx, suggestion); err != nil {
			return saved, fmt.Errorf("ingest tip %d: %w", idx, err)
		}

		saved++
	}

	return saved, nil
}

// tipToSuggestion builds an open local suggestion from one content-free tip.
// The suggestion carries only what the tip carried plus the hive source marker:
// no raw params (the tip had none), no concrete value, and a low confidence
// regardless of the tip's claimed savings, because a tip's bucketed saving was
// measured on another node's data, not this one's. The target is the runner
// type, not a local workflow.action, since a tip names no local target.
func tipToSuggestion(tip *Tip, id string) *store.Suggestion {
	evidence := map[string]any{
		EvidenceSource:        SourceHive,
		EvidenceRunnerType:    string(tip.RunnerType),
		EvidenceCategory:      string(tip.Category),
		EvidenceCostBucket:    tip.CostBucket,
		EvidenceLatencyBucket: tip.LatencyBucket,
	}

	if tip.ParamShape != nil {
		evidence[EvidenceParamShape] = tip.ParamShape.Direction + " " + tip.ParamShape.Param
	}

	return &store.Suggestion{
		ID:        id,
		Kind:      tip.Kind,
		Target:    string(tip.RunnerType),
		Rationale: hiveRationale(tip),
		Evidence:  evidence,
		// No Proposed map: a tip carries no concrete change, only a shape, so
		// applying one is never automatic. A human decides where (and whether)
		// it applies on this node.
		EstSavings: store.Savings{
			// A received tip's saving was measured elsewhere, so it is held at
			// low confidence on this node regardless of the bucket it claimed.
			Confidence: store.ConfidenceLow,
		},
		Status: store.SuggestionOpen,
	}
}

// hiveRationale writes the reviewer-facing summary of a received tip, naming
// only its content-free fields so the rationale itself carries no content.
func hiveRationale(tip *Tip) string {
	if tip.ParamShape != nil {
		return fmt.Sprintf(
			"hive tip (%s): for %s actions, %s %s cut cost into the %s bucket on other nodes. Review whether it fits a workflow here before applying.",
			tip.Category, tip.RunnerType, tip.ParamShape.Direction, tip.ParamShape.Param, tip.CostBucket,
		)
	}

	return fmt.Sprintf(
		"hive tip (%s): for %s actions, %s cut cost into the %s bucket on other nodes. Review whether it fits a workflow here before applying.",
		tip.Category, tip.RunnerType, tip.Kind, tip.CostBucket,
	)
}
