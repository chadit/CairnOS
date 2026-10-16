package cli

import (
	"context"
	"encoding/json"
	"flag"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

const usageCmdHelp = "usage: cairnos usage [--host NAME] [--launchers a,b] [--json] [--dir D]\n"

// The two params the runner reads, spelled the way an [action] block spells
// them, so the flags and a workflow ask for the same thing.
const (
	paramUsageHost      = "host"
	paramUsageLaunchers = "launchers"
)

// usageActionName labels the one task this command runs, the way an action
// block would name it.
const usageActionName = "usage"

// cmdUsage probes the configured model CLI logins, or reads the report a
// configured script last wrote, and prints what each one has spent of its
// rate-limit windows.
func (a *App) cmdUsage(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet(usageActionName, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	hostFlag := flags.String(paramUsageHost, "", "configured [host.<name>] to probe (default this machine)")
	launchersFlag := flags.String(paramUsageLaunchers, "", "comma-separated subset of the configured launchers")
	jsonFlag := flags.Bool("json", false, "print the report as indented JSON")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", usageCmdHelp)

		return ExitUsage
	}

	cfg, code, proceed := a.loadConfigAt(*dirFlag, usageActionName)
	if !proceed {
		return code
	}

	probe, err := builtins.NewUsage(cfg.Runners[builtins.UsageName], usageOptions(cfg.Hosts, a.envLookup()))
	if err != nil {
		a.printErrf("usage: %v\n", err)

		return ExitFailure
	}

	return a.reportUsage(ctx, probe, usageProbeTask(*hostFlag, *launchersFlag), *jsonFlag)
}

// reportUsage runs the probe, or reads the script's report, and renders it. A login that failed rides in the
// report, so only a probe that reported nothing at all is an exit failure.
func (a *App) reportUsage(ctx context.Context, probe *builtins.Usage, task *runner.Task, asJSON bool) int {
	a.noteClaudeGap(probe, task)

	report, err := probe.Report(ctx, task)
	if err != nil {
		a.printErrf("usage: %v\n", err)

		return ExitFailure
	}

	if asJSON {
		return a.printUsageJSON(&report)
	}

	a.writeUsageTable(&report)

	return ExitOK
}

// usageProbeTask builds the one action this command runs. An absent flag leaves
// its param out, which is how the runner reads "this machine" and "all of them".
func usageProbeTask(host, launchers string) *runner.Task {
	params := map[string]any{}

	if host != "" {
		params[paramUsageHost] = host
	}

	if launchers != "" {
		params[paramUsageLaunchers] = splitUsageLaunchers(launchers)
	}

	return &runner.Task{ActionName: usageActionName, Params: params, Resolved: true}
}

// splitUsageLaunchers turns the comma-separated flag into the list the runner
// validates, dropping the empty piece a trailing comma leaves behind.
func splitUsageLaunchers(value string) []any {
	names := make([]any, 0, strings.Count(value, ",")+1)

	for piece := range strings.SplitSeq(value, ",") {
		trimmed := strings.TrimSpace(piece)
		if trimmed != "" {
			names = append(names, trimmed)
		}
	}

	return names
}

// noteClaudeGap warns before a probe spends minutes spacing Claude logins apart,
// so the wait for the CLI's usage cache to expire does not read as a hang.
func (a *App) noteClaudeGap(probe *builtins.Usage, task *runner.Task) {
	gap, logins := probe.ClaudeGap(task)

	waits := logins - 1
	if gap <= 0 || waits < 1 {
		return
	}

	a.printErrf("probing %d Claude logins %ds apart, about %dm total\n",
		logins, int(gap.Seconds()), usageWaitMinutes(time.Duration(waits)*gap))
}

// usageWaitMinutes rounds the wait to whole minutes and never below one, since
// the note sets an expectation rather than timing the run.
func usageWaitMinutes(total time.Duration) int {
	minutes := int((total + time.Minute/2) / time.Minute)
	if minutes < 1 {
		return 1
	}

	return minutes
}

// printUsageJSON prints the report in the shape a script parses.
func (a *App) printUsageJSON(report *builtins.UsageReport) int {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		a.printErrf("usage: encode report: %v\n", err)

		return ExitFailure
	}

	a.printOutf("%s\n", encoded)

	return ExitOK
}

// writeUsageTable prints the report's rows. tabwriter leaves a short row's last
// cell out of the columns, so a failed login's reason spans them without widening them.
// A stale report's notice goes above the table, the first thing read.
func (a *App) writeUsageTable(report *builtins.UsageReport) {
	if report.Notice != "" {
		a.printOutf("%s\n", report.Notice)
	}

	tab := newTab(a.stdout)

	for _, row := range report.Rows() {
		a.tabWritef(tab, "%s\n", strings.Join(row, "\t"))
	}

	a.flushTab(tab)
}
