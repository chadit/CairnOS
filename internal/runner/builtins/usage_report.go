package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// Config keys of the report-file source. Serve runs a script that writes a
// cairnos.usage.v1 report on an interval, and a request reads that file
// instead of probing every login, so it answers at once.
const (
	cfgKeyUsageScript   = "script"
	cfgKeyUsageOutput   = "output"
	cfgKeyUsageInterval = "interval"
	cfgKeyUsageHost     = "host"
)

// usageOutputToken, as a whole script element, stands for the output path, so
// the block names the path once and the script and the reader cannot disagree.
const usageOutputToken = "{output}"

// usageReportSchema is the one report layout this build reads. A newer layout
// may move fields this parser relies on, so it is refused, not guessed at.
const usageReportSchema = "cairnos.usage.v1"

// usageStaleIntervals is how many refresh intervals an account's reading may
// age, plus one run's timeout, before a request flags it. One missed run is
// tolerated; a second means the refresh is broken. The timeout is added because
// a script may stamp a reading when its run starts, and the file lands only
// when the run ends.
const usageStaleIntervals = 2

// codeUsageBadReport marks a report file this build cannot read.
const codeUsageBadReport = UsageName + ".bad_report"

// usageScript is the report-file source: the command serve runs on the
// interval, the host it runs on (empty for this machine), and the file it
// writes there.
type usageScript struct {
	host     string
	output   string
	argv     []string
	interval time.Duration
}

// usageReportFile is the cairnos.usage.v1 report. Epochs decode as float64
// because a script may write fractional seconds, as Python's time.time() does.
type usageReportFile struct {
	Schema    string               `json:"schema"`
	Accounts  []usageReportAccount `json:"accounts"`
	WrittenAt float64              `json:"written_at"`
}

// usageReportAccount is one account's entry. A failed read fills Error and
// leaves the windows out, the same shape a failed probe has.
type usageReportAccount struct {
	Score        *float64            `json:"score"`
	Tool         string              `json:"tool"`
	Wrapper      string              `json:"wrapper"`
	Plan         string              `json:"plan"`
	Error        string              `json:"error"`
	Windows      []usageReportWindow `json:"windows"`
	ResetCredits []float64           `json:"reset_credit_expiries"`
	ReadAt       float64             `json:"read_at"`
}

