package builtins_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

const (
	langKey  = "language"
	projKey  = "project"
	dirKey   = "dir"
	langGo   = "go"
	projName = "cairnos"

	// actionRules is the fixture action name; rulesKey reads the rule map
	// out of the structured output. Same literal, different roles.
	actionRules = "rules"
	rulesKey    = "rules"

	// keyMaxLine is the shared rule key the merge and service tests assert on.
	keyMaxLine = "max_line"
)

// errDownstream stands in for a non-unknown-language failure from a wired
// rules service, so the propagation test asserts on a real sentinel rather
// than an inline error string (err113).
var errDownstream = errors.New("rules backend unavailable")

func writeRules(t *testing.T, dir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write rules file: %v", err)
	}
}

func newRulesRunner(t *testing.T, dir string) *builtins.LanguageRules {
	t.Helper()

	rules, err := builtins.NewLanguageRules(map[string]any{dirKey: dir})
	if err != nil {
		t.Fatalf("new language rules: %v", err)
	}

	return rules
}

func rulesTask(language, project string) *runner.Task {
	params := map[string]any{langKey: language}
	if project != "" {
		params[projKey] = project
	}

	return &runner.Task{RunID: testRunID, ActionName: actionRules, Params: params, Resolved: true}
}

func TestLanguageRulesMergesProjectOverlay(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	writeRules(t, dir, "go.toml", `# base rule set
max_line = "120"
errors = "wrap"
`)
	writeRules(t, dir, "go.cairnos.toml", `max_line = "100"
naming = "snake"
`)

	rules := newRulesRunner(t, dir)

	result, err := rules.Execute(t.Context(), rulesTask(langGo, projName))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isMap := result.Output.(map[string]any)
	if !isMap {
		t.Fatalf("output = %#v", result.Output)
	}

	if output[langKey] != langGo || output[projKey] != projName {
		t.Fatalf("language/project = %v/%v", output[langKey], output[projKey])
	}

	ruleSet, isRules := output[rulesKey].(map[string]string)
	if !isRules {
		t.Fatalf("rules = %#v", output[rulesKey])
	}

	want := map[string]string{
		keyMaxLine: "100",   // project overlay wins
		"errors":   "wrap",  // base survives
		"naming":   "snake", // overlay-only key lands
	}

	for key, value := range want {
		if ruleSet[key] != value {
			t.Fatalf("rules[%s] = %q, want %q (full set %v)", key, ruleSet[key], value, ruleSet)
		}
	}
}

