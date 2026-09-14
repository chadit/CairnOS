package tuning

import (
	"fmt"
	"maps"
	"slices"

	"github.com/chadit/CairnOS/internal/store"
)

// Proposed-change keys for param-tuning suggestions.
const (
	ProposedMaxTokens   = "max_tokens"
	ProposedConfigBlock = "config_block"
)

// cfgKeyMaxTokens is the [runner.<name>] config key the LLM runners read for
// their output cap; the detector reads the same key.
const cfgKeyMaxTokens = "max_tokens"

// paramHeadroom doubles the observed p95 when proposing a new cap, so an
// unusually long (but legitimate) output still fits.
const paramHeadroom = 2

// tokenRounding rounds proposed caps up to the next multiple of 64, a
// conventional granularity for token limits, and doubles as the floor so a
// proposal never lands at zero.
const tokenRounding = 64

// tokenDist groups one action's observed output-token history.
type tokenDist struct {
	workflow     string
	action       string
	runnerName   string
	outputTokens []int64
	totalCost    float64
	totalTokens  int64
}

// detectParamTuning finds actions whose runner-configured max_tokens sits far
// above the output lengths they produce. The cap lives in the [runner.<name>]
// block that workflow params cannot override, so the proposal targets that
// block and says so: the change touches every action sharing the runner.
// Tightening a cap can change output, so this kind stays low confidence behind
// the replay guardrail.
func (e *Engine) detectParamTuning(corpus []*runRecord) []*store.Suggestion {
	if len(e.cfg.RunnerConfigs) == 0 {
		return nil
	}

	groups := make(map[string]*tokenDist, len(corpus))

	for _, record := range corpus {
		for _, result := range record.results {
			usage := result.Usage
			if result.Status != store.ActionSucceeded || usage.InputTokens+usage.OutputTokens == 0 {
				continue
			}

			key := record.run.WorkflowName + "|" + result.ActionName + "|" + result.Runner

			group, exists := groups[key]
			if !exists {
				group = &tokenDist{
					workflow:   record.run.WorkflowName,
					action:     result.ActionName,
					runnerName: result.Runner,
				}
				groups[key] = group
			}

			group.outputTokens = append(group.outputTokens, usage.OutputTokens)
			group.totalCost += usage.CostUSD
			group.totalTokens += usage.InputTokens + usage.OutputTokens
		}
	}

	var suggestions []*store.Suggestion

	for _, key := range slices.Sorted(maps.Keys(groups)) {
		suggestion, qualifies := e.paramSuggestion(groups[key])
		if !qualifies {
			continue
		}

		suggestions = append(suggestions, suggestion)
	}

	return suggestions
}

// paramSuggestion checks one action's distribution against its runner's
// configured cap and renders the suggestion when the gap is wide enough.
func (e *Engine) paramSuggestion(group *tokenDist) (*store.Suggestion, bool) {
	if len(group.outputTokens) < e.cfg.ParamMinSamples || group.totalTokens == 0 {
		return nil, false
	}

	maxTokens, configured := configuredMaxTokens(e.cfg.RunnerConfigs[group.runnerName])
	if !configured {
		return nil, false
	}

	tail := percentileOf(group.outputTokens, percentileTail)
	if float64(maxTokens) < e.cfg.MaxTokensRatio*float64(tail) {
		return nil, false
	}

	proposed := roundUpTokens(tail * paramHeadroom)
	if proposed >= maxTokens {
		return nil, false
	}

	// Blended price per token from this action's own history. The saving is
	// the cap-overrun cost a tighter limit prevents per run: an estimate of
	// avoided worst-case spend, not a per-call discount, hence low
	// confidence.
	perTokenUSD := group.totalCost / float64(group.totalTokens)
	samples := len(group.outputTokens)

	return &store.Suggestion{
		Kind:   store.SuggestionParamTuning,
		Target: group.workflow + "." + group.action,
		Rationale: fmt.Sprintf(
			"action %q in workflow %q runs on %q with max_tokens %d, but its observed outputs peak near %d tokens (p95 over %d samples). A cap of %d keeps headroom while bounding worst-case spend. The change lives in the [runner.%s] config block and affects every action using that runner, and a tighter cap can change output, so this stays low confidence pending replay and review.",
			group.action, group.workflow, group.runnerName, maxTokens, tail, samples, proposed, group.runnerName,
		),
		Evidence: map[string]any{
			EvidenceWorkflow:  group.workflow,
			EvidenceAction:    group.action,
			EvidenceRunner:    group.runnerName,
			EvidenceMaxTokens: maxTokens,
			EvidenceOutputTokens: map[string]any{
				"min": slices.Min(group.outputTokens),
				"p50": percentileOf(group.outputTokens, percentileMedian),
				"p95": tail,
				"max": slices.Max(group.outputTokens),
			},
			EvidenceSamples: samples,
		},
		Proposed: map[string]any{
			ProposedMaxTokens:   proposed,
			ProposedConfigBlock: "runner." + group.runnerName,
		},
		EstSavings: store.Savings{
			CostUSDPerRun: float64(maxTokens-proposed) * perTokenUSD,
			// Output changes are the open research problem (R1): low, always.
			Confidence: store.ConfidenceLow,
		},
	}, true
}

// configuredMaxTokens reads a runner config block's max_tokens with the
// loose numeric coercion TOML decoding produces.
func configuredMaxTokens(cfg map[string]any) (int64, bool) {
	switch value := cfg[cfgKeyMaxTokens].(type) {
	case int64:
		return value, value > 0
	case int:
		return int64(value), value > 0
	case float64:
		return int64(value), value > 0
	default:
		return 0, false
	}
}

// roundUpTokens rounds a proposed cap up to the next tokenRounding multiple,
// with the multiple itself as the floor.
func roundUpTokens(tokens int64) int64 {
	if tokens < tokenRounding {
		return tokenRounding
	}

	// Round up to a multiple of 64 with bit masking: adding 63 then clearing
	// the low six bits is the power-of-2 form of ceil(n/64)*64, cheaper than
	// the divide-multiply round trip.
	return (tokens + tokenRounding - 1) &^ (tokenRounding - 1)
}
