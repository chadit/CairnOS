package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/localmodel"
	"github.com/chadit/CairnOS/internal/service"
)

const modelUsage = `usage: cairnos model <setup|status|check|upgrade|uninstall> [flags]

  setup      Download the pinned llama.cpp server and model, write a
             [runner.local] config block, and register the model server as a
             supervised sidecar. --no-service installs the files only;
             --dry-run prints the plan and touches nothing.
  status     Report installed versus pinned versions and whether the server
             answers. --json prints it as one object.
  check      Compare the compiled-in pins against upstream. Exits 0 whether or
             not anything is behind; non-zero only when a lookup fails.
  upgrade    Re-download anything whose pin moved, restart the sidecar, and
             keep the previous runtime until the new one answers.
  uninstall  Stop and deregister the sidecar. Downloads stay unless --purge.

The model runs on CPU. The default download is about 5 GB and the server
holds roughly 7-9 GB of memory at a 32K context.
`

// Flag names shared by the model subcommands.
const (
	flagModel     = "model"
	flagPort      = "port"
	flagThreads   = "threads"
	flagCtxSize   = "ctx-size"
	flagForce     = "force"
	flagOffline   = "offline"
	flagNoService = "no-service"
	flagDryRun    = "dry-run"
	flagJSON      = "json"
	flagPurge     = "purge"
)

// Bounds for the numeric flags. A port must be a real TCP port, and the two
// server knobs must be positive for llama-server to start on them.
const (
	minPort = 1
	maxPort = 65535
)

// errFlagValue reports a flag whose value cannot work. Catching it here beats
// finding out from a server that started on the wrong port: --port 0 makes
// llama-server bind an ephemeral port while every probe still targets :0, so
// an upgrade would roll back a runtime that was running fine.
var errFlagValue = errors.New("model: flag value out of range")

// stateNotInstalled is what status prints for a component that was never set
// up, on the same line the supervisor state would otherwise occupy.
const stateNotInstalled = "not installed"

// threadHeadroom is how many CPU threads the default leaves for everything
// else. A model server that claims every core starves the engine, the store,
// and whatever else shares the machine, and the tokens-per-second it buys back
// are not worth a node that stops answering while it generates.
const threadHeadroom = 2

// upgradeProbeBudget is how long an upgrade waits for the replaced server to
// answer before rolling back. A cold start maps several gigabytes of weights,
// so the budget is measured in tens of seconds rather than in seconds.
const upgradeProbeBudget = 2 * time.Minute

// localRunnerBlock is the config setup appends. type = "openai" is not a
// claim about who serves the request: llama-server speaks the same
// chat-completions API, so the built-in runner needs only the base_url to
// reach it. The model string is passed through verbatim and llama-server
// accepts any value, so it names the catalog entry for the reader's benefit.
const localRunnerBlock = `
[runner.local]
type = "openai"
model = "%s"
base_url = "%s"
`

// localRunnerHeader is the section header setup looks for before appending, so
// a second run leaves an operator's edited block alone.
const localRunnerHeader = "[runner.local]"

// modelOptions collects every model subcommand flag; each subcommand
// registers only the ones it honors.
type modelOptions struct {
	dir       string
	model     string
	port      int
	threads   int
	ctxSize   int
	force     bool
	offline   bool
	noService bool
	dryRun    bool
	jsonOut   bool
	purge     bool
}

// cmdModel dispatches the local-model subcommands.
func (a *App) cmdModel(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", modelUsage)

		return ExitUsage
	}

	switch args[0] {
	case "setup":
		return a.cmdModelInstall(ctx, "model setup", args[1:], false)
	case "upgrade":
		return a.cmdModelInstall(ctx, "model upgrade", args[1:], true)
	case subcmdStatus:
		return a.cmdModelStatus(ctx, args[1:])
	case "check":
		return a.cmdModelCheck(ctx, args[1:])
	case "uninstall":
		return a.cmdModelUninstall(ctx, args[1:])
	case "-h", "--help", "help":
		// Asking for help is not a typo, so it skips the "unknown subcommand"
		// line the default arm leads with.
		a.printErrf("%s", modelUsage)

		return ExitUsage
	default:
		a.printErrf("unknown model subcommand %q\n", args[0])
		a.printErrf("%s", modelUsage)

		return ExitUsage
	}
}

