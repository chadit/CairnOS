package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Shared names across the platform planners.
const (
	// unitName is the systemd unit; launchdLabel is the launchd job label.
	unitName     = "cairnos.service"
	launchdLabel = "dev.cairnos.serve"

	systemUnitPath = "/etc/systemd/system/cairnos.service"

	// systemEnvDir is where a system install keeps both environment files. A
	// system unit runs as the cairnos account, whose home holds state rather
	// than configuration, so the files live under /etc like any other unit's.
	systemEnvDir = "/etc/cairnos"

	// envFileName is the hand-written environment file, and secretsEnvFileName
	// the one `cairnos secrets pull` generates. They are named here rather than
	// at each use so the unit and the pull cannot drift apart.
	envFileName = "env"

	// #nosec G101 -- a file name, not a credential.
	secretsEnvFileName = "secrets.env"

	// SystemHome is the default data directory for a system-mode install,
	// matching the doc-11 host-install layout.
	SystemHome = "/var/lib/cairnos"

	dirPerm  = 0o750
	filePerm = 0o600

	systemctlBin = "systemctl"
	launchctlBin = "launchctl"
	userScope    = "--user"

	// Supervisor verbs shared by the serve and local-model planners, named
	// once so a typo in either planner is a compile error rather than a
	// command the supervisor silently rejects.
	cmdRestart      = "restart"
	cmdEnable       = "enable"
	cmdDisable      = "disable"
	cmdDaemonReload = "daemon-reload"
	cmdIsActive     = "is-active"
	cmdBootout      = "bootout"
	cmdBootstrap    = "bootstrap"
	cmdPrint        = "print"

	goosDarwin = "darwin"
	goosLinux  = "linux"

	labelSystemdUser   = "systemd (user)"
	labelSystemdSystem = "systemd (system)"
	labelLaunchdUser   = "launchd (user)"
)

// envFileTemplate seeds the env file the supervisor reads at start. Secrets
// belong here rather than in config.toml or the unit, so the unit files stay
// safe to read and to check into config management.
const envFileTemplate = `# CairnOS service environment (read at service start; keep mode 0600).
# Secrets belong here, never in config.toml or the service unit.
#
# CAIRNOS_RUNNER_ANTHROPIC_API_KEY=
# CAIRNOS_RUNNER_OPENAI_API_KEY=
# CAIRNOS_RUNNER_GITHUB_TOKEN=
# CAIRNOS_NOTIFY_DISCORD_WEBHOOK=
`

// Options select what setup installs.
type Options struct {
	// Home is the CairnOS home the service runs against (config + data).
	Home string

	// Binary is the absolute path of the cairnos executable the unit starts.
	Binary string

	// System installs a machine-wide service under root. The default (false)
	// is a per-user service, which covers every agent that does not need to
	// control the machine itself.
	System bool
}

// Host is a snapshot of the facts the planners branch on, gathered once by
// DetectHost so the planners stay pure and tests build hosts directly.
type Host struct {
	// HasUser answers whether a named system account exists, so the system
	// planner knows whether to include the useradd step. Nil means unknown
	// and the planner includes the step defensively.
	HasUser func(name string) bool

	GOOS      string
	Username  string
	UID       string
	HomeDir   string
	ConfigDir string
	EUID      int

	// HasSystemd reports /run/systemd/system, the documented way to detect a
	// running systemd without talking to it.
	HasSystemd bool
}

// File is one file a plan writes.
type File struct {
	Path    string
	Content string
	Perm    os.FileMode
	// KeepExisting marks operator-owned content (the env file): once it
	// exists, re-running setup never rewrites it.
	KeepExisting bool
}

// Dir is one directory a plan creates.
type Dir struct {
	Path string
	Perm os.FileMode
}

// Command is one supervisor call a plan runs. Optional commands may fail
// without failing the plan; the failure surfaces as a notice built from
// Hint, for steps like linger that can want privileges setup does not have.
type Command struct {
	Hint     string
	Argv     []string
	Optional bool
}

// Plan is everything setup or uninstall would do, in apply order: Dirs,
// Files, Commands, Removals, then After. The split around Removals exists
// for uninstall, where the supervisor must release a unit before its file
// goes and reload after it is gone.
type Plan struct {
	Label    string
	Dirs     []Dir
	Files    []File
	Commands []Command
	Removals []string
	After    []Command
	Notices  []string
}

// Install identifies one discovered install for status and uninstall.
type Install struct {
	Label      string
	UnitPath   string
	StatusArgv []string
	System     bool
}

