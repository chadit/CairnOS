package tuning

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/store"
)

// Annotation renders an approved suggestion as the change for the operator
// to apply by hand: a kind-specific preamble, the proposed keys and values,
// and the standing Phase 3 reminder that CairnOS writes nothing itself. The
// CLI prints it on approve and the API returns it in the approve response,
// so both surfaces hand the human the same text.
func Annotation(suggestion *store.Suggestion) string {
	var out strings.Builder

	fmt.Fprintf(&out, "suggestion %s (%s) targets %s\n", suggestion.ID, suggestion.Kind, suggestion.Target)
	out.WriteString(annotationPreamble(suggestion))
	out.WriteString("\nproposed change:\n")

	for _, key := range slices.Sorted(maps.Keys(suggestion.Proposed)) {
		fmt.Fprintf(&out, "  %s = %v\n", key, suggestion.Proposed[key])
	}

	out.WriteString("\napply this by hand; CairnOS does not modify workflow or config files in Phase 3.\n")

	return out.String()
}

// annotationPreamble says where the change goes, per kind.
func annotationPreamble(suggestion *store.Suggestion) string {
	switch suggestion.Kind {
	case store.SuggestionCaching:
		return "add a result cache for this action in the workflow file:\n"
	case store.SuggestionBatching:
		return "restructure the workflow so the burst of calls becomes one batched action:\n"
	case store.SuggestionParamTuning:
		if block, hasBlock := suggestion.Proposed[ProposedConfigBlock].(string); hasBlock {
			return fmt.Sprintf("edit the [%s] block in config.toml:\n", block)
		}

		return "edit the runner's config block in config.toml:\n"
	default:
		return ""
	}
}