// installFlags registers the flags setup and upgrade share.
func installFlags(flags *flag.FlagSet, opts *modelOptions) {
	flags.StringVar(&opts.dir, flagDir, "", flagDirUsage)
	flags.StringVar(&opts.model, flagModel, localmodel.DefaultModel, "catalog model to install")
	flags.IntVar(&opts.port, flagPort, localmodel.DefaultPort, "loopback port the model server listens on")
	flags.IntVar(&opts.threads, flagThreads, defaultThreads(), "CPU threads the model server may use")
	flags.IntVar(&opts.ctxSize, flagCtxSize, localmodel.DefaultContextSize, "context window in tokens")
	flags.BoolVar(&opts.force, flagForce, false, "re-download and re-extract even when the pins already match")
	flags.BoolVar(&opts.offline, flagOffline, false, "never reach the network; fail when something is not cached")
	flags.BoolVar(&opts.noService, flagNoService, false, "install the files but register no supervised service")
	flags.BoolVar(&opts.dryRun, flagDryRun, false, "print the plan without touching anything")
}

// defaultThreads leaves threadHeadroom cores for the rest of the machine,
// floored at one so a single-core or dual-core host still gets a server.
func defaultThreads() int {
	threads := runtime.NumCPU() - threadHeadroom
	if threads < 1 {
		return 1
	}

	return threads
}