// PlanSetup picks the platform planner for the host and returns the full
// install plan without touching anything.
func PlanSetup(host *Host, opts Options) (Plan, error) {
	if err := guardMode(host, opts.System); err != nil {
		return Plan{}, err
	}

	switch {
	case host.GOOS == goosDarwin:
		return planLaunchdSetup(host, opts), nil
	case host.GOOS == goosLinux && host.HasSystemd:
		return planSystemdSetup(host, opts), nil
	default:
		return Plan{}, fmt.Errorf("%w (GOOS %s)", ErrUnsupported, host.GOOS)
	}
}

// PlanUninstall builds the removal plan for one install mode. Data, config,
// and the env file are deliberately absent from the removals.
func PlanUninstall(host *Host, system bool) (Plan, error) {
	if err := guardMode(host, system); err != nil {
		return Plan{}, err
	}

	switch {
	case host.GOOS == goosDarwin:
		return planLaunchdUninstall(host), nil
	case host.GOOS == goosLinux && host.HasSystemd:
		return planSystemdUninstall(host, system), nil
	default:
		return Plan{}, fmt.Errorf("%w (GOOS %s)", ErrUnsupported, host.GOOS)
	}
}

// Discover returns the installs whose supervisor files exist on this host,
// probing through exists so tests need no filesystem.
func Discover(host *Host, exists func(string) bool) []Install {
	var found []Install

	for _, candidate := range candidates(host) {
		if exists(candidate.UnitPath) {
			found = append(found, candidate)
		}
	}

	return found
}

// candidates lists every install location this host could carry, whether or
// not anything is installed there.
func candidates(host *Host) []Install {
	if host.GOOS == goosDarwin {
		return []Install{{
			Label:      labelLaunchdUser,
			System:     false,
			UnitPath:   launchdAgentPath(host),
			StatusArgv: []string{launchctlBin, cmdPrint, launchdTarget(host)},
		}}
	}

	return []Install{
		{
			Label:      labelSystemdUser,
			System:     false,
			UnitPath:   userUnitPath(host),
			StatusArgv: []string{systemctlBin, userScope, cmdIsActive, unitName},
		},
		{
			Label:      labelSystemdSystem,
			System:     true,
			UnitPath:   systemUnitPath,
			StatusArgv: []string{systemctlBin, cmdIsActive, unitName},
		},
	}
}

// SummarizeStatus turns one status command's outcome into a short state
// word. systemctl is-active prints the state itself; launchctl print needs
// its "state = X" line fished out, and a non-zero exit means the job is not
// loaded at all.
func SummarizeStatus(install Install, output string, err error) string {
	if install.StatusArgv[0] == launchctlBin {
		return summarizeLaunchd(output, err)
	}

	state := strings.TrimSpace(output)
	if state == "" {
		state = "unknown"
	}

	return state
}

// StateHealthy reports whether a summarized state means the service is up,
// so the status command can pick its exit code.
func StateHealthy(state string) bool {
	return state == "active" || state == "running"
}

// summarizeLaunchd reads launchctl print output for the job state.
func summarizeLaunchd(output string, err error) string {
	if err != nil {
		return "not loaded"
	}

	for line := range strings.Lines(output) {
		trimmed := strings.TrimSpace(line)
		if state, found := strings.CutPrefix(trimmed, "state = "); found {
			return strings.TrimSpace(state)
		}
	}

	return "loaded"
}

// guardMode enforces the mode rules before any planning: system mode needs
// root, macOS system mode is not built, and a root user-mode install is a
// mistake refused outright.
func guardMode(host *Host, system bool) error {
	if system && host.GOOS == goosDarwin {
		return ErrDarwinSystem
	}

	if system && host.EUID != 0 {
		return ErrNeedRoot
	}

	if !system && host.EUID == 0 && host.GOOS == goosLinux {
		return ErrRootUserMode
	}

	return nil
}

// userUnitPath places the user unit under the XDG config home, where
// systemd --user looks for units.
func userUnitPath(host *Host) string {
	return filepath.Join(host.ConfigDir, "systemd", "user", unitName)
}

// EnvDir is the directory holding both environment files: the CairnOS home for
// a user install, /etc/cairnos for a system one. It is exported because
// `cairnos secrets pull` has to write into the same directory the unit reads
// from, and a system unit does not read from the home at all.
func EnvDir(home string, system bool) string {
	if system {
		return systemEnvDir
	}

	return home
}

// envFilePath is the hand-written environment file the unit loads first. It
// stays unexported because nothing outside this package writes it: the pull
// only ever touches the generated file beside it.
func envFilePath(home string, system bool) string {
	return filepath.Join(EnvDir(home, system), envFileName)
}

// SecretsEnvFile is the generated file the unit loads second. It is never
// scaffolded or templated: `cairnos secrets pull` writes it whole, so setup only
// has to tell the unit where to look, and the pull has to ask here rather than
// guessing from the home.
func SecretsEnvFile(home string, system bool) string {
	return filepath.Join(EnvDir(home, system), secretsEnvFileName)
}
