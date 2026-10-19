package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/pkg/runner"
)

// UsageName is the registry name of the rate-limit reporting runner.
const UsageName = "usage"

// Config keys of the [runner.usage] block. The two launcher lists are separate
// because the protocol, not the binary name, decides how a login is read.
const (
	cfgKeyUsageClaude    = "claude"
	cfgKeyUsageCodex     = "codex"
	cfgKeyUsageClaudeGap = "claude_gap"

	cfgKeyUsageResetDuePercent = "reset_due_percent"
	cfgKeyUsageResetDueLead    = "reset_due_lead"
)

// Action params.
const (
	paramUsageHost      = "host"
	paramUsageLaunchers = "launchers"
)

// usageResetLayout prints reset times in UTC so two hosts' reports compare
// however either machine's clock is configured.
const usageResetLayout = "2006-01-02 15:04 MST"

// Defaults for a block the operator left out or left partly filled.
const (
	defaultUsageTimeout = 60 * time.Second

	// defaultUsageScriptTimeout bounds a report script that reads every account
	// in turn and writes only at the end. It is sized on the assumption that one
	// account read, a slow login probe included, takes up to about 20 seconds:
	// at that rate five minutes covers about 15 accounts, where the probe's
	// one-minute default would stop the script after the third.
	defaultUsageScriptTimeout = 5 * time.Minute

	// defaultUsageClaudeGap outlasts the roughly 60-second cache described on
	// probeAll, with a few seconds to spare for a slow launcher.
	defaultUsageClaudeGap = 65 * time.Second

	defaultClaudeCommand = "claude"
	defaultCodexCommand  = "codex"

	// A login under a fifth of its week with two days or more to go is one a
	// banked reset can rescue before the week runs dry, the case the alert is for.
	defaultUsageResetDuePercent = 20.0
	defaultUsageResetDueLead    = "48h"
)

// usageFullPercent is the top of the 0-100 scale both CLIs report usage on.
const usageFullPercent = 100.0

// hoursPerDay turns a span into the days a reset-due line reports.
const hoursPerDay = 24

// Classified error codes this runner returns.
const (
	codeUsageUnknownHost     = UsageName + ".unknown_host"
	codeUsageHostNoSSH       = UsageName + ".host_no_ssh"
	codeUsageBadLaunchers    = UsageName + ".bad_" + paramUsageLaunchers
	codeUsageUnknownLauncher = UsageName + ".unknown_launcher"
	codeUsageUnavailable     = UsageName + ".unavailable"
)

// usageHomePrefix is the one relative form a launcher or wrapper may take.
// The host that runs the binary is the only one that knows its own home, so a
// remote launch hands the expansion over rather than doing it here.
const usageHomePrefix = "~/"

// usageLauncherPattern bounds a configured launcher. Nothing a shell acts on is
// in the set, because a remote entry is spliced into a command the login shell
// runs; a leading dash is out too, since ssh would read it as a flag.
var usageLauncherPattern = regexp.MustCompile(`^(~/)?[A-Za-z0-9_.+@/][A-Za-z0-9_.+@/-]*$`)

// UsageHost is one machine this runner can reach. It carries the two
// [host.<name>] keys the launch needs, which keeps the built-in off the config
// package's dependency path the way the source runner stays off internal/source.
type UsageHost struct {
	// SSH is the alias the ssh command connects to.
	SSH string

	// Wrapper is a command on this host the launcher is run through. Empty when
	// the block names none, which runs the launcher directly.
	Wrapper string
}

// UsageSleeper waits for gap or until ctx ends, reporting false when the
// context ended first. It is a seam so a test can exercise the spacing on
// probeAll without spending a real minute on it.
type UsageSleeper func(ctx context.Context, gap time.Duration) bool

// UsageOptions carries the seams the factory fills, each a value the runner
// cannot discover for itself: the host table, the environment PATH and HOME
// come from, the ssh program, the Codex handshake version, the wait between
// two Claude probes, and the clock a script report's age is read on (nil is
// time.Now).
type UsageOptions struct {
	Hosts   map[string]UsageHost
	Lookup  environ.Lookup
	Sleep   UsageSleeper
	Now     func() time.Time
	SSH     string
	Version string
}

// usageLauncher pairs one configured launcher with the protocol it speaks.
type usageLauncher struct {
	name     string
	protocol string
}

