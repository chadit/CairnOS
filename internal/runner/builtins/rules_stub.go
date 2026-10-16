package builtins

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// LanguageRulesName is the registry name of the rules lookup runner.
const LanguageRulesName = "language_rules_service"

// LanguageRules config key, params, and file naming.
const (
	cfgKeyDir       = "dir"
	paramLanguage   = "language"
	paramProject    = "project"
	rulesExt        = ".toml"
	codePrefixRules = "rules"
)

// RulesLookup is the narrow slice of the Language Rules Service the runner
// needs: one merge call. *rules.Service satisfies it. Keeping the runner on a
// one-method interface (rather than the full Service) means a change to the
// rules store's management methods never touches this runner, and the wiring
// can pass any merge source (interfacebloat: compose narrow interfaces).
type RulesLookup interface {
	// GetRules returns the merged rule set for a language, optionally narrowed
	// to a project, as a flat key/value map.
	GetRules(ctx context.Context, language, project string) (map[string]string, error)
}

// LanguageRules serves coding rules from the Rules Service or, with none
// wired, from static files (.agents/spec/03-runner-interface.md;
// .agents/spec/05-memory-and-knowledge.md, "Language Rules Service"). With
// neither a service nor a dir, Validate fails.
//
// Config: dir (optional with a service). Params: language (required) and
// project (optional). Rule files are flat key = "value" TOML under dir:
// <language>.toml is the base set, <language>.<project>.toml overrides it.
type LanguageRules struct {
	service RulesLookup
	dir     string
}

// NewLanguageRules builds the static-file runner from its config block. An
// empty block still builds (the registry constructs every built-in at
// startup); Validate reports the missing dir when a workflow references the
// runner with no service wired.
func NewLanguageRules(cfg map[string]any) (*LanguageRules, error) {
	dir, err := configValue(cfg, cfgKeyDir, "")
	if err != nil {
		return nil, fmt.Errorf("configure %s: %w", LanguageRulesName, err)
	}

	return &LanguageRules{dir: dir}, nil
}

// LanguageRulesFactory adapts NewLanguageRules to the registry contract,
// building the static-file-only runner. The serve wiring uses
// LanguageRulesServiceFactory instead when a rules service exists.
func LanguageRulesFactory() runner.Factory {
	return asFactory(NewLanguageRules)
}

// LanguageRulesServiceFactory adapts the runner to the registry contract with
// a Rules Service wired in, closing over the service because the registry only
// passes config blocks. The dir from config still rides along as the
// static-file fallback, so a node with both a service and a configured dir
// answers from the service first and the files for an unknown language.
func LanguageRulesServiceFactory(service RulesLookup) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		rulesRunner, err := NewLanguageRules(cfg)
		if err != nil {
			return nil, err
		}

		rulesRunner.service = service

		return rulesRunner, nil
	}
}

// Name implements runner.Runner.
func (*LanguageRules) Name() string {
	return LanguageRulesName
}

// Validate implements runner.Runner: a service or a dir must back the runner,
// language is a required name, project an optional one. The file-name safety
// check still runs whenever a dir is configured, since a wired service can
// fall back to the files.
func (l *LanguageRules) Validate(task *runner.Task) error {
	if l.service == nil && l.dir == "" {
		return runner.NewError(runner.ClassPermanent, codePrefixRules+".not_configured",
			"runner needs a rules service or a configured dir")
	}

	if err := requireStringParam(task, paramLanguage, codePrefixRules); err != nil {
		return err
	}

	if err := optionalStringParam(task, paramProject, codePrefixRules); err != nil {
		return err
	}

	if l.dir == "" {
		return nil
	}

	return validateRuleNames(task)
}

// Execute implements runner.Runner. Reads are local files, so the ctx check
// up front is the whole cancellation story.
func (l *LanguageRules) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := ctx.Err(); err != nil {
		return runner.Result{}, &runner.Error{
			Class: runner.ClassTransient, Code: codePrefixRules + ".canceled",
			Message: "canceled before lookup", Cause: err,
		}
	}

	if err := l.Validate(task); err != nil {
		return runner.Result{}, err
	}

	language, _ := task.Params[paramLanguage].(string)
	project, _ := task.Params[paramProject].(string)

	start := time.Now()

	merged, err := l.resolveRules(ctx, language, project)
	if err != nil {
		return runner.Result{}, err
	}

	return runner.Result{
		Output: map[string]any{
			paramLanguage: language,
			paramProject:  project,
			"rules":       merged,
		},
		Usage: runner.Usage{Latency: time.Since(start)},
	}, nil
}

