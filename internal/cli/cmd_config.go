package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/workflow"
)

const (
	configUsage = "usage: cairnos config allow-action <add|list> [flags]\n"

	allowActionUsage = "usage: cairnos config allow-action add <name> [--dir D]\n" +
		"       cairnos config allow-action list [--dir D]\n"

	allowActionWord  = "allow-action"
	allowActionAdd   = "config allow-action add"
	allowActionList  = "config allow-action list"
	allowActionsHome = "[runner.exec] allow_actions"
)

// cmdConfig edits config.toml in place. allow-action is its only family so
// far: it replaces hand edits to the exec runner's allow list.
func (a *App) cmdConfig(args []string) int {
	if len(args) == 0 || args[0] != allowActionWord {
		a.printErrf("%s", configUsage)

		return ExitUsage
	}

	rest := args[1:]
	if len(rest) == 0 {
		a.printErrf("%s", allowActionUsage)

		return ExitUsage
	}

	switch rest[0] {
	case subcmdAdd:
		return a.cmdAllowActionAdd(rest[1:])
	case subcmdList:
		return a.cmdAllowActionList(rest[1:])
	default:
		a.printErrf("unknown config allow-action subcommand %q\n", rest[0])
		a.printErrf("%s", allowActionUsage)

		return ExitUsage
	}
}

// cmdAllowActionAdd appends one action name to the exec allow list. serve
// reads config.toml once at startup, so a successful add says a restart is
// needed rather than performing one.
func (a *App) cmdAllowActionAdd(args []string) int {
	flags := flag.NewFlagSet(allowActionAdd, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	name, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if name == "" {
		a.printErrf("%s", allowActionUsage)

		return ExitUsage
	}

	if !workflow.ValidName(name) {
		a.printErrf("%s: %q is not an action name: it must be a lowercase letter followed by lowercase letters, digits, or _\n",
			allowActionAdd, name)

		return ExitUsage
	}

	path, src, mode, err := a.readConfigFile(*dirFlag)
	if err != nil {
		a.printCmdErr(allowActionAdd, err)

		return ExitFailure
	}

	edited, added, err := config.AddExecAllowAction(src, name)
	if err != nil {
		a.printErrf("%s: %s left unchanged: %v\n", allowActionAdd, path, err)

		return ExitFailure
	}

	if !added {
		a.printOutf("%s is already in %s in %s\n", name, allowActionsHome, path)

		return ExitOK
	}

	if err := a.writeFileAtomic(path, edited, mode); err != nil {
		a.printCmdErr(allowActionAdd, err)

		return ExitFailure
	}

	a.printOutf("added %s to %s in %s\n", name, allowActionsHome, path)
	a.printErrf("restart cairnos to apply: serve reads config.toml only at startup\n")

	return ExitOK
}

// cmdAllowActionList prints the exec allow list one name per line, so a
// script can grep it. An empty list gets a stderr note because exec then
// refuses every action, which is worth saying out loud.
func (a *App) cmdAllowActionList(args []string) int {
	flags := flag.NewFlagSet(allowActionList, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", allowActionUsage)

		return ExitUsage
	}

	path, src, _, err := a.readConfigFile(*dirFlag)
	if err != nil {
		a.printCmdErr(allowActionList, err)

		return ExitFailure
	}

	names, err := config.ExecAllowActions(src)
	if err != nil {
		a.printErrf("%s: %s: %v\n", allowActionList, path, err)

		return ExitFailure
	}

	if len(names) == 0 {
		a.printErrf("%s is empty in %s, so exec refuses every action\n", allowActionsHome, path)
	}

	for _, name := range names {
		a.printOutf("%s\n", name)
	}

	return ExitOK
}

// readConfigFile reads config.toml under the CairnOS home with its mode. A
// symlinked config resolves to its target first, since the atomic rename
// would otherwise replace the link with a plain file.
func (a *App) readConfigFile(dirFlag string) (string, []byte, os.FileMode, error) {
	dir, err := resolveDir(dirFlag, a.envLookup())
	if err != nil {
		return "", nil, 0, err
	}

	return readConfigIn(dir)
}

// readConfigIn is readConfigFile for a home already resolved.
func readConfigIn(dir string) (string, []byte, os.FileMode, error) {
	path, err := filepath.EvalSymlinks(filepath.Join(dir, configFileName))
	if err != nil {
		return "", nil, 0, fmt.Errorf("find config: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", nil, 0, fmt.Errorf("stat config: %w", err)
	}

	src, err := os.ReadFile(path) // #nosec G304 -- the CairnOS home's own config file, named by the operator's --dir
	if err != nil {
		return "", nil, 0, fmt.Errorf("read config: %w", err)
	}

	return path, src, info.Mode().Perm(), nil
}