// usageReportWindow keeps only the fields UsageWindow carries: the remaining
// share and seconds to reset follow from these.
type usageReportWindow struct {
	Name        string  `json:"name"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    float64 `json:"resets_at"`
}

// decodeUsageScript reads the report-file source, returning nil for a block
// that probes launchers. The two sources are exclusive, and a key belonging to
// the one not chosen is refused rather than ignored, since it would otherwise
// look like it did something.
func decodeUsageScript(cfg map[string]any, hosts map[string]UsageHost) (*usageScript, error) {
	argv, err := configStrings(cfg, cfgKeyUsageScript)
	if err != nil {
		return nil, err
	}

	if argv == nil {
		return nil, refuseUsageKeys(cfg, "without "+cfgKeyUsageScript,
			cfgKeyUsageOutput, cfgKeyUsageInterval, cfgKeyUsageHost)
	}

	err = refuseUsageKeys(cfg, "beside "+cfgKeyUsageScript+", which replaces the launcher settings",
		cfgKeyUsageClaude, cfgKeyUsageCodex, cfgKeyUsageClaudeGap)
	if err != nil {
		return nil, err
	}

	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("%w: %s must start with a command", ErrConfigValue, cfgKeyUsageScript)
	}

	script := &usageScript{argv: argv}

	if script.output, err = decodeUsageOutput(cfg); err != nil {
		return nil, err
	}

	if script.interval, err = decodeUsageInterval(cfg); err != nil {
		return nil, err
	}

	if script.host, err = decodeUsageScriptHost(cfg, hosts); err != nil {
		return nil, err
	}

	return script, nil
}

// refuseUsageKeys fails when the block sets any of keys, naming each one. The
// message reads "<keys> set <why>", which holds for one key or several.
func refuseUsageKeys(cfg map[string]any, why string, keys ...string) error {
	present := slices.DeleteFunc(slices.Clone(keys), func(key string) bool {
		_, set := cfg[key]

		return !set
	})

	if len(present) == 0 {
		return nil
	}

	return fmt.Errorf("%w: %s set %s", ErrConfigValue, strings.Join(present, ", "), why)
}

// decodeUsageOutput reads the report path. A relative path is refused for the
// reason a relative launcher is: it would resolve against whatever working
// directory the script or serve happened to have.
func decodeUsageOutput(cfg map[string]any) (string, error) {
	output, err := configValue(cfg, cfgKeyUsageOutput, "")
	if err != nil {
		return "", err
	}

	if output == "" {
		return "", fmt.Errorf("%w: %s needs %s, the file the script writes", ErrConfigValue, cfgKeyUsageScript, cfgKeyUsageOutput)
	}

	if !strings.HasPrefix(output, "/") && !strings.HasPrefix(output, usageHomePrefix) {
		return "", fmt.Errorf("%w: %s %q must be an absolute path or start with %q",
			ErrConfigValue, cfgKeyUsageOutput, output, usageHomePrefix)
	}

	return output, nil
}

// decodeUsageInterval reads the refresh period in seconds, like the block's
// other durations. There is no default: how fresh the numbers must be is the
// operator's call, and it also sets when a reading counts as stale.
func decodeUsageInterval(cfg map[string]any) (time.Duration, error) {
	seconds, present, err := configInt(cfg, cfgKeyUsageInterval)
	if err != nil {
		return 0, err
	}

	if !present || seconds <= 0 {
		return 0, fmt.Errorf("%w: %s needs %s, a positive number of seconds", ErrConfigValue, cfgKeyUsageScript, cfgKeyUsageInterval)
	}

	return time.Duration(seconds) * time.Second, nil
}

// decodeUsageScriptHost reads the host the script runs on and its file lives
// on, held to the same checks a host param gets, but at config load, since no
// request names it.
func decodeUsageScriptHost(cfg map[string]any, hosts map[string]UsageHost) (string, error) {
	name, err := configValue(cfg, cfgKeyUsageHost, "")
	if err != nil || name == "" {
		return "", err
	}

	host, known := hosts[name]
	if !known {
		return "", fmt.Errorf("%w: %s %q has no [host.%s] block", ErrConfigValue, cfgKeyUsageHost, name, name)
	}

	if host.SSH == "" {
		return "", fmt.Errorf("%w: %s %q has no ssh alias to run the script over", ErrConfigValue, cfgKeyUsageHost, name)
	}

	return name, nil
}

// where names the machine the script's file lives on, for messages.
func (s *usageScript) where() string {
	if s.host == "" {
		return "this machine"
	}

	return "host " + s.host
}

// args is the script's argv with the output element filled in. A "~/" is left
// in place, since only the machine that runs the script knows its home.
func (s *usageScript) args() []string {
	args := slices.Clone(s.argv)

	for index, arg := range args {
		if arg == usageOutputToken {
			args[index] = s.output
		}
	}

	return args
}

// staleNotice names the accounts whose reading is older than the bound, so a
// reader is told which numbers are old instead of trusting them. It goes by each
// account's read_at rather than the file's written_at, because a script may
// merge a run's readings into the file and keep the entries it did not read.
func (u *Usage) staleNotice(logins []UsageLogin, now time.Time) string {
	bound := usageStaleIntervals*u.script.interval + u.timeout

	var (
		stale  []string
		oldest time.Time
	)

	for index := range logins {
		login := &logins[index]
		if now.Sub(login.ReadAt) <= bound {
			continue
		}

		stale = append(stale, login.Launcher)

		if oldest.IsZero() || login.ReadAt.Before(oldest) {
			oldest = login.ReadAt
		}
	}

	if len(stale) == 0 {
		return ""
	}

	return fmt.Sprintf("stale: %s last read %s ago or earlier (%s); the script is set to run every %s, "+
		"so the refresh is failing or serve is not running it",
		strings.Join(stale, ", "), now.Sub(oldest).Round(time.Minute), oldest.UTC().Format(usageResetLayout), u.script.interval)
}

// RefreshInterval is how often serve runs the report script, zero when the
// block probes launchers and there is nothing to refresh.
func (u *Usage) RefreshInterval() time.Duration {
	if u.script == nil {
		return 0
	}

	return u.script.interval
}

// RunRefresh runs the report script now and again on every tick until ctx ends,
// returning at once for a block that probes launchers. A failed run is logged
// and the loop waits for the next tick: the file it did not replace ages into
// the stale notice, which is how a reader learns the refresh is broken.
func (u *Usage) RunRefresh(ctx context.Context, ticks <-chan time.Time, logger *slog.Logger) {
	if u.script == nil {
		return
	}

	for {
		if err := u.refresh(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("usage report script failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

// refresh runs the script once under the block's timeout. The script writes the
// file itself and nothing here reads it back: a request parses it and reports
// whatever is wrong with it.
func (u *Usage) refresh(ctx context.Context) error {
	runCtx, cancel := context.WithTimeout(ctx, u.timeout)
	defer cancel()

	path, argv, err := u.scriptCommand()
	if err != nil {
		return err
	}

	if _, err := runUsageCommand(runCtx, path, argv); err != nil {
		return fmt.Errorf("run %s script on %s: %w", UsageName, u.script.where(), err)
	}

	return nil
}

// scriptCommand picks the program and argv for one script run: the command
// itself here, or ssh carrying it to the configured host through that host's
// wrapper, the way a launcher runs there.
func (u *Usage) scriptCommand() (string, []string, error) {
	args := u.script.args()

	if u.script.host != "" {
		return u.sshCommand(u.script.host, remoteScript(u.hosts[u.script.host].Wrapper, args[0], args[1:]))
	}

	path, err := u.resolveLocal(args[0])
	if err != nil {
		return "", nil, err
	}

	local := make([]string, 0, len(args)-1)

	for _, arg := range args[1:] {
		expanded, err := u.expandHome(arg)
		if err != nil {
			return "", nil, err
		}

		local = append(local, expanded)
	}

	return path, local, nil
}

// readReport answers a request from the script's last report without running
// the script, which is what keeps a request cheap. A report too old to trust
// still comes back, with a notice leading its summary.
func (u *Usage) readReport(ctx context.Context, task *runner.Task) (UsageReport, error) {
	data, err := u.readReportFile(ctx)
	if err != nil {
		return UsageReport{}, err
	}

	now := u.now()

	report, err := parseUsageReport(data, now)
	if err != nil {
		return UsageReport{}, runner.Wrapf(err, runner.ClassPermanent, codeUsageBadReport, "%v", err)
	}

	if report.Logins, err = narrowReportLogins(report.Logins, task); err != nil {
		return UsageReport{}, err
	}

	report.Host = u.script.host
	report.Notice = u.staleNotice(report.Logins, now)
	report.finish(u.resetDue, now)

	return report, reportOutcome(report.Logins)
}

// readReportFile fetches the file off this disk, or over ssh from the host the
// script runs on, where it lives. A missing file is transient: the next
// refresh may write it.
func (u *Usage) readReportFile(ctx context.Context) ([]byte, error) {
	if u.script.host != "" {
		return u.readRemoteReport(ctx)
	}

	path, err := u.expandHome(u.script.output)
	if err != nil {
		return nil, runner.Wrapf(err, runner.ClassPermanent, codeUsageUnavailable, "%v", err)
	}

	// #nosec G304 -- the path is the operator's own [runner.usage] output key.
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		message := fmt.Sprintf("no usage report at %s yet: serve writes it by running the [runner.%s] script "+
			"every %s, so serve is not running with this block or the script has not succeeded",
			path, UsageName, u.script.interval)

		return nil, runner.Wrapf(err, runner.ClassTransient, codeUsageUnavailable, "%s", message)
	}

	if err != nil {
		return nil, runner.Wrapf(err, runner.ClassTransient, codeUsageUnavailable, "read usage report: %v", err)
	}

	return data, nil
}

// readRemoteReport cats the file over ssh. A missing file surfaces as cat's own
// "No such file or directory" in the message.
func (u *Usage) readRemoteReport(ctx context.Context) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, u.timeout)
	defer cancel()

	path, argv, err := u.sshCommand(u.script.host, "exec cat -- "+remoteWord(u.script.output))
	if err != nil {
		return nil, runner.Wrapf(err, runner.ClassPermanent, codeUsageUnavailable, "%v", err)
	}

	text, err := runUsageCommand(runCtx, path, argv)
	if err != nil {
		return nil, runner.Wrapf(err, runner.ClassTransient, codeUsageUnavailable,
			"read usage report on %s: %v", u.script.where(), err)
	}

	return []byte(text), nil
}

// runUsageCommand runs one command to completion and returns its stdout,
// bounded the way the CLI runners bound theirs.
func runUsageCommand(ctx context.Context, path string, argv []string) (string, error) {
	piped, err := startPiped(ctx, func() *exec.Cmd {
		// #nosec G204 -- path and argv come from the operator's [runner.usage]
		// block and [host.<name>] table; every remote word is shell-quoted.
		cmd := exec.CommandContext(ctx, path, argv...)
		cmd.WaitDelay = usageWaitDelay
		groupProcess(cmd)

		return cmd
	})
	if err != nil {
		return "", fmt.Errorf("start %s: %w", path, err)
	}

	stdout, scanErr := collectStdout(ctx, piped.stdout, nil)

	if err := piped.cmd.Wait(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", path, err, piped.stderr.Text())
	}

	if scanErr != nil {
		return "", scanErr
	}

	return stdout, nil
}

// parseUsageReport decodes a cairnos.usage.v1 report into the probe's report
// shape. An expired reset credit is dropped, since the file may be read long
// after it was written. An account missing a name refuses the whole file:
// unlike a failed read, which is one dead account, it means the writer is
// broken and leaves no name to report under.
func parseUsageReport(data []byte, now time.Time) (UsageReport, error) {
	var file usageReportFile

	if err := json.Unmarshal(data, &file); err != nil {
		return UsageReport{}, fmt.Errorf("%w: %w", errUsageBadReport, err)
	}

	if file.Schema != usageReportSchema {
		return UsageReport{}, fmt.Errorf("%w: schema %q is not one this build reads; want %q",
			errUsageBadReport, file.Schema, usageReportSchema)
	}

	report := UsageReport{WrittenAt: epochTime(file.WrittenAt), Logins: make([]UsageLogin, 0, len(file.Accounts))}

	for index := range file.Accounts {
		account := &file.Accounts[index]
		if err := account.validate(index); err != nil {
			return UsageReport{}, err
		}

		report.Logins = append(report.Logins, account.login(report.WrittenAt, now))
	}

	return report, nil
}

// validate refuses an account missing one of the two names the format requires.
// launchers selects by wrapper, so an account without one loads but can never
// be picked, and a blank tool leaves the login's protocol empty. The index
// names the entry because neither name can.
func (a *usageReportAccount) validate(index int) error {
	if strings.TrimSpace(a.Wrapper) == "" {
		return fmt.Errorf("%w: accounts[%d] (tool %q) has an empty wrapper; wrapper must be non-empty",
			errUsageBadReport, index, a.Tool)
	}

	if strings.TrimSpace(a.Tool) == "" {
		return fmt.Errorf("%w: accounts[%d] (wrapper %q) has an empty tool; tool must be non-empty",
			errUsageBadReport, index, a.Wrapper)
	}

	return nil
}

// login maps one account onto a report entry: the wrapper is what a caller
// launches, so it plays the launcher, and the tool names the protocol. Both are
// trimmed, as validate judged them, because launchers matches names exactly and
// a padded wrapper could never be picked. An entry without read_at counts as
// read when the file was written.
func (a *usageReportAccount) login(writtenAt, now time.Time) UsageLogin {
	readAt := writtenAt
	if a.ReadAt > 0 {
		readAt = epochTime(a.ReadAt)
	}

	login := UsageLogin{
		ReadAt:   readAt,
		Launcher: strings.TrimSpace(a.Wrapper),
		Protocol: strings.TrimSpace(a.Tool),
		Plan:     a.Plan,
		Error:    a.Error,
		Score:    a.Score,
	}

	for _, window := range a.Windows {
		login.Windows = append(login.Windows, UsageWindow{
			Name:        window.Name,
			UsedPercent: window.UsedPercent,
			ResetsAt:    epochTime(window.ResetsAt),
		})
	}

	expiries := make([]time.Time, 0, len(a.ResetCredits))

	for _, expiry := range a.ResetCredits {
		expiries = append(expiries, epochTime(expiry))
	}

	login.ResetCredits = unexpiredCredits(expiries, now)

	return login
}

// epochTime converts fractional epoch seconds, keeping whole-second precision,
// which is all a reset time or a staleness bound needs.
func epochTime(seconds float64) time.Time {
	return time.Unix(int64(seconds), 0)
}

// narrowReportLogins keeps the launchers a request asked for, in file order. A
// name the file does not carry is refused for the reason an unlisted launcher is:
// a typo that matched nothing would look like a report with no logins.
func narrowReportLogins(logins []UsageLogin, task *runner.Task) ([]UsageLogin, error) {
	items, isList := task.Params[paramUsageLaunchers].([]any)
	if !isList {
		return logins, nil
	}

	for _, item := range items {
		name, _ := item.(string)
		if !slices.ContainsFunc(logins, func(login UsageLogin) bool { return login.Launcher == name }) {
			return nil, taskError(runner.ClassPermanent, codeUsageUnknownLauncher,
				"launcher %s is not in the usage report", name)
		}
	}

	return slices.DeleteFunc(logins, func(login UsageLogin) bool {
		return !slices.Contains(items, any(login.Launcher))
	}), nil
}

// reportOutcome fails a request only when no account in the file has numbers,
// matching a probe where nothing answered. The script may fix it next run.
func reportOutcome(logins []UsageLogin) error {
	if len(logins) == 0 {
		return taskError(runner.ClassTransient, codeUsageUnavailable, "the usage report lists no accounts")
	}

	reasons := make([]string, 0, len(logins))

	for index := range logins {
		login := &logins[index]
		if login.Error == "" {
			return nil
		}

		reasons = append(reasons, login.Launcher+": "+login.Error)
	}

	return taskError(runner.ClassTransient, codeUsageUnavailable,
		"no account in the usage report has numbers: %s", strings.Join(reasons, "; "))
}