// Usage reports the rate-limit windows of the model CLI logins on this host or
// a configured one. It speaks each CLI's own stdio protocol through the
// operator's launcher, so the launcher owns identity, or with script set reads
// that script's report instead.
//
// Config: timeout plus claude, codex, claude_gap or script, output, interval,
// host. Params: host, launchers.
type Usage struct {
	hosts     map[string]UsageHost
	lookup    environ.Lookup
	sleep     UsageSleeper
	now       func() time.Time
	script    *usageScript
	ssh       string
	version   string
	launchers []usageLauncher
	timeout   time.Duration
	claudeGap time.Duration
	resetDue  usageResetRule
}

// usageResetRule is when a login counts as reset-due: under percent of its
// week left with more than lead to go before the week resets on its own.
type usageResetRule struct {
	lead    time.Duration
	percent float64
}

// UsageReport is one entry per login, naming the host only across an ssh hop.
// Summary, HasErrors and Failures let a workflow post the table, branch on a
// failure, and name failed logins without walking the list.
// WrittenAt and Notice come only from a script's file: when it was written,
// and a warning once that is too long ago to trust.
type UsageReport struct {
	WrittenAt time.Time `json:"written_at,omitzero"`
	Host      string    `json:"host,omitempty"`
	Summary   string    `json:"summary"`
	Notice    string    `json:"notice"`
	Failures  string    `json:"failures"`

	// ResetDue names the logins worth spending a banked reset on, one line
	// each the way Failures names failed ones, and HasResetDue flags it.
	ResetDue    string       `json:"reset_due"`
	Logins      []UsageLogin `json:"logins"`
	HasErrors   bool         `json:"has_errors"`
	HasResetDue bool         `json:"has_reset_due"`
}

// UsageLogin is one launcher's answer. A failed probe fills Error and leaves
// the rest zero, because one dead login must not cost the report the others.
// ReadAt and Score come only from a report file: when the account was read and
// the script's ranking score.
type UsageLogin struct {
	ReadAt   time.Time     `json:"read_at,omitzero"`
	Score    *float64      `json:"score,omitempty"`
	Launcher string        `json:"launcher"`
	Protocol string        `json:"protocol"`
	Plan     string        `json:"plan"`
	Error    string        `json:"error,omitempty"`
	Windows  []UsageWindow `json:"windows"`

	// ResetCredits is the expiry of each banked reset still unexpired, from a
	// report file or a Codex reply. Claude banks none.
	ResetCredits []time.Time `json:"reset_credits,omitempty"`
}

// UsageWindow is one rate-limit window. UsedPercent is the share of the window
// already spent, on the CLI's own 0-100 scale.
type UsageWindow struct {
	ResetsAt    time.Time `json:"resets_at"`
	Name        string    `json:"name"`
	UsedPercent float64   `json:"used_percent"`
}

// NewUsage decodes the runner config block. An absent block is the ordinary
// case and yields the two default launchers with the default timeout, so a host
// that never wrote the block still answers for whatever is on its PATH. A block
// with script set reads that script's report and decodes no launchers.
func NewUsage(cfg map[string]any, opts UsageOptions) (*Usage, error) {
	script, err := decodeUsageScript(cfg, opts.Hosts)
	if err != nil {
		return nil, &configError{runner: UsageName, err: err}
	}

	fallback := defaultUsageTimeout
	if script != nil {
		fallback = defaultUsageScriptTimeout
	}

	timeout, err := decodeUsageTimeout(cfg, fallback)
	if err != nil {
		return nil, &configError{runner: UsageName, err: err}
	}

	resetDue, err := decodeUsageResetRule(cfg)
	if err != nil {
		return nil, &configError{runner: UsageName, err: err}
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	usage := &Usage{
		script:   script,
		ssh:      usageSSHCommand(opts.SSH),
		version:  usageVersion(opts.Version),
		hosts:    opts.Hosts,
		lookup:   opts.Lookup,
		sleep:    usageSleeper(opts.Sleep),
		now:      now,
		timeout:  timeout,
		resetDue: resetDue,
	}

	if script != nil {
		return usage, nil
	}

	usage.claudeGap, err = decodeUsageClaudeGap(cfg)
	if err != nil {
		return nil, &configError{runner: UsageName, err: err}
	}

	usage.launchers, err = decodeUsageLaunchers(cfg)
	if err != nil {
		return nil, &configError{runner: UsageName, err: err}
	}

	return usage, nil
}

// UsageFactory adapts the runner to the registry contract, closing over the
// wiring seams so BuildAll can hand the factory nothing but the config block.
func UsageFactory(opts UsageOptions) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		return NewUsage(cfg, opts)
	}
}

// Name implements runner.Runner.
func (*Usage) Name() string { return UsageName }

