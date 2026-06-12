package tuning

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// proposedCacheTTL is the starter TTL every caching suggestion proposes. It
// is a talking point for the reviewer, not a measured value: the right TTL
// depends on how stale the cached answer may go.
const proposedCacheTTL = "1h"

// Proposed-change keys for caching suggestions.
const (
	ProposedCache    = "cache"
	ProposedCacheTTL = "cache_ttl"
)

// cacheGroup accumulates the repeats of one (runner, workflow hash, action,
// run inputs) signature.
type cacheGroup struct {
	workflow      string
	action        string
	runnerName    string
	signature     string
	count         int
	totalCost     float64
	totalLatency  time.Duration
	firstOutput   string
	outputsDiffer bool
}

// detectCaching finds actions that ran with the same effective parameters
// and produced the same output at least CacheMinRepeats times. Action
// params are not persisted, so "identical params" is read as identical
// (runner, workflow hash, action name, run inputs): the same definition fed
// the same inputs. The identical-output requirement then proves the call
// was repeat work, which is exactly what a result cache removes.
func (e *Engine) detectCaching(corpus []*runRecord) []*store.Suggestion {
	groups := make(map[string]*cacheGroup, len(corpus))

	for _, record := range corpus {
		inputsJSON, inputsOK := canonicalJSON(record.run.Inputs)
		if !inputsOK {
			continue
		}

		for _, result := range record.results {
			if result.Status != store.ActionSucceeded || result.Output == nil {
				continue
			}

			outputJSON, outputOK := canonicalJSON(result.Output)
			if !outputOK {
				continue
			}

			signature := result.Runner + "|" + record.run.WorkflowHash + "|" + result.ActionName + "|" + inputsJSON
			key := record.run.WorkflowName + "|" + signature

			group, exists := groups[key]
			if !exists {
				group = &cacheGroup{
					workflow:    record.run.WorkflowName,
					action:      result.ActionName,
					runnerName:  result.Runner,
					signature:   signature,
					firstOutput: outputJSON,
				}
				groups[key] = group
			}

			group.count++
			group.totalCost += result.Usage.CostUSD
			group.totalLatency += result.Usage.Latency

			if outputJSON != group.firstOutput {
				group.outputsDiffer = true
			}
		}
	}

	var suggestions []*store.Suggestion

	for _, key := range slices.Sorted(maps.Keys(groups)) {
		group := groups[key]
		if group.count < e.cfg.CacheMinRepeats || group.outputsDiffer {
			continue
		}

		suggestions = append(suggestions, cacheSuggestion(group))
	}

	return suggestions
}

// cacheSuggestion renders one qualifying repeat group as a suggestion. The
// per-run saving assumes every repeat after the first becomes a cache hit,
// spread across the observed repeats.
func cacheSuggestion(group *cacheGroup) *store.Suggestion {
	repeats := float64(group.count)
	costPerRepeat := group.totalCost / repeats
	avoidable := (repeats - 1) / repeats

	return &store.Suggestion{
		Kind:   store.SuggestionCaching,
		Target: group.workflow + "." + group.action,
		Rationale: fmt.Sprintf(
			"action %q in workflow %q ran %d times with identical inputs and returned the identical output every time; each repeat cost about $%.4f. Caching the result with a TTL turns every repeat after the first into a cache hit.",
			group.action, group.workflow, group.count, costPerRepeat,
		),
		Evidence: map[string]any{
			EvidenceWorkflow:      group.workflow,
			EvidenceAction:        group.action,
			EvidenceRunner:        group.runnerName,
			EvidenceRepeatCount:   group.count,
			EvidenceCostPerRepeat: costPerRepeat,
			EvidenceParamsHash:    shortHash(group.signature),
		},
		Proposed: map[string]any{
			ProposedCache:    true,
			ProposedCacheTTL: proposedCacheTTL,
		},
		EstSavings: store.Savings{
			CostUSDPerRun: costPerRepeat * avoidable,
			LatencyDelta:  time.Duration(float64(group.totalLatency) / repeats * avoidable),
			// A cache hit is verifiable and output-neutral, the spec's reason
			// caching leads the suggestion kinds.
			Confidence: store.ConfidenceHigh,
		},
	}
}