// cmdModelInstall runs setup and upgrade, which walk the same path: bring the
// runtime and weights to the pins, write the config block, and put the server
// under supervision. upgrade additionally proves the new server answers before
// discarding the runtime it replaced.
func (a *App) cmdModelInstall(ctx context.Context, prefix string, args []string, upgrade bool) int {
	flags := flag.NewFlagSet(prefix, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	var opts modelOptions

	installFlags(flags, &opts)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if err := validateServerFlags(&opts); err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	target, err := resolveModelTarget(&opts)
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	if opts.dryRun {
		a.printInstallPlan(prefix, target, &opts)

		return ExitOK
	}

	return a.applyInstall(ctx, prefix, target, &opts, upgrade)
}

// modelTarget is everything an install pass needs that comes from flags and
// the catalog rather than from the network.
type modelTarget struct {
	home   string
	layout localmodel.Layout
	model  localmodel.Model
	build  localmodel.Build
}

// resolveModelTarget turns the flags into the home, catalog entry, and
// platform build an install pass works against.
func resolveModelTarget(opts *modelOptions) (*modelTarget, error) {
	// Only the port is checked here. Every subcommand that resolves a target
	// carries one, while the two server knobs exist on setup and upgrade alone
	// and are checked where they are registered.
	if err := validatePortFlag(opts.port); err != nil {
		return nil, err
	}

	home, err := resolveDir(opts.dir)
	if err != nil {
		return nil, err
	}

	model, err := localmodel.Lookup(opts.model)
	if err != nil {
		return nil, fmt.Errorf("resolve model: %w", err)
	}

	build, err := localmodel.HostBuild()
	if err != nil {
		return nil, fmt.Errorf("resolve runtime build: %w", err)
	}

	return &modelTarget{home: home, layout: localmodel.NewLayout(home), model: model, build: build}, nil
}

// validatePortFlag rejects a port the server cannot listen on or the probe
// cannot reach.
func validatePortFlag(port int) error {
	if port < minPort || port > maxPort {
		return fmt.Errorf("%w: --%s must be between %d and %d, got %d",
			errFlagValue, flagPort, minPort, maxPort, port)
	}

	return nil
}

// validateServerFlags rejects thread and context values llama-server cannot
// start on, before anything is downloaded or any supervisor file is written.
func validateServerFlags(opts *modelOptions) error {
	if opts.threads < 1 {
		return fmt.Errorf("%w: --%s must be at least 1, got %d", errFlagValue, flagThreads, opts.threads)
	}

	if opts.ctxSize < 1 {
		return fmt.Errorf("%w: --%s must be at least 1, got %d", errFlagValue, flagCtxSize, opts.ctxSize)
	}

	return nil
}

// printInstallPlan renders what setup or upgrade would do without doing it.
func (a *App) printInstallPlan(prefix string, target *modelTarget, opts *modelOptions) {
	a.printOutf("plan: %s\n", prefix)
	a.printOutf("runtime: %s %s (%s) %s -> %s\n",
		localmodel.RuntimeName, localmodel.PinnedVersion, localmodel.PinnedBuild,
		target.build.Asset, target.layout.RuntimeDir())
	a.printOutf("model:   %s %s (%d bytes) -> %s\n",
		target.model.Name, target.model.File, target.model.Bytes, target.layout.ModelPath(&target.model))
	a.printOutf("config:  %s\n", configBlockPlan(target.home, opts.port))

	if opts.noService {
		a.printOutf("service: skipped (--no-service)\n")

		return
	}

	plan, err := sidecarPlan(target, opts)
	if err != nil {
		a.printOutf("service: %v\n", err)

		return
	}

	a.printOutf("%s", service.Describe(plan))
}

// configBlockPlan describes what the config step would do, so a dry run says
// whether an existing block would be left alone.
func configBlockPlan(home string, port int) string {
	path := filepath.Join(home, configFileName)

	present, err := hasLocalRunnerBlock(path)
	if err != nil {
		return fmt.Sprintf("cannot read %s: %v", path, err)
	}

	if present {
		return localRunnerHeader + " already present in " + path
	}

	return "append " + localRunnerHeader + " (base_url " + localmodel.BaseURL(port) + ") to " + path
}

// applyInstall performs the install, writes the config block, and puts the
// server under supervision.
func (a *App) applyInstall(
	ctx context.Context, prefix string, target *modelTarget, opts *modelOptions, upgrade bool,
) int {
	result, err := localmodel.Install(ctx, &localmodel.InstallOptions{
		Layout: target.layout,
		Model:  target.model,
		Fetch:  localmodel.FetchOptions{Force: opts.force, Offline: opts.offline, Progress: a.stderr},
	})
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	a.printInstallResult(target, &result)

	if err := a.writeConfigBlock(target.home, target.model.Name, opts.port); err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	if opts.noService {
		a.printOutf("service: skipped (--no-service); start it with: %s\n", manualStartHint(target, opts))

		return a.settleRuntime(target, &result, nil)
	}

	return a.installSidecar(ctx, prefix, target, opts, &result, upgrade)
}

// printInstallResult reports what the pass changed, so a re-run that
// downloaded nothing reads differently from a fresh install.
func (a *App) printInstallResult(target *modelTarget, result *localmodel.InstallResult) {
	a.printOutf("runtime: %s %s (%s) %s -> %s\n",
		localmodel.RuntimeName, localmodel.PinnedVersion, localmodel.PinnedBuild,
		changeWord(result.RuntimeInstalled), result.ServerPath)
	a.printOutf("model:   %s %s -> %s\n", target.model.Name, changeWord(result.ModelInstalled), result.ModelPath)
}

// changeWord says whether a component was fetched or was already in place.
func changeWord(installed bool) string {
	if installed {
		return "installed"
	}

	return "already installed"
}

// installSidecar registers and starts the model server, then, for an upgrade,
// waits for it to answer before letting go of the runtime it replaced.
func (a *App) installSidecar(
	ctx context.Context, prefix string, target *modelTarget, opts *modelOptions,
	result *localmodel.InstallResult, upgrade bool,
) int {
	plan, err := sidecarPlan(target, opts)
	if err != nil {
		a.printCmdErr(prefix, err)
		a.printErrf("%s: the runtime and model are installed; re-run with --%s to skip this step\n",
			prefix, flagNoService)
		a.printErrf("%s: start the server yourself with: %s\n", prefix, manualStartHint(target, opts))

		return ExitFailure
	}

	notices, err := service.Apply(ctx, plan, service.ExecCommand)
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	a.printOutf("service: %s\n", plan.Label)

	for _, notice := range notices {
		a.printOutf("%s\n", notice)
	}

	if !upgrade {
		a.printOutf("check it: cairnos model status\n")

		return a.settleRuntime(target, result, nil)
	}

	return a.settleRuntime(target, result, a.verifyUpgrade(ctx, opts.port))
}

// sidecarPlan builds the supervisor plan for the model server on this host.
func sidecarPlan(target *modelTarget, opts *modelOptions) (*service.Plan, error) {
	host, err := service.DetectHost()
	if err != nil {
		return nil, fmt.Errorf("detect host: %w", err)
	}

	plan, err := service.PlanSidecarSetup(host, service.SidecarOptions{
		Home:        target.home,
		Server:      target.layout.ServerPath(),
		Model:       target.layout.ModelPath(&target.model),
		Port:        opts.port,
		Threads:     opts.threads,
		ContextSize: opts.ctxSize,
	})
	if err != nil {
		return nil, fmt.Errorf("plan the local model service: %w", err)
	}

	return &plan, nil
}

// verifyUpgrade waits for the freshly restarted server to answer its models
// endpoint. A nil result means the new runtime earned the right to replace the
// old one.
func (a *App) verifyUpgrade(ctx context.Context, port int) error {
	baseURL := localmodel.BaseURL(port)
	a.printOutf("waiting for %s to answer\n", baseURL)

	if err := localmodel.WaitHealthy(ctx, nil, baseURL, upgradeProbeBudget); err != nil {
		return fmt.Errorf("verify the upgraded server: %w", err)
	}

	return nil
}

// settleRuntime decides the fate of a displaced runtime: discard it when the
// pass succeeded, put it back when the new server never answered. The rollback
// restores the install record along with the binary, so status and the next
// pass both see the build that is actually on disk. A rollback failure is
// reported alongside the original problem rather than replacing it, because
// the first error is what the operator has to act on.
func (a *App) settleRuntime(target *modelTarget, result *localmodel.InstallResult, cause error) int {
	if cause == nil {
		if err := target.layout.CommitRuntime(); err != nil {
			a.printErrf("model: %v\n", err)
		}

		return ExitOK
	}

	a.printErrf("model upgrade: %v\n", cause)

	if err := localmodel.Rollback(target.layout, result); err != nil {
		a.printErrf("model upgrade: restoring the previous runtime failed: %v\n", err)

		return ExitFailure
	}

	a.printErrf("model upgrade: restored the previous runtime; restart it with: cairnos model setup\n")

	return ExitFailure
}

// manualStartHint is the command that starts the server by hand. Both callers
// need it: --no-service asked for no supervisor, and a host this package
// cannot supervise, Windows today, has no other way to run what it just
// downloaded.
func manualStartHint(target *modelTarget, opts *modelOptions) string {
	return fmt.Sprintf("%s -m %s --host 127.0.0.1 --port %d --threads %d --ctx-size %d --jinja",
		target.layout.ServerPath(), target.layout.ModelPath(&target.model),
		opts.port, opts.threads, opts.ctxSize)
}

// writeConfigBlock appends the [runner.local] block unless the config already
// carries one. It appends rather than rewrites because config.toml belongs to
// the operator: an edited base_url, a different model, or an added key all
// survive a re-run untouched.
func (a *App) writeConfigBlock(home, modelName string, port int) error {
	path := filepath.Join(home, configFileName)

	present, err := hasLocalRunnerBlock(path)
	if err != nil {
		return err
	}

	if present {
		a.printOutf("config:  %s already present in %s\n", localRunnerHeader, path)

		return nil
	}

	if err := os.MkdirAll(home, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", home, err)
	}

	block := fmt.Sprintf(localRunnerBlock, modelName, localmodel.BaseURL(port))

	if err := appendToFile(path, block); err != nil {
		return err
	}

	a.printOutf("config:  added %s to %s\n", localRunnerHeader, path)

	return nil
}

// hasLocalRunnerBlock reports whether the config already declares the local
// runner. A missing file is not an error: there is simply no block yet.
func hasLocalRunnerBlock(path string) (bool, error) {
	// #nosec G304 -- path is the CairnOS home's config file
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return false, fmt.Errorf("read %s: %w", path, err)
	}

	for line := range strings.Lines(string(raw)) {
		if strings.TrimSpace(line) == localRunnerHeader {
			return true, nil
		}
	}

	return false, nil
}