func TestLanguageRulesMissingOverlayFallsBack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	writeRules(t, dir, "go.toml", `max_line = "120"
`)

	rules := newRulesRunner(t, dir)

	result, err := rules.Execute(t.Context(), rulesTask(langGo, "ghost-project"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isMap := result.Output.(map[string]any)
	if !isMap {
		t.Fatalf("output = %#v", result.Output)
	}

	ruleSet, isRules := output[rulesKey].(map[string]string)
	if !isRules || ruleSet["max_line"] != "120" {
		t.Fatalf("rules = %#v, want base set", output[rulesKey])
	}
}

func TestLanguageRulesUnknownLanguage(t *testing.T) {
	t.Parallel()

	rules := newRulesRunner(t, t.TempDir())

	_, err := rules.Execute(t.Context(), rulesTask("fortran", ""))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || classified.Class != runner.ClassPermanent || classified.Code != "rules.unknown_language" {
		t.Fatalf("error = %v, want permanent rules.unknown_language", err)
	}
}

func TestLanguageRulesValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		params  map[string]any
		name    string
		dir     string
		wantErr bool
	}{
		{name: "no dir configured", params: map[string]any{langKey: langGo}, wantErr: true},
		{name: "missing language", dir: actionRules, params: map[string]any{}, wantErr: true},
		{name: "language escapes dir", dir: actionRules, params: map[string]any{langKey: "../etc/passwd"}, wantErr: true},
		{name: "project escapes dir", dir: actionRules, params: map[string]any{langKey: langGo, projKey: "a/b"}, wantErr: true},
		{name: "language only", dir: actionRules, params: map[string]any{langKey: langGo}, wantErr: false},
		{name: "language and project", dir: actionRules, params: map[string]any{langKey: langGo, projKey: projName}, wantErr: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg map[string]any
			if tt.dir != "" {
				cfg = map[string]any{dirKey: tt.dir}
			}

			rules, err := builtins.NewLanguageRules(cfg)
			if err != nil {
				t.Fatalf("new language rules: %v", err)
			}

			err = rules.Validate(&runner.Task{
				RunID:      testRunID,
				ActionName: actionRules,
				Params:     tt.params,
				Resolved:   true,
			})

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("validate error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

// fakeRulesLookup is a stand-in for the wired Rules Service: it returns a fixed
// rule map for one language and ErrRulesUnknownLanguage for anything else, or a
// hard error when failWith is set, so the runner's precedence is testable
// without the real store.
type fakeRulesLookup struct {
	failWith error
	merged   map[string]string
	language string
}

func (f *fakeRulesLookup) GetRules(_ context.Context, language, _ string) (map[string]string, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}

	if language != f.language {
		return nil, builtins.ErrRulesUnknownLanguage
	}

	return f.merged, nil
}

// buildServiceRunner builds a service-backed runner and returns the concrete
// type, doing the comma-ok assertion once so the cases stay assertion-free
// (forcetypeassert).
func buildServiceRunner(t *testing.T, lookup *fakeRulesLookup, cfg map[string]any) *builtins.LanguageRules {
	t.Helper()

	built, err := builtins.LanguageRulesServiceFactory(lookup)(cfg)
	if err != nil {
		t.Fatalf("build service-backed runner: %v", err)
	}

	rulesRunner, isConcrete := built.(*builtins.LanguageRules)
	if !isConcrete {
		t.Fatalf("factory returned %T, want *builtins.LanguageRules", built)
	}

	return rulesRunner
}

// runRules executes the runner and returns its rule map, failing the test on
// any execution error.
func runRules(t *testing.T, rulesRunner *builtins.LanguageRules, language, project string) map[string]string {
	t.Helper()

	result, err := rulesRunner.Execute(t.Context(), rulesTask(language, project))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isMap := result.Output.(map[string]any)
	if !isMap {
		t.Fatalf("output = %#v", result.Output)
	}

	ruleSet, isRules := output[rulesKey].(map[string]string)
	if !isRules {
		t.Fatalf("rules = %#v", output[rulesKey])
	}

	return ruleSet
}

func TestLanguageRulesServiceAnswersFirst(t *testing.T) {
	t.Parallel()

	// A dir is configured but the service has the language, so the service
	// wins: the file value never shows.
	dir := t.TempDir()
	writeRules(t, dir, "go.toml", `max_line = "999"`)

	lookup := &fakeRulesLookup{language: langGo, merged: map[string]string{keyMaxLine: "100"}}
	rulesRunner := buildServiceRunner(t, lookup, map[string]any{dirKey: dir})

	served := runRules(t, rulesRunner, langGo, "")
	if served[keyMaxLine] != "100" {
		t.Fatalf("max_line = %q, want service value 100 (not file 999)", served[keyMaxLine])
	}
}

func TestLanguageRulesServiceFallsBackToFiles(t *testing.T) {
	t.Parallel()

	// The service has nothing for python, but the dir does, so the static file
	// answers rather than erroring.
	dir := t.TempDir()
	writeRules(t, dir, "python.toml", `max_line = "88"`)

	lookup := &fakeRulesLookup{language: langGo, merged: map[string]string{keyMaxLine: "100"}}
	rulesRunner := buildServiceRunner(t, lookup, map[string]any{dirKey: dir})

	served := runRules(t, rulesRunner, "python", "")
	if served[keyMaxLine] != "88" {
		t.Fatalf("max_line = %q, want file fallback 88", served[keyMaxLine])
	}
}

func TestLanguageRulesServiceUnknownWithoutDir(t *testing.T) {
	t.Parallel()

	// No dir, and the service does not know the language, so the runner reports
	// the same unknown_language code the static path uses.
	lookup := &fakeRulesLookup{language: langGo, merged: map[string]string{keyMaxLine: "100"}}
	rulesRunner := buildServiceRunner(t, lookup, nil)

	_, execErr := rulesRunner.Execute(t.Context(), rulesTask("python", ""))

	classified, isClassified := errors.AsType[*runner.Error](execErr)
	if !isClassified || classified.Code != "rules.unknown_language" {
		t.Fatalf("error = %v, want rules.unknown_language", execErr)
	}
}

func TestLanguageRulesServiceErrorPropagates(t *testing.T) {
	t.Parallel()

	// A non-unknown service error surfaces as rules.service; it must not be
	// swallowed by the file fallback.
	dir := t.TempDir()
	writeRules(t, dir, "go.toml", `max_line = "120"`)

	lookup := &fakeRulesLookup{failWith: errDownstream}
	rulesRunner := buildServiceRunner(t, lookup, map[string]any{dirKey: dir})

	_, execErr := rulesRunner.Execute(t.Context(), rulesTask(langGo, ""))

	classified, isClassified := errors.AsType[*runner.Error](execErr)
	if !isClassified || classified.Code != "rules.service" {
		t.Fatalf("error = %v, want rules.service", execErr)
	}
}

func TestLanguageRulesServiceValidatesWithoutDir(t *testing.T) {
	t.Parallel()

	// A wired service alone satisfies Validate; no dir is needed.
	lookup := &fakeRulesLookup{language: langGo, merged: map[string]string{}}
	rulesRunner := buildServiceRunner(t, lookup, nil)

	if vErr := rulesRunner.Validate(rulesTask(langGo, "")); vErr != nil {
		t.Fatalf("validate with service and no dir = %v, want nil", vErr)
	}
}