// resolveRules applies the service-first precedence: a wired service answers,
// and only when it reports the language unknown does a configured dir fall back
// to the static files. That fallback is what lets the shipped examples, which
// carry a rules dir, keep working alongside a serve process backed by the rules
// store.
func (l *LanguageRules) resolveRules(ctx context.Context, language, project string) (map[string]string, error) {
	if l.service == nil {
		return l.loadRules(language, project)
	}

	merged, err := l.service.GetRules(ctx, language, project)
	if err == nil {
		return merged, nil
	}

	if !errors.Is(err, ErrRulesUnknownLanguage) {
		return nil, &runner.Error{
			Class: runner.ClassPermanent, Code: codePrefixRules + ".service",
			Message: err.Error(), Cause: err,
		}
	}

	if l.dir != "" {
		// The store has nothing for this language; the configured dir may, so
		// the static files are the second source rather than an error.
		return l.loadRules(language, project)
	}

	return nil, runner.NewError(runner.ClassPermanent, codePrefixRules+".unknown_language",
		fmt.Sprintf("no rules for language %q", language))
}

// loadRules reads the base language file and overlays the optional project
// file, project keys winning, per the merge contract in the spec.
func (l *LanguageRules) loadRules(language, project string) (map[string]string, error) {
	rules, err := readFlatRules(filepath.Join(l.dir, language+rulesExt))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, runner.NewError(runner.ClassPermanent, codePrefixRules+".unknown_language",
			fmt.Sprintf("no rules file for language %q", language))
	}

	if err != nil {
		return nil, &runner.Error{Class: runner.ClassPermanent, Code: codePrefixRules + ".read", Message: err.Error(), Cause: err}
	}

	if project == "" {
		return rules, nil
	}

	overlay, err := readFlatRules(filepath.Join(l.dir, language+"."+project+rulesExt))
	if errors.Is(err, fs.ErrNotExist) {
		// A missing project overlay is normal: most projects ride the
		// language defaults.
		return rules, nil
	}

	if err != nil {
		return nil, &runner.Error{Class: runner.ClassPermanent, Code: codePrefixRules + ".read", Message: err.Error(), Cause: err}
	}

	maps.Copy(rules, overlay)

	return rules, nil
}

// validateRuleNames restricts language and project to letters, digits, dash,
// and underscore: the values splice into file names, so anything looser
// could escape the rules dir. Unresolved references defer to launch time,
// matching local_fn.
func validateRuleNames(task *runner.Task) error {
	for _, key := range []string{paramLanguage, paramProject} {
		name, _ := task.Params[key].(string)
		if name == "" {
			continue
		}

		if !task.Resolved && containsRefDelimiter(name) {
			continue
		}

		if !safeFileStem(name) {
			return runner.NewError(runner.ClassPermanent, codePrefixRules+".bad_"+key,
				key+" may only contain letters, digits, dash, and underscore")
		}
	}

	return nil
}

// safeFileStem reports whether a params value is safe to splice into a file
// name: no separators and no dots, so lookups stay inside the rules dir.
func safeFileStem(name string) bool {
	for _, ch := range name {
		if !isStemChar(ch) {
			return false
		}
	}

	return true
}

func isStemChar(ch rune) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') || ch == '-' || ch == '_'
}

// readFlatRules parses one flat key = "value" rules file. A hand-rolled
// reader keeps this Phase 1 stub on the standard library; the files are flat
// string maps by contract, never nested TOML
// (.agents/spec/05-memory-and-knowledge.md).
func readFlatRules(path string) (map[string]string, error) {
	// #nosec G304 -- path is the operator-configured rules dir joined with
	// params validated against a letters-digits-dash-underscore allowlist.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rules file: %w", err)
	}

	lines := strings.Split(string(raw), "\n")
	rules := make(map[string]string, len(lines))

	for lineNo, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			return nil, fmt.Errorf("%w: %s line %d has no key = value pair",
				ErrMalformedRules, filepath.Base(path), lineNo+1)
		}

		rules[strings.TrimSpace(key)] = unquoteValue(strings.TrimSpace(value))
	}

	return rules, nil
}

// unquoteValue strips TOML basic-string quoting, tolerating bare values so
// hand-written files stay forgiving.
func unquoteValue(value string) string {
	if len(value) >= 2 && strings.HasPrefix(value, `"`) {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}

	return value
}
