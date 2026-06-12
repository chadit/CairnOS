package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/runner/builtins"
)

// rulesLookup adapts a rules.Service to the narrow GetRules(map) seam the
// language_rules_service runner and the MCP get_language_rules tool both
// depend on. It lives in the wiring layer so neither the runner nor the MCP
// package imports the rules store, and it translates rules.ErrUnknownLanguage
// into builtins.ErrRulesUnknownLanguage so the runner can fall back to its
// static files without knowing the store's sentinel.
type rulesLookup struct {
	service rules.Service
}

// newRulesLookup wraps a rules.Service for the runner and MCP seams.
func newRulesLookup(service rules.Service) *rulesLookup {
	return &rulesLookup{service: service}
}

// GetRules merges the language defaults with the project overrides and returns
// the flat key/value map both consumers want. An unknown language is
// translated to the runner's own sentinel so the fallback path triggers.
func (r *rulesLookup) GetRules(ctx context.Context, language, project string) (map[string]string, error) {
	set, err := r.service.GetRules(ctx, language, project)
	if err == nil {
		return set.AsMap(), nil
	}

	if errors.Is(err, rules.ErrUnknownLanguage) {
		return nil, builtins.ErrRulesUnknownLanguage
	}

	return nil, fmt.Errorf("get rules: %w", err)
}