// Validate implements runner.Runner. A literal host or launcher name is checked
// against config at workflow load, so a typo fails before anything runs; a value
// still carrying a reference waits for the launch-time pass.
func (u *Usage) Validate(task *runner.Task) error {
	if err := optionalStringParam(task, paramUsageHost, UsageName); err != nil {
		return err
	}

	if err := u.validateHost(task); err != nil {
		return err
	}

	return u.validateLaunchers(task)
}

// Execute implements runner.Runner. The report leaves as a JSON-shaped map,
// which is what lets a downstream action read ${{ action.x.output.summary }};
// an interpolation reference has no way to walk a Go struct.
func (u *Usage) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	start := time.Now()

	report, err := u.Report(ctx, task)
	if err != nil {
		return runner.Result{}, err
	}

	output, err := report.Map()
	if err != nil {
		return runner.Result{}, runner.Wrapf(err, runner.ClassPermanent, codeUsageUnavailable, "%v", err)
	}

	meta := make(map[string]string, len(report.Logins))

	for index := range report.Logins {
		login := &report.Logins[index]
		meta[login.Launcher] = login.summary()
	}

	return runner.Result{
		Output: output,
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   meta,
	}, nil
}

// Report probes every selected launcher in turn. A launcher that fails is
// recorded in its own Error rather than failing the whole probe: a box where
// only codex is logged in should still report codex. `cairnos usage` calls this
// directly, so it renders the struct rather than the map Execute returns.
func (u *Usage) Report(ctx context.Context, task *runner.Task) (UsageReport, error) {
	if err := u.Validate(task); err != nil {
		return UsageReport{}, err
	}

	if u.script != nil {
		return u.readReport(ctx, task)
	}

	host, _ := task.Params[paramUsageHost].(string)
	probes := u.probeAll(ctx, task, host, u.selected(task))

	report := UsageReport{Host: host, Logins: make([]UsageLogin, 0, len(probes))}

	for index := range probes {
		report.Logins = append(report.Logins, probes[index].login)
	}

	report.finish(u.resetDue, u.now())

	return report, usageOutcome(probes)
}

// ClaudeGap reports the wait between two Claude probes and how many of the
// launchers this task selects will pay it, so a caller can say up front that a
// run is waiting on the cache rather than hung.
func (u *Usage) ClaudeGap(task *runner.Task) (time.Duration, int) {
	var claude int

	for _, launcher := range u.selected(task) {
		if launcher.protocol == cfgKeyUsageClaude {
			claude++
		}
	}

	return u.claudeGap, claude
}

// Map renders the report as the JSON object it serializes to, which is the
// shape every other structured built-in returns and the only shape a workflow
// reference can read a field out of.
func (r *UsageReport) Map() (map[string]any, error) {
	encoded, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode %s report: %w", UsageName, err)
	}

	var fields map[string]any

	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, fmt.Errorf("decode %s report: %w", UsageName, err)
	}

	return fields, nil
}

// String renders one line per login, the compact form a log line or a preview
// cell reads without any table layout.
func (r *UsageReport) String() string {
	lines := make([]string, 0, len(r.Logins)+1)

	if r.Notice != "" {
		lines = append(lines, r.Notice)
	}

	for index := range r.Logins {
		login := &r.Logins[index]
		lines = append(lines, r.prefix()+login.Launcher+": "+login.summary())
	}

	return strings.Join(lines, "\n")
}

// Rows lays the report out header first, one row per window. A login with no
// windows gets a short row whose last cell says why and spans the window columns.
// The score and credit columns appear only when some login carries them, so a
// probe's table keeps its old width.
func (r *UsageReport) Rows() [][]string {
	header := []string{"LOGIN", "PLAN", "WINDOW", "USED", "RESETS"}

	ranked := slices.ContainsFunc(r.Logins, func(login UsageLogin) bool {
		return login.Score != nil || len(login.ResetCredits) > 0
	})
	if ranked {
		header = append(header, "SCORE", "CREDITS")
	}

	rows := make([][]string, 0, len(r.Logins)+1)
	rows = append(rows, header)

	for index := range r.Logins {
		rows = append(rows, r.Logins[index].rows(ranked)...)
	}

	return rows
}

// finish fills the fields derived from the logins, so a probe and a report file
// render the same summary. A notice leads it, where a reader meets it first.
func (r *UsageReport) finish(rule usageResetRule, now time.Time) {
	r.Summary = markdownTable(r.Rows())

	if r.Notice != "" {
		r.Summary = r.Notice + "\n" + r.Summary
	}

	r.HasErrors = slices.ContainsFunc(r.Logins, func(login UsageLogin) bool { return login.Error != "" })
	r.Failures = r.failureLines()
	r.ResetDue = r.resetDueLines(rule, now)
	r.HasResetDue = r.ResetDue != ""
}

