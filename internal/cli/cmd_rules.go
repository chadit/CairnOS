package cli

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chadit/CairnOS/internal/rules"
)

const rulesUsage = `usage: cairnos rules <set|get|import> ...

  set <lang> [<project>] <key> <value>   Upsert one rule
  get <lang> [<project>]                 Print the merged rule set
  import <dir>                           Seed rules from static <lang>[.<project>].toml files
`

// rulesFileExt is the static-file extension import reads, the same flat
// key = "value" TOML format the language_rules_service stub used, so the old
// examples/rules dir migrates in.
const rulesFileExt = ".toml"

// minSetArgs and minGetArgs bound the positional counts the parser accepts
// before deciding the optional project slot is present.
const (
	setArgsNoProject = 3 // lang key value
	setArgsProject   = 4 // lang project key value
	getArgsProject   = 2 // lang project
)

// cmdRules dispatches the rules subcommands.
func (a *App) cmdRules(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", rulesUsage)

		return ExitUsage
	}

	switch args[0] {
	case subcmdSet:
		return a.cmdRulesSet(ctx, args[1:])
	case subcmdGet:
		return a.cmdRulesGet(ctx, args[1:])
	case subcmdImport:
		return a.cmdRulesImport(ctx, args[1:])
	default:
		a.printErrf("unknown rules subcommand %q\n", args[0])
		a.printErrf("%s", rulesUsage)

		return ExitUsage
	}
}

func (a *App) cmdRulesSet(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("rules set", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	descFlag := flags.String("description", "", "optional human note on the rule")
	dirFlag := dirFlagOn(flags)

	positionals, code, proceed := parseThreePositionals(flags, args)
	if !proceed {
		return code
	}

	rule, ok := parseSetPositionals(positionals)
	if !ok {
		a.printErrf("usage: cairnos rules set <lang> [<project>] <key> <value>\n")

		return ExitUsage
	}

	rule.Description = *descFlag

	svc, _, code, proceed := openAt(ctx, a, "rules set", *dirFlag, rulesBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(svc)

	if err := svc.PutRule(ctx, rule); err != nil {
		a.printErrf("rules set: %v\n", err)

		return ExitFailure
	}

	a.printOutf("set %s/%s/%s\n", rule.Language, rule.Project, rule.Key)

	return ExitOK
}

// parseSetPositionals reads the variable-length positional list into a Rule.
// Three positionals are lang/key/value (a language default); four are
// lang/project/key/value (a project override).
func parseSetPositionals(positionals []string) (*rules.Rule, bool) {
	switch len(positionals) {
	case setArgsNoProject:
		return &rules.Rule{Language: positionals[0], Key: positionals[1], Value: positionals[2]}, true
	case setArgsProject:
		return &rules.Rule{
			Language: positionals[0], Project: positionals[1],
			Key: positionals[2], Value: positionals[3],
		}, true
	default:
		return nil, false
	}
}

func (a *App) cmdRulesGet(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("rules get", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	positionals, code, proceed := parseThreePositionals(flags, args)
	if !proceed {
		return code
	}

	if len(positionals) == 0 {
		a.printErrf("usage: cairnos rules get <lang> [<project>]\n")

		return ExitUsage
	}

	language := positionals[0]

	var project string
	if len(positionals) >= getArgsProject {
		project = positionals[1]
	}

	svc, _, code, proceed := openAt(ctx, a, "rules get", *dirFlag, rulesBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(svc)

	set, err := svc.GetRules(ctx, language, project)
	if err != nil {
		a.printErrf("rules get: %v\n", err)

		return ExitFailure
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "KEY\tVALUE\n")

	for idx := range set.Rules {
		a.tabWritef(tab, "%s\t%s\n", set.Rules[idx].Key, set.Rules[idx].Value)
	}

	a.flushTab(tab)

	return ExitOK
}

func (a *App) cmdRulesImport(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("rules import", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	srcDir, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if srcDir == "" {
		a.printErrf("usage: cairnos rules import <dir> [--dir D]\n")

		return ExitUsage
	}

	svc, _, code, proceed := openAt(ctx, a, "rules import", *dirFlag, rulesBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(svc)

	imported, err := importRulesDir(ctx, svc, srcDir)
	if err != nil {
		a.printErrf("rules import: %v\n", err)

		return ExitFailure
	}

	a.printOutf("imported %d rule(s) from %s\n", imported, srcDir)

	return ExitOK
}

// importRulesDir reads every <lang>[.<project>].toml file under srcDir and
// upserts its flat key = "value" rules, the same format the static-file stub
// read, so the old examples/rules dir migrates into the store. It returns the
// count of rules written.
func importRulesDir(ctx context.Context, svc rules.Service, srcDir string) (int, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return 0, &importError{op: "read rules dir", err: err}
	}

	var total int

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), rulesFileExt) {
			continue
		}

		language, project := splitRuleFileName(entry.Name())

		count, fileErr := importRuleFile(ctx, svc, filepath.Join(srcDir, entry.Name()), language, project)
		if fileErr != nil {
			return total, fileErr
		}

		total += count
	}

	return total, nil
}

// importRuleFile parses one flat rules file and upserts every key under the
// language and optional project the file name encodes.
func importRuleFile(ctx context.Context, svc rules.Service, path, language, project string) (int, error) {
	pairs, err := readFlatRuleFile(path)
	if err != nil {
		return 0, err
	}

	var count int

	for key, value := range pairs {
		rule := &rules.Rule{Language: language, Project: project, Key: key, Value: value}

		if putErr := svc.PutRule(ctx, rule); putErr != nil {
			return count, &importError{op: "put rule", err: putErr}
		}

		count++
	}

	return count, nil
}

// splitRuleFileName decodes a rules file name into its language and optional
// project: "go.toml" is language go, "go.finance-app.toml" is language go with
// project finance-app. A name with more dots keeps everything after the first
// as the project, matching the stub's <language>.<project>.toml convention.
func splitRuleFileName(name string) (string, string) {
	stem := strings.TrimSuffix(name, rulesFileExt)

	language, project, found := strings.Cut(stem, ".")
	if !found {
		return language, ""
	}

	return language, project
}

// readFlatRuleFile parses a flat key = "value" rules file into a map, the same
// shape the static-file stub read. Blank lines and # comments are skipped, and
// a TOML basic-string value is unquoted.
func readFlatRuleFile(path string) (map[string]string, error) {
	// #nosec G304 -- path is the operator-supplied import dir joined with a
	// directory entry name; reading local files is the command's whole job.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &importError{op: "read rules file", err: err}
	}

	lines := strings.Split(string(raw), "\n")
	pairs := make(map[string]string, len(lines))

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			continue
		}

		pairs[strings.TrimSpace(key)] = unquoteRuleValue(strings.TrimSpace(value))
	}

	return pairs, nil
}

// importError tags an import failure with the operation that hit it, so the
// command surfaces a wrapped error (wrapcheck) rather than a bare os/store
// error, and errors.Is still reaches the cause.
type importError struct {
	err error
	op  string
}

func (e *importError) Error() string {
	return e.op + ": " + e.err.Error()
}

func (e *importError) Unwrap() error {
	return e.err
}

// unquoteRuleValue strips TOML basic-string quoting, tolerating bare values so
// hand-written files stay forgiving.
func unquoteRuleValue(value string) string {
	if len(value) >= 2 && strings.HasPrefix(value, `"`) {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}

	return value
}
