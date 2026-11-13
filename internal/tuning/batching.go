package tuning

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// Proposed-change keys for batching suggestions.
const (
	ProposedBatch        = "batch"
	ProposedMaxBatchSize = "max_batch_size"
)

// burst is the densest same-runner call cluster found inside one run.
type burst struct {
	calls      int
	minCost    float64
	minLatency time.Duration
}

// batchGroup aggregates the bursts one (workflow, runner) pair showed across
// runs.
type batchGroup struct {
	workflow     string
	runnerName   string
	runsAffected int
	maxCalls     int
	overheadSum  float64
	savingsSum   float64
	latencySaved time.Duration
}

// detectBatching finds runs that fired BatchMinCalls or more calls at the
// same runner inside the sliding window: many small calls to one runner in
// a short span. The smallest call in the burst stands in for
// the fixed per-call overhead: whatever that call spent is a floor on what
// every other call in the burst also paid just to exist.
func (e *Engine) detectBatching(corpus []*runRecord) []*store.Suggestion {
	groups := make(map[string]*batchGroup, len(corpus))
	runsPerWorkflow := make(map[string]int, len(corpus))

	for _, record := range corpus {
		runsPerWorkflow[record.run.WorkflowName]++

		for runnerName, results := range callsByRunner(record.results) {
			found, hit := densestBurst(results, e.cfg.BatchWindow)
			if !hit || found.calls < e.cfg.BatchMinCalls {
				continue
			}

			key := record.run.WorkflowName + "|" + runnerName

			group, exists := groups[key]
			if !exists {
				group = &batchGroup{workflow: record.run.WorkflowName, runnerName: runnerName}
				groups[key] = group
			}

			group.runsAffected++
			group.maxCalls = max(group.maxCalls, found.calls)
			group.overheadSum += found.minCost

			avoidedCalls := found.calls - 1
			group.savingsSum += float64(avoidedCalls) * found.minCost
			group.latencySaved += time.Duration(avoidedCalls) * found.minLatency
		}
	}

	suggestions := make([]*store.Suggestion, 0, len(groups))

	for _, key := range slices.Sorted(maps.Keys(groups)) {
		group := groups[key]
		suggestions = append(suggestions, batchSuggestion(group, runsPerWorkflow[group.workflow], e.cfg.BatchWindow))
	}

	return suggestions
}

// callsByRunner buckets a run's action rows by runner, keeping only rows
// with a start time to slide the window over. Failed calls count too: their
// overhead was spent all the same.
func callsByRunner(results []*store.ActionResult) map[string][]*store.ActionResult {
	byRunner := make(map[string][]*store.ActionResult, len(results))

	for _, result := range results {
		if result.StartedAt.IsZero() {
			continue
		}

		byRunner[result.Runner] = append(byRunner[result.Runner], result)
	}

	return byRunner
}

// densestBurst slides the window over the calls sorted by start time and
// returns the densest cluster, with the cheapest and fastest call inside it
// as the overhead proxies. The false return means no calls at all.
func densestBurst(results []*store.ActionResult, window time.Duration) (burst, bool) {
	if len(results) == 0 {
		return burst{}, false
	}

	sorted := slices.Clone(results)
	slices.SortFunc(sorted, func(left, right *store.ActionResult) int {
		return left.StartedAt.Compare(right.StartedAt)
	})

	var (
		best burst
		low  int
	)

	for high := range sorted {
		for sorted[high].StartedAt.Sub(sorted[low].StartedAt) > window {
			low++
		}

		calls := high - low + 1
		if calls <= best.calls {
			continue
		}

		best = burst{calls: calls, minCost: sorted[low].Usage.CostUSD, minLatency: sorted[low].Usage.Latency}

		for _, inWindow := range sorted[low : high+1] {
			best.minCost = min(best.minCost, inWindow.Usage.CostUSD)
			best.minLatency = min(best.minLatency, inWindow.Usage.Latency)
		}
	}

	return best, true
}

// batchSuggestion renders one qualifying (workflow, runner) burst pattern.
// Savings average over every observed run of the workflow, not just the
// bursty ones, so the estimate stays honest about how often the pattern
// shows up.
func batchSuggestion(group *batchGroup, runsObserved int, window time.Duration) *store.Suggestion {
	perRunSaving := group.savingsSum / float64(runsObserved)
	perRunLatency := time.Duration(float64(group.latencySaved) / float64(runsObserved))
	avgOverhead := group.overheadSum / float64(group.runsAffected)

	return &store.Suggestion{
		Kind:   store.SuggestionBatching,
		Target: group.workflow,
		Rationale: fmt.Sprintf(
			"workflow %q made up to %d calls to runner %q within %s in a single run; the smallest of those calls cost about $%.4f, a floor on the per-call overhead. Batching them into one request pays that overhead once instead of %d times.",
			group.workflow, group.maxCalls, group.runnerName, window, avgOverhead, group.maxCalls,
		),
		Evidence: map[string]any{
			EvidenceWorkflow:     group.workflow,
			EvidenceRunner:       group.runnerName,
			EvidenceCallCount:    group.maxCalls,
			EvidenceCallOverhead: avgOverhead,
			EvidenceWindow:       window.String(),
			EvidenceRunsAffected: group.runsAffected,
			EvidenceRunsObserved: runsObserved,
		},
		Proposed: map[string]any{
			ProposedBatch:        true,
			ProposedMaxBatchSize: group.maxCalls,
		},
		EstSavings: store.Savings{
			CostUSDPerRun: perRunSaving,
			LatencyDelta:  perRunLatency,
			// Batching is output-neutral in shape but reworks the workflow's
			// call structure, so it sits a band below caching.
			Confidence: store.ConfidenceMedium,
		},
	}
}
