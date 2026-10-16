// Package rulestest is the shared behavioral spec for rules.Service
// implementations. RunConformance is one suite every backend runs against a
// fresh store: the sqlite backend calls it, and the postgres backend calls it
// when a test DSN is configured. One spec, two backends, so a merge or
// list-languages difference between them fails a test instead of going
// unnoticed (.agents/spec/05-memory-and-knowledge.md, the merge-semantics
// acceptance criterion).
package rulestest

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/rules"
)

// Shared fixture values; goconst applies to tests in this repo's lint policy.
const (
	langGo      = "go"
	langPython  = "python"
	projFinance = "finance-app"
	keyMaxLine  = "max_line"
	keyErrors   = "errors"
	keyNaming   = "naming"
	valWrap     = "wrap"
	valSnake    = "snake"
)

// twoRows is the unmerged row count testListRules expects (one default plus
// one project override); a named constant keeps the magic-number check happy.
const twoRows = 2

// OpenFunc hands the suite a fresh, empty rules store. The implementation
// registers cleanup so each subtest gets an isolated database. S is the
// backend's own type, so a backend's opener returns what it opened while the
// suite drives only the rules.Service methods.
type OpenFunc[S rules.Service] func(t *testing.T) S

// RunConformance runs the full rules.Service behavioral suite against the
// store openFn hands out. Each named subtest opens its own store, so they are
// independent and the caller may run the suite under t.Parallel.
func RunConformance[S rules.Service](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	cases := []struct {
		run  func(t *testing.T, openFn OpenFunc[S])
		name string
	}{
		{name: "PutAndGetDefaults", run: testPutAndGetDefaults},
		{name: "ProjectOverrideReplacesAndAdds", run: testProjectOverride},
		{name: "MissingProjectFallsBackToDefaults", run: testMissingProjectFallsBack},
		{name: "UnknownLanguage", run: testUnknownLanguage},
		{name: "PutRejectsEmptyKey", run: testPutRejectsEmptyKey},
		{name: "ListLanguages", run: testListLanguages},
		{name: "ListRulesAcrossProjects", run: testListRules},
		{name: "RuleSetAsMap", run: testRuleSetAsMap},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, openFn)
		})
	}
}

// putRule stages one rule, failing the test on error.
func putRule(t *testing.T, svc rules.Service, rule *rules.Rule) {
	t.Helper()

	if err := svc.PutRule(t.Context(), rule); err != nil {
		t.Fatalf("put rule %+v: %v", rule, err)
	}
}

// ruleMap renders a rule set as a key/value map for value comparison.
func ruleMap(set rules.RuleSet) map[string]string {
	return set.AsMap()
}

func testPutAndGetDefaults[S rules.Service](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	svc := openFn(t)

	putRule(t, svc, &rules.Rule{Language: langGo, Key: keyMaxLine, Value: "120"})
	putRule(t, svc, &rules.Rule{Language: langGo, Key: keyErrors, Value: valWrap})

	set, err := svc.GetRules(t.Context(), langGo, "")
	if err != nil {
		t.Fatalf("get rules: %v", err)
	}

	if set.Language != langGo || set.Project != "" {
		t.Errorf("rule set = %+v, want language go, no project", set)
	}

	want := map[string]string{keyMaxLine: "120", keyErrors: valWrap}
	if got := ruleMap(set); !reflect.DeepEqual(got, want) {
		t.Errorf("defaults = %v, want %v", got, want)
	}
}

func testProjectOverride[S rules.Service](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	svc := openFn(t)

	// Language defaults.
	putRule(t, svc, &rules.Rule{Language: langGo, Key: keyMaxLine, Value: "120"})
	putRule(t, svc, &rules.Rule{Language: langGo, Key: keyErrors, Value: valWrap})

	// Project rules: one replaces a default key, one adds a new key.
	putRule(t, svc, &rules.Rule{Language: langGo, Project: projFinance, Key: keyMaxLine, Value: "100"})
	putRule(t, svc, &rules.Rule{Language: langGo, Project: projFinance, Key: keyNaming, Value: valSnake})

	set, err := svc.GetRules(t.Context(), langGo, projFinance)
	if err != nil {
		t.Fatalf("get merged rules: %v", err)
	}

	if set.Project != projFinance {
		t.Errorf("rule set project = %q, want %q", set.Project, projFinance)
	}

	want := map[string]string{
		keyMaxLine: "100",    // project override wins
		keyErrors:  valWrap,  // language default survives
		keyNaming:  valSnake, // project-only key lands
	}
	if got := ruleMap(set); !reflect.DeepEqual(got, want) {
		t.Errorf("merged rules = %v, want %v", got, want)
	}
}