// appendToFile adds content to the end of path, creating it when absent.
func appendToFile(path, content string) error {
	// #nosec G304 -- path is the CairnOS home's config file
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}

	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}

	return nil
}

// cmdModelCheck compares the compiled-in pins against upstream. It exits
// non-zero only when a lookup fails: "an update exists" is information, not a
// failure, and the CI job that reads this decides what to do with it.
func (a *App) cmdModelCheck(ctx context.Context, args []string) int {
	const prefix = "model check"

	flags := flag.NewFlagSet(prefix, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	jsonOut := flags.Bool(flagJSON, false, "print the report as JSON")

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	report, err := localmodel.Check(ctx, localmodel.NewUpstream(localmodel.UpstreamOptions{}))
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	if !*jsonOut {
		for _, line := range report.Lines() {
			a.printOutf("%s\n", line)
		}

		return ExitOK
	}

	encoded, err := report.JSON()
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	a.printOutf("%s", encoded)

	return ExitOK
}

// modelStatus is what `model status` reports: what is installed, whether it
// matches the pins, and whether the server is answering.
type modelStatus struct {
	Runtime       statusComponent `json:"runtime"`
	Model         statusComponent `json:"model"`
	Service       string          `json:"service"`
	ServiceLabel  string          `json:"service_label"`
	Endpoint      string          `json:"endpoint"`
	ServerAnswers bool            `json:"server_answers"`
}

// statusComponent compares one installed component against its pin.
type statusComponent struct {
	Installed string `json:"installed"`
	Pinned    string `json:"pinned"`
	Matches   bool   `json:"matches"`
}

// cmdModelStatus reports installed versus pinned and whether the server is up.
func (a *App) cmdModelStatus(ctx context.Context, args []string) int {
	const prefix = "model status"

	flags := flag.NewFlagSet(prefix, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	var opts modelOptions

	flags.StringVar(&opts.dir, flagDir, "", flagDirUsage)
	flags.StringVar(&opts.model, flagModel, localmodel.DefaultModel, "catalog model to report on")
	flags.IntVar(&opts.port, flagPort, localmodel.DefaultPort, "loopback port the model server listens on")
	flags.BoolVar(&opts.jsonOut, flagJSON, false, "print the report as JSON")

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	target, err := resolveModelTarget(&opts)
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	status, err := collectStatus(ctx, target, &opts)
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	return a.printStatus(prefix, status, opts.jsonOut)
}

// collectStatus reads the manifest, asks the supervisor for the service state,
// and probes the endpoint.
func collectStatus(ctx context.Context, target *modelTarget, opts *modelOptions) (*modelStatus, error) {
	manifest, _, err := localmodel.LoadManifest(target.layout.ManifestPath())
	if err != nil {
		return nil, fmt.Errorf("read the install record: %w", err)
	}

	endpoint := localmodel.BaseURL(opts.port)
	status := &modelStatus{
		Runtime: statusComponent{
			Installed: installedRuntimeText(&manifest),
			Pinned:    localmodel.PinnedVersion + " (" + localmodel.PinnedBuild + ")",
			Matches:   manifest.RuntimeMatches(&target.build),
		},
		Model: statusComponent{
			Installed: installedModelText(&manifest),
			Pinned:    target.model.Name + " " + localmodel.ShortDigest(target.model.SHA256),
			Matches:   manifest.ModelMatches(&target.model),
		},
		Endpoint:      endpoint,
		ServerAnswers: localmodel.Probe(ctx, nil, endpoint) == nil,
	}

	status.ServiceLabel, status.Service = serviceState(ctx)

	return status, nil
}

// serviceState asks the host supervisor what it thinks of the sidecar.
func serviceState(ctx context.Context) (string, string) {
	host, err := service.DetectHost()
	if err != nil {
		return "", "unknown"
	}

	install, err := service.SidecarInstall(host)
	if err != nil {
		return "", "unsupported"
	}

	if _, statErr := os.Stat(install.UnitPath); statErr != nil {
		return install.Label, stateNotInstalled
	}

	output, runErr := service.ExecCommand(ctx, install.StatusArgv)

	return install.Label, service.SummarizeStatus(install, output, runErr)
}

// installedRuntimeText names the runtime the manifest records, or says nothing
// is installed.
func installedRuntimeText(manifest *localmodel.Manifest) string {
	if manifest.Runtime.Version == "" {
		return stateNotInstalled
	}

	return manifest.Runtime.Version + " (" + manifest.Runtime.Build + ")"
}

// installedModelText names the model the manifest records.
func installedModelText(manifest *localmodel.Manifest) string {
	if manifest.Model.Name == "" {
		return stateNotInstalled
	}

	return manifest.Model.Name + " " + localmodel.ShortDigest(manifest.Model.SHA256)
}

// printStatus renders the status report and picks the exit code: healthy only
// when both components match their pins and the server answers.
func (a *App) printStatus(prefix string, status *modelStatus, asJSON bool) int {
	if asJSON {
		raw, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			a.printCmdErr(prefix, err)

			return ExitFailure
		}

		a.printOutf("%s\n", raw)
	} else {
		a.printOutf("runtime: %s, pinned %s %s\n",
			status.Runtime.Installed, status.Runtime.Pinned, matchWord(status.Runtime.Matches))
		a.printOutf("model:   %s, pinned %s %s\n",
			status.Model.Installed, status.Model.Pinned, matchWord(status.Model.Matches))
		a.printOutf("service: %s %s\n", status.ServiceLabel, status.Service)
		a.printOutf("server:  %s %s\n", status.Endpoint, answerWord(status.ServerAnswers))
	}

	if !status.Runtime.Matches || !status.Model.Matches || !status.ServerAnswers {
		return ExitFailure
	}

	return ExitOK
}

// matchWord is the trailing verdict on an installed-versus-pinned line.
func matchWord(matches bool) string {
	if matches {
		return "(match)"
	}

	return "(run: cairnos model setup)"
}

// answerWord says whether the endpoint responded.
func answerWord(answers bool) string {
	if answers {
		return "(answering)"
	}

	return "(not answering)"
}

// cmdModelUninstall stops and deregisters the sidecar, keeping the downloads
// unless --purge asks for the disk space back.
func (a *App) cmdModelUninstall(ctx context.Context, args []string) int {
	const prefix = "model uninstall"

	flags := flag.NewFlagSet(prefix, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	var opts modelOptions

	flags.StringVar(&opts.dir, flagDir, "", flagDirUsage)
	flags.BoolVar(&opts.dryRun, flagDryRun, false, "print the plan without touching anything")
	flags.BoolVar(&opts.purge, flagPurge, false, "also delete the downloaded runtime and model")

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	home, err := resolveDir(opts.dir)
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	plan, err := uninstallPlan()
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	if opts.dryRun {
		a.printOutf("%s", service.Describe(plan))
		a.printOutf("%s\n", purgePlan(home, opts.purge))

		return ExitOK
	}

	return a.applyUninstall(ctx, prefix, plan, home, opts.purge)
}

// uninstallPlan builds the sidecar removal plan for this host.
func uninstallPlan() (*service.Plan, error) {
	host, err := service.DetectHost()
	if err != nil {
		return nil, fmt.Errorf("detect host: %w", err)
	}

	plan, err := service.PlanSidecarUninstall(host)
	if err != nil {
		return nil, fmt.Errorf("plan the local model service removal: %w", err)
	}

	return &plan, nil
}

// purgePlan describes what --purge would delete, or what it leaves behind.
func purgePlan(home string, purge bool) string {
	root := localmodel.NewLayout(home).Root()
	if purge {
		return "rm -r: " + root
	}

	return "keep: " + root + " (pass --" + flagPurge + " to delete it)"
}

// applyUninstall removes the service and, with --purge, the downloads.
func (a *App) applyUninstall(ctx context.Context, prefix string, plan *service.Plan, home string, purge bool) int {
	notices, err := service.Apply(ctx, plan, service.ExecCommand)
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	a.printOutf("uninstalled: %s\n", plan.Label)

	for _, notice := range notices {
		a.printOutf("%s\n", notice)
	}

	layout := localmodel.NewLayout(home)

	if !purge {
		a.printOutf("kept: %s\n", layout.Root())

		return ExitOK
	}

	if err := layout.Purge(); err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	a.printOutf("deleted: %s\n", layout.Root())

	return ExitOK
}