// resetDueLines is one line per login that rule calls reset-due, in the shape
// failureLines uses, so one alert template can carry either.
func (r *UsageReport) resetDueLines(rule usageResetRule, now time.Time) string {
	lines := make([]string, 0, len(r.Logins))

	for index := range r.Logins {
		if line, due := r.Logins[index].resetDueLine(rule, now); due {
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, "\n")
}

// resetDueLine describes a login running low on its week with the reset still
// far off while it holds a banked reset. Only a window named 7d counts, since a
// shorter one refills on its own before a banked reset would matter.
func (l *UsageLogin) resetDueLine(rule usageResetRule, now time.Time) (string, bool) {
	if l.Error != "" || len(l.ResetCredits) == 0 {
		return "", false
	}

	index := slices.IndexFunc(l.Windows, func(window UsageWindow) bool { return window.Name == windowSevenDay })
	if index < 0 {
		return "", false
	}

	week := l.Windows[index]
	remaining := usageFullPercent - week.UsedPercent
	untilReset := week.ResetsAt.Sub(now)

	if remaining >= rule.percent || untilReset <= rule.lead {
		return "", false
	}

	firstExpiry := slices.MinFunc(l.ResetCredits, time.Time.Compare)

	return fmt.Sprintf("%s login %s: %s%% of the week left, %s days to the reset, %s, the first expires in %s days",
		l.Protocol, l.Launcher, oneDecimal(remaining), usageDays(untilReset),
		bankedResets(len(l.ResetCredits)), usageDays(firstExpiry.Sub(now))), true
}

// usageDays renders a span in days to one decimal, the grain an operator
// deciding whether to spend a reset reads at.
func usageDays(span time.Duration) string {
	return oneDecimal(span.Hours() / hoursPerDay)
}

// oneDecimal renders a value with exactly one decimal place.
func oneDecimal(value float64) string {
	return strconv.FormatFloat(value, 'f', 1, 64)
}

// bankedResets counts the credits with the noun agreeing.
func bankedResets(count int) string {
	if count == 1 {
		return "1 banked reset"
	}

	return strconv.Itoa(count) + " banked resets"
}

// failureLines is one line per failed login. A CLI's error can span lines, so
// its whitespace collapses to keep each failure on the line that names it.
func (r *UsageReport) failureLines() string {
	lines := make([]string, 0, len(r.Logins))

	for index := range r.Logins {
		if login := &r.Logins[index]; login.Error != "" {
			reason := strings.Join(strings.Fields(login.Error), " ")
			lines = append(lines, login.Protocol+" login "+login.Launcher+": "+reason)
		}
	}

	return strings.Join(lines, "\n")
}

// prefix names the host on every line rather than on a header of its own, so a
// truncated preview still says which machine the numbers came from.
func (r *UsageReport) prefix() string {
	if r.Host == "" {
		return ""
	}

	return r.Host + "/"
}

// summary is the one-line form that lands in Meta and in String. A failed probe
// reports its reason there instead of an empty window list.
func (l *UsageLogin) summary() string {
	if l.Error != "" {
		return "error: " + l.Error
	}

	if len(l.Windows) == 0 {
		return "no windows"
	}

	parts := make([]string, 0, len(l.Windows))

	for _, window := range l.Windows {
		parts = append(parts, window.Name+" "+formatPercent(window.UsedPercent)+"%")
	}

	return strings.Join(parts, " ")
}

// rows names the launcher, plan, score and credits on the first window only, so
// one login reads as one block. The base name drops an install path that would
// only widen the column.
func (l *UsageLogin) rows(ranked bool) [][]string {
	name := path.Base(l.Launcher)

	if len(l.Windows) == 0 {
		return [][]string{{name, l.Plan, l.summary()}}
	}

	rows := make([][]string, 0, len(l.Windows))
	plan := l.Plan
	rank := l.rankCells()

	for _, window := range l.Windows {
		row := []string{
			name, plan, window.Name, formatPercent(window.UsedPercent) + "%",
			window.ResetsAt.UTC().Format(usageResetLayout),
		}

		if ranked {
			row = append(row, rank...)
		}

		rows = append(rows, row)
		name, plan, rank = "", "", []string{"", ""}
	}

	return rows
}

// rankCells shows the credits as a count because their expiry times ride in
// the JSON. A login the script left unscored shows a dash rather than a
// misleading zero.
func (l *UsageLogin) rankCells() []string {
	score := "-"
	if l.Score != nil {
		score = oneDecimal(*l.Score)
	}

	return []string{score, strconv.Itoa(len(l.ResetCredits))}
}

// markdownTable renders rows as a padded pipe table inside a code fence, because
// chat clients draw a bare pipe table in proportional type or drop it.
func markdownTable(rows [][]string) string {
	widths := columnWidths(rows)

	var table strings.Builder

	table.WriteString("```\n")

	for index, row := range rows {
		writeMarkdownRow(&table, row, widths)

		if index == 0 {
			writeMarkdownRule(&table, widths)
		}
	}

	table.WriteString("```")

	return table.String()
}

// columnWidths measures every cell but a short row's last, which spans the rest of
// the line the way tabwriter treats an unterminated cell.
func columnWidths(rows [][]string) []int {
	widths := make([]int, len(rows[0]))

	for _, row := range rows {
		measured := row
		if len(row) < len(widths) {
			measured = row[:len(row)-1]
		}

		for index, cell := range measured {
			widths[index] = max(widths[index], len(cell))
		}
	}

	return widths
}

// writeMarkdownRow pads each cell to its column. A short row's last cell is
// written as is, since it spans the columns it does not fill.
func writeMarkdownRow(table *strings.Builder, row []string, widths []int) {
	table.Grow(markdownRowWidth(widths))
	table.WriteString("|")

	for index, cell := range row {
		var pad int

		if len(row) == len(widths) || index < len(row)-1 {
			pad = widths[index] - len(cell)
		}

		table.WriteByte(' ')
		table.WriteString(cell)
		table.WriteString(strings.Repeat(" ", pad))
		table.WriteString(" |")
	}

	table.WriteString("\n")
}

// markdownCellPadding is the space on either side of a cell, which the rule under
// the header has to span too.
const markdownCellPadding = 2

// markdownRowWidth is the byte length of one padded row or rule, so each write
// grows the builder once instead of per cell.
func markdownRowWidth(widths []int) int {
	total := len("|\n")
	for _, width := range widths {
		total += width + markdownCellPadding + len("|")
	}

	return total
}

// writeMarkdownRule writes the header separator a Markdown table requires.
func writeMarkdownRule(table *strings.Builder, widths []int) {
	table.Grow(markdownRowWidth(widths))
	table.WriteString("|")

	for _, width := range widths {
		table.WriteString(strings.Repeat("-", width+markdownCellPadding))
		table.WriteByte('|')
	}

	table.WriteString("\n")
}

// formatPercent prints a whole number without a trailing ".0" and keeps a
// fractional one intact, since the CLIs report both shapes.
func formatPercent(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// validateHost checks the optional host param against the configured table. An
// empty value reads as "run here", which is what an unset workflow input
// resolves to.
func (u *Usage) validateHost(task *runner.Task) error {
	name, _ := task.Params[paramUsageHost].(string)
	if name == "" {
		return nil
	}

	if !task.Resolved && containsRefDelimiter(name) {
		return nil
	}

	if u.script != nil && name != u.script.host {
		return taskError(runner.ClassPermanent, codeUsageUnknownHost,
			"[runner.%s] reads its report from %s, so it has none for host %s", UsageName, u.script.where(), name)
	}

	host, known := u.hosts[name]
	if !known {
		return taskError(runner.ClassPermanent, codeUsageUnknownHost,
			"no [host.%s] block is configured", name)
	}

	if host.SSH == "" {
		return taskError(runner.ClassPermanent, codeUsageHostNoSSH,
			"host %s has no ssh alias, so %s cannot reach it", name, UsageName)
	}

	return nil
}

// validateLaunchers checks the optional narrowing list. Every entry has to name
// a configured launcher, because a typo that silently probed nothing would look
// exactly like a host with no logins.
func (u *Usage) validateLaunchers(task *runner.Task) error {
	raw, exists := task.Params[paramUsageLaunchers]
	if !exists {
		return nil
	}

	if deferred, isText := raw.(string); isText && !task.Resolved && containsRefDelimiter(deferred) {
		return nil
	}

	items, isList := raw.([]any)
	if !isList {
		return paramError(UsageName, paramUsageLaunchers, "must be a list of strings")
	}

	// An empty list narrows the run to nothing, which reads at a glance like
	// "probe them all". Refusing it at load beats a run that quietly did none.
	if len(items) == 0 {
		return paramError(UsageName, paramUsageLaunchers, "must name at least one configured launcher")
	}

	for index, item := range items {
		name, isString := item.(string)
		if !isString {
			return taskError(runner.ClassPermanent, codeUsageBadLaunchers,
				"%s[%d] must be a string, got %T", paramUsageLaunchers, index, item)
		}

		if err := u.checkKnownLauncher(task, name); err != nil {
			return err
		}
	}

	return nil
}

// checkKnownLauncher refuses one name the config never listed, deferring while
// it still carries a reference. A script's report names its own launchers, so
// in that mode the name is checked against the file when it is read.
func (u *Usage) checkKnownLauncher(task *runner.Task, name string) error {
	if u.script != nil || (!task.Resolved && containsRefDelimiter(name)) {
		return nil
	}

	if slices.ContainsFunc(u.launchers, func(l usageLauncher) bool { return l.name == name }) {
		return nil
	}

	return taskError(runner.ClassPermanent, codeUsageUnknownLauncher,
		"launcher %s is not listed in [runner.%s]", name, UsageName)
}

// selected narrows the configured launchers to the ones the task asked for,
// keeping config order so two runs report in the same sequence.
func (u *Usage) selected(task *runner.Task) []usageLauncher {
	items, isList := task.Params[paramUsageLaunchers].([]any)
	if !isList {
		return u.launchers
	}

	wanted := make(map[string]struct{}, len(items))

	for _, item := range items {
		if name, isString := item.(string); isString {
			wanted[name] = struct{}{}
		}
	}

	chosen := make([]usageLauncher, 0, len(wanted))

	for _, launcher := range u.launchers {
		if _, ok := wanted[launcher.name]; ok {
			chosen = append(chosen, launcher)
		}
	}

	return chosen
}

// usageProbe is one launcher's outcome. The two flags ride alongside the login
// because the report carries a message while the error class needs to know
// whether another run could ever do better: a missing binary and a login the
// CLI itself turned down are both settled, a timeout or a broken hop is not.
type usageProbe struct {
	login    UsageLogin
	notFound bool
	refused  bool
}

// probeAll runs the selected launchers in order, spacing consecutive Claude
// probes apart. They stay sequential on purpose: Claude Code caches its
// get_usage answer in the shared config directory for about a minute and does
// not key it by login, so two probes running together would report one login's
// numbers twice.
func (u *Usage) probeAll(
	ctx context.Context, task *runner.Task, host string, selected []usageLauncher,
) []usageProbe {
	probes := make([]usageProbe, 0, len(selected))

	var claudeProbed bool

	for index, launcher := range selected {
		if launcher.protocol == cfgKeyUsageClaude {
			if claudeProbed && !u.waitClaudeGap(ctx, task) {
				return append(probes, unprobed(selected[index:])...)
			}

			claudeProbed = true
		}

		probes = append(probes, u.probe(ctx, task, host, launcher))
	}

	return probes
}

// waitClaudeGap spaces two Claude probes apart, honoring the task deadline as
// well as ctx. A wait cut short answers false, which leaves the launchers still
// to come unprobed rather than reporting the previous login's cached numbers.
func (u *Usage) waitClaudeGap(ctx context.Context, task *runner.Task) bool {
	if u.claudeGap <= 0 {
		return true
	}

	waitCtx := ctx

	if !task.Deadline.IsZero() {
		var cancel context.CancelFunc

		waitCtx, cancel = context.WithDeadline(ctx, task.Deadline)
		defer cancel()
	}

	return u.sleep(waitCtx, u.claudeGap)
}

// unprobed records the launchers a cut-short wait never reached, so the report
// says they were skipped instead of leaving them out of it.
func unprobed(launchers []usageLauncher) []usageProbe {
	probes := make([]usageProbe, 0, len(launchers))

	for _, launcher := range launchers {
		probes = append(probes, usageProbe{login: UsageLogin{
			Launcher: launcher.name,
			Protocol: launcher.protocol,
			Error:    errUsageUnprobed.Error(),
		}})
	}

	return probes
}

// probe runs one launcher under its own deadline and turns the exchange into a
// report entry.
func (u *Usage) probe(ctx context.Context, task *runner.Task, host string, launcher usageLauncher) usageProbe {
	login := UsageLogin{Launcher: launcher.name, Protocol: launcher.protocol}

	dialog, err := usageDialogFor(launcher.protocol, u.version)
	if err != nil {
		login.Error = err.Error()

		return usageProbe{login: login, refused: true}
	}

	runCtx, cancel := u.runContext(ctx, task)
	defer cancel()

	detail, err := u.converse(runCtx, cancel, host, launcher.name, dialog)
	if err != nil {
		login.Error = err.Error()

		return usageProbe{login: login, notFound: isMissingCommand(err), refused: isRefusal(err)}
	}

	login.Plan = detail.plan
	login.Windows = detail.windows
	login.ResetCredits = unexpiredCredits(detail.resetCredits, u.now())

	return usageProbe{login: login}
}

// runContext derives the effective deadline: the config timeout as the
// backstop, Task.Deadline when the engine set an earlier one.
func (u *Usage) runContext(ctx context.Context, task *runner.Task) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(u.timeout)

	if !task.Deadline.IsZero() && task.Deadline.Before(deadline) {
		deadline = task.Deadline
	}

	return context.WithDeadline(ctx, deadline)
}

// usageOutcome decides the action's own result. A report with one live login is
// a success; only a clean sweep of failures is an error, and it is permanent
// when no retry could ever find the binaries.
func usageOutcome(probes []usageProbe) error {
	if len(probes) == 0 {
		return taskError(runner.ClassPermanent, codeUsageUnavailable,
			"no launchers are configured for %s", UsageName)
	}

	reasons := make([]string, 0, len(probes))
	settled := true

	for index := range probes {
		probe := &probes[index]
		if probe.login.Error == "" {
			return nil
		}

		reasons = append(reasons, probe.login.Launcher+": "+probe.login.Error)

		// A binary that is not there and a login the CLI turned down are both
		// facts another run would meet again. A timeout, a broken ssh hop, or
		// any other start failure is about the environment, so it is worth one.
		settled = settled && (probe.notFound || probe.refused)
	}

	class := runner.ClassTransient
	if settled {
		class = runner.ClassPermanent
	}

	return taskError(class, codeUsageUnavailable,
		"no login reported usage: %s", strings.Join(reasons, "; "))
}

// unexpiredCredits keeps the credits that expire after now, the same cut the
// report file's credits get, so both modes count a banked reset alike.
func unexpiredCredits(expiries []time.Time, now time.Time) []time.Time {
	return slices.DeleteFunc(expiries, func(expiry time.Time) bool { return !expiry.After(now) })
}

// isRefusal reports whether a probe failed on something the login or the config
// decides, rather than on the environment it ran in.
func isRefusal(err error) bool {
	return errors.Is(err, errUsageRefused) ||
		errors.Is(err, errUsageNoLimits) ||
		errors.Is(err, errUsageProtocol) ||
		errors.Is(err, errUsageBadWindow)
}

// isMissingCommand reports whether a probe failed because the launcher, or ssh
// itself, is not installed where it was looked for.
func isMissingCommand(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

// decodeUsageTimeout reads the wall-clock bound on one launcher probe or one
// script run, in seconds. A probe that never returns would hold the action open past its
// schedule, so the bound applies whether or not the operator wrote one.
func decodeUsageTimeout(cfg map[string]any, fallback time.Duration) (time.Duration, error) {
	seconds, present, err := configInt(cfg, cfgKeyTimeout)
	if err != nil {
		return 0, err
	}

	if !present {
		return fallback, nil
	}

	if seconds <= 0 {
		return 0, fmt.Errorf("%w: %s must be a positive number of seconds", ErrConfigValue, cfgKeyTimeout)
	}

	return time.Duration(seconds) * time.Second, nil
}

// decodeUsageClaudeGap reads the spacing between two Claude probes. Zero is
// allowed and means no wait, which is right for an operator whose launchers
// give each login a config directory of its own.
func decodeUsageClaudeGap(cfg map[string]any) (time.Duration, error) {
	seconds, present, err := configInt(cfg, cfgKeyUsageClaudeGap)
	if err != nil {
		return 0, err
	}

	if !present {
		return defaultUsageClaudeGap, nil
	}

	if seconds < 0 {
		return 0, fmt.Errorf("%w: %s must not be negative", ErrConfigValue, cfgKeyUsageClaudeGap)
	}

	return time.Duration(seconds) * time.Second, nil
}

// decodeUsageResetRule reads the two reset-due thresholds. Both modes take
// them, since a report file and a probe both carry the windows and credits the
// rule reads.
func decodeUsageResetRule(cfg map[string]any) (usageResetRule, error) {
	percent, present, err := configFloat(cfg, cfgKeyUsageResetDuePercent)
	if err != nil {
		return usageResetRule{}, err
	}

	if !present {
		percent = defaultUsageResetDuePercent
	}

	// The negated range test also refuses NaN, which TOML can spell and which
	// would otherwise make every comparison false and the rule never fire.
	if !(percent >= 0 && percent <= usageFullPercent) {
		return usageResetRule{}, fmt.Errorf("%w: %s must be within 0 to 100", ErrConfigValue, cfgKeyUsageResetDuePercent)
	}

	lead, err := decodeUsageResetDueLead(cfg)
	if err != nil {
		return usageResetRule{}, err
	}

	return usageResetRule{lead: lead, percent: percent}, nil
}

// decodeUsageResetDueLead reads how long before the weekly reset a login stops
// counting as reset-due. It is a duration string rather than seconds because
// the useful values run to days.
func decodeUsageResetDueLead(cfg map[string]any) (time.Duration, error) {
	text, err := configValue(cfg, cfgKeyUsageResetDueLead, defaultUsageResetDueLead)
	if err != nil {
		return 0, err
	}

	lead, err := time.ParseDuration(text)
	if err != nil || lead <= 0 {
		return 0, fmt.Errorf("%w: %s must be a positive duration such as %q", ErrConfigValue,
			cfgKeyUsageResetDueLead, defaultUsageResetDueLead)
	}

	return lead, nil
}

// decodeUsageLaunchers reads both launcher lists into one ordered set. A name
// listed under both protocols is refused: the report keys Meta by launcher, so
// the second entry would overwrite the first.
func decodeUsageLaunchers(cfg map[string]any) ([]usageLauncher, error) {
	claude, err := usageLauncherList(cfg, cfgKeyUsageClaude, defaultClaudeCommand)
	if err != nil {
		return nil, err
	}

	codex, err := usageLauncherList(cfg, cfgKeyUsageCodex, defaultCodexCommand)
	if err != nil {
		return nil, err
	}

	launchers := make([]usageLauncher, 0, len(claude)+len(codex))
	seen := make(map[string]struct{}, len(claude)+len(codex))

	for _, entry := range append(claude, codex...) {
		if _, taken := seen[entry.name]; taken {
			return nil, fmt.Errorf("%w: launcher %q is listed under both %s and %s",
				ErrConfigValue, entry.name, cfgKeyUsageClaude, cfgKeyUsageCodex)
		}

		seen[entry.name] = struct{}{}

		launchers = append(launchers, entry)
	}

	return launchers, nil
}

// usageLauncherList decodes one protocol's list. An absent key takes the
// default launcher; a key written as an empty array means the operator has no
// login of that kind, which is a different thing and stays empty.
func usageLauncherList(cfg map[string]any, key, fallback string) ([]usageLauncher, error) {
	names, err := configStrings(cfg, key)
	if err != nil {
		return nil, err
	}

	if names == nil {
		names = []string{fallback}
	}

	launchers := make([]usageLauncher, 0, len(names))

	for _, name := range names {
		if err := checkUsageLauncher(key, name); err != nil {
			return nil, err
		}

		launchers = append(launchers, usageLauncher{name: name, protocol: key})
	}

	return launchers, nil
}

// checkUsageLauncher holds one entry to a bare name or a resolvable path. A
// relative path is refused because it would resolve against whatever working
// directory the probe happened to have, which config cannot promise.
func checkUsageLauncher(key, name string) error {
	if !usageLauncherPattern.MatchString(name) {
		return fmt.Errorf("%w: %s entry %q must match %s", ErrConfigValue, key, name, usageLauncherPattern)
	}

	if strings.HasSuffix(name, "/") {
		return fmt.Errorf("%w: %s entry %q must not end with a slash", ErrConfigValue, key, name)
	}

	if !strings.Contains(name, "/") {
		return nil
	}

	if !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, usageHomePrefix) {
		return fmt.Errorf("%w: %s entry %q must be a bare name, an absolute path, or start with %q",
			ErrConfigValue, key, name, usageHomePrefix)
	}

	return nil
}

// usageSSHCommand picks the program that opens the remote hop. An empty name
// is the ordinary case: only a caller substituting its own program passes one.
func usageSSHCommand(configured string) string {
	if configured == "" {
		return "ssh"
	}

	return configured
}

// usageSleeper falls back to a real wait. Naming one is a caller's way of
// keeping the gap off the clock.
func usageSleeper(configured UsageSleeper) UsageSleeper {
	if configured != nil {
		return configured
	}

	return func(ctx context.Context, gap time.Duration) bool {
		timer := time.NewTimer(gap)
		defer timer.Stop()

		select {
		case <-timer.C:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

// usageVersion falls back to a placeholder the Codex handshake accepts, for the
// callers that have no build version to report.
func usageVersion(configured string) string {
	if configured == "" {
		return "0.0.0"
	}

	return configured
}