func testMissingProjectFallsBack[S rules.Service](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	svc := openFn(t)

	putRule(t, svc, &rules.Rule{Language: langGo, Key: keyMaxLine, Value: "120"})

	// A project with no overlay rows returns the language defaults, the common
	// case: most projects ride the defaults.
	set, err := svc.GetRules(t.Context(), langGo, "ghost-project")
	if err != nil {
		t.Fatalf("get rules for ghost project: %v", err)
	}

	want := map[string]string{keyMaxLine: "120"}
	if got := ruleMap(set); !reflect.DeepEqual(got, want) {
		t.Errorf("fallback rules = %v, want defaults %v", got, want)
	}
}

func testUnknownLanguage[S rules.Service](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	svc := openFn(t)

	if _, err := svc.GetRules(t.Context(), "fortran", ""); !errors.Is(err, rules.ErrUnknownLanguage) {
		t.Errorf("get rules for unknown language = %v, want ErrUnknownLanguage", err)
	}
}

func testPutRejectsEmptyKey[S rules.Service](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	svc := openFn(t)

	if err := svc.PutRule(t.Context(), &rules.Rule{Language: langGo, Key: ""}); !errors.Is(err, rules.ErrInvalidRule) {
		t.Errorf("put rule with empty key = %v, want ErrInvalidRule", err)
	}

	if err := svc.PutRule(t.Context(), &rules.Rule{Language: "", Key: keyMaxLine}); !errors.Is(err, rules.ErrInvalidRule) {
		t.Errorf("put rule with empty language = %v, want ErrInvalidRule", err)
	}
}

func testListLanguages[S rules.Service](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	svc := openFn(t)

	putRule(t, svc, &rules.Rule{Language: langGo, Key: keyMaxLine, Value: "120"})
	putRule(t, svc, &rules.Rule{Language: langPython, Key: keyMaxLine, Value: "88"})
	// A second go rule must not duplicate go in the language list.
	putRule(t, svc, &rules.Rule{Language: langGo, Key: keyErrors, Value: valWrap})

	langs, err := svc.ListLanguages(t.Context())
	if err != nil {
		t.Fatalf("list languages: %v", err)
	}

	want := []string{langGo, langPython}
	if !reflect.DeepEqual(langs, want) {
		t.Errorf("languages = %v, want %v", langs, want)
	}
}

func testListRules[S rules.Service](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	svc := openFn(t)

	putRule(t, svc, &rules.Rule{Language: langGo, Key: keyMaxLine, Value: "120"})
	putRule(t, svc, &rules.Rule{Language: langGo, Project: projFinance, Key: keyMaxLine, Value: "100"})

	all, err := svc.ListRules(t.Context(), langGo)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}

	// Unmerged: both the default and the project override are present, so an
	// operator sees where each value comes from.
	if len(all) != twoRows {
		t.Fatalf("list rules returned %d rows, want both default and override", len(all))
	}

	projects := []string{all[0].Project, all[1].Project}
	if !slices.Contains(projects, "") || !slices.Contains(projects, projFinance) {
		t.Errorf("list rules projects = %v, want one default and one finance-app", projects)
	}
}

func testRuleSetAsMap[S rules.Service](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	svc := openFn(t)

	putRule(t, svc, &rules.Rule{Language: langGo, Key: keyMaxLine, Value: "120", Description: "line length cap"})

	set, err := svc.GetRules(t.Context(), langGo, "")
	if err != nil {
		t.Fatalf("get rules: %v", err)
	}

	// AsMap drops descriptions, returning only the merged values the runner
	// emits as its flat output map.
	want := map[string]string{keyMaxLine: "120"}
	if got := set.AsMap(); !reflect.DeepEqual(got, want) {
		t.Errorf("AsMap = %v, want %v", got, want)
	}
}
