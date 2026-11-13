// Package rules is the Language Rules Service: coding standards keyed by
// language and project, with project overrides merged over language defaults.
// It backs the language_rules_service runner and the Language Rules Assistant
// sample agent.
//
// Storage choice: a dedicated rules table keyed (workspace, language, project,
// key), not the vault. The service could be a typed view over the
// vault, but a flat key/value rule is a poor fit for a document store, and a
// dedicated table makes the merge a two-query union instead of a graph walk
// over tagged documents. The vault still holds prose rule documents; this
// service holds the typed key/value rules the runner merges. Both are
// workspace-scoped through the same store context helper.
package rules

import (
	"context"
	"slices"
)

// Service is the rules contract every backend satisfies; swapping backends is
// a config choice, never a runner change, the same seam the vault and memory
// stores have. GetRules is the merge call the runner makes; the rest manage
// the rule set.
type Service interface {
	// GetRules returns the merged rule set for a language, optionally narrowed
	// to a project. Project rules override or extend the language defaults: a
	// project rule with a Key that matches a language default replaces it, a
	// project rule with a new Key adds to the set.
	GetRules(ctx context.Context, language, project string) (RuleSet, error)

	// PutRule upserts one rule, keyed by (language, project, key) within the
	// workspace. An empty project sets a language default.
	PutRule(ctx context.Context, rule *Rule) error

	// ListLanguages returns the distinct languages that have at least one rule
	// in the workspace, sorted.
	ListLanguages(ctx context.Context) ([]string, error)

	// ListRules returns every rule for a language across all projects (and the
	// language defaults), unmerged, so an operator can see where each value
	// comes from. Sorted by project then key.
	ListRules(ctx context.Context, language string) ([]Rule, error)
}

// Rule is one coding-standard entry. An empty Project means the rule is a
// language default applying to every project in that language.
type Rule struct {
	Language    string
	Project     string // empty = applies to all projects in the language
	Key         string // e.g. "naming.functions", "indent.style"
	Value       string
	Description string
}

// RuleSet is the merged result of GetRules: the language defaults with the
// project's overrides applied. Project records which project's overrides were
// merged in (empty for a defaults-only set).
type RuleSet struct {
	Language string
	Project  string
	Rules    []Rule // language defaults merged with project overrides
}

// Merge applies project overrides over language defaults and returns the
// merged, sorted rule set. Both backends share it after reading their two rule
// slices. A project rule whose Key already appears among the defaults replaces
// that default; a new Key adds. The result carries the language both slices
// came from and the project that supplied the overrides.
func Merge(language, project string, defaults, overrides []Rule) RuleSet {
	merged := make(map[string]Rule, len(defaults)+len(overrides))

	for _, rule := range defaults {
		merged[rule.Key] = rule
	}

	for _, rule := range overrides {
		merged[rule.Key] = rule
	}

	return RuleSet{Language: language, Project: project, Rules: sortedRules(merged)}
}

// sortedRules flattens a key-to-rule map into a slice sorted by key, so the
// merged set reads back in the same order every call, which the runner output
// and the tests rely on.
func sortedRules(byKey map[string]Rule) []Rule {
	out := make([]Rule, 0, len(byKey))
	for _, rule := range byKey {
		out = append(out, rule)
	}

	slices.SortFunc(out, func(a, b Rule) int {
		return cmpString(a.Key, b.Key)
	})

	return out
}

// cmpString orders two strings, the comparator sortedRules and ListRules share.
func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// AsMap renders a rule set as a flat key/value map, the shape the
// language_rules_service runner emits as its Output. Descriptions are dropped:
// the runner's contract is the merged values, and the original static-file
// stub returned a flat string map.
func (rs *RuleSet) AsMap() map[string]string {
	out := make(map[string]string, len(rs.Rules))
	for idx := range rs.Rules {
		out[rs.Rules[idx].Key] = rs.Rules[idx].Value
	}

	return out
}
