package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/service"
)

const serviceUsage = `usage: cairnos service <setup|status|uninstall> [flags]

  setup      Install cairnos serve under the host supervisor and start it.
             The default is a per-user service; --system installs a
             machine-wide service (needs root, for agents that must control
             the machine itself). --dir picks the CairnOS home; --dry-run
             prints the plan and touches nothing.
  status     Report each discovered install and ask its supervisor for the
             service state.
  uninstall  Stop the service and remove the supervisor files. Config, data,
             and the env file stay; --system targets the system-wide install.

Supported today: systemd (Linux, user or system) and launchd (macOS, user).
`

// cmdService dispatches the host-service subcommands (D14 in
// .agents/spec/14-open-decisions.md; design in 11-deployment-and-config.md,
// "Host install").
func (a *App) cmdService(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", serviceUsage)

		return ExitUsage
	}

	switch args[0] {
	case "setup":
		return a.cmdServiceSetup(ctx, args[1:])
	case subcmdStatus:
		return a.cmdServiceStatus(ctx, args[1:])
	case "uninstall":
		return a.cmdServiceUninstall(ctx, args[1:])
	default:
		a.printErrf("unknown service subcommand %q\n", args[0])
		a.printErrf("%s", serviceUsage)

		return ExitUsage
	}
}

// cmdServiceSetup plans the install for this host and either prints the plan
// (--dry-run) or applies it and starts the service.
func (a *App) cmdServiceSetup(ctx context.Context, args []string) int {
	const prefix = "service setup"

	flags := flag.NewFlagSet(prefix, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	systemFlag := flags.Bool("system", false, "install a machine-wide service (needs root)")
	dryRunFlag := flags.Bool("dry-run", false, "print the plan without touching anything")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	host, err := service.DetectHost()
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	opts, err := serviceOptions(*systemFlag, *dirFlag, a.envLookup())
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	plan, err := service.PlanSetup(host, opts)
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	if *dryRunFlag {
		a.printOutf("%s", service.Describe(&plan))

		return ExitOK
	}

	notices, err := service.Apply(ctx, &plan, service.ExecCommand)
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	a.printOutf("installed: %s\n", plan.Label)

	for _, notice := range notices {
		a.printOutf("%s\n", notice)
	}

	a.printOutf("check it: cairnos service status\n")

	return ExitOK
}

// cmdServiceStatus reports every discovered install and exits healthy only
// when at least one is running.
func (a *App) cmdServiceStatus(ctx context.Context, args []string) int {
	const prefix = "service status"

	flags := flag.NewFlagSet(prefix, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	host, err := service.DetectHost()
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	installs := service.Discover(host, func(path string) bool {
		_, statErr := os.Stat(path)

		return statErr == nil
	})

	if len(installs) == 0 {
		a.printOutf("no service installed; run: cairnos service setup\n")

		return ExitFailure
	}

	var healthy bool

	for _, install := range installs {
		output, runErr := service.ExecCommand(ctx, install.StatusArgv)
		state := service.SummarizeStatus(install, output, runErr)
		healthy = healthy || service.StateHealthy(state)

		a.printOutf("%s: %s\n", install.Label, state)
	}

	if !healthy {
		return ExitFailure
	}

	return ExitOK
}

// cmdServiceUninstall removes one install mode, leaving config, data, and
// the env file behind.
func (a *App) cmdServiceUninstall(ctx context.Context, args []string) int {
	const prefix = "service uninstall"

	flags := flag.NewFlagSet(prefix, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	systemFlag := flags.Bool("system", false, "remove the machine-wide install (needs root)")
	dryRunFlag := flags.Bool("dry-run", false, "print the plan without touching anything")

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	host, err := service.DetectHost()
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	plan, err := service.PlanUninstall(host, *systemFlag)
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	if *dryRunFlag {
		a.printOutf("%s", service.Describe(&plan))

		return ExitOK
	}

	notices, err := service.Apply(ctx, &plan, service.ExecCommand)
	if err != nil {
		a.printCmdErr(prefix, err)

		return ExitFailure
	}

	a.printOutf("uninstalled: %s\n", plan.Label)

	for _, notice := range notices {
		a.printOutf("%s\n", notice)
	}

	return ExitOK
}

// serviceOptions resolves the home and binary the unit will reference. The
// system-mode home defaults to the doc-11 layout instead of the invoking
// root's own dotdir, which would tie a machine service to a human account.
func serviceOptions(system bool, dirFlag string, lookup environ.Lookup) (service.Options, error) {
	home := dirFlag

	if home == "" && system {
		home = service.SystemHome
	}

	if !system {
		resolved, err := resolveDir(dirFlag, lookup)
		if err != nil {
			return service.Options{}, err
		}

		home = resolved
	}

	binary, err := executablePath()
	if err != nil {
		return service.Options{}, err
	}

	return service.Options{System: system, Home: home, Binary: binary}, nil
}

// executablePath is the absolute, symlink-resolved path of this binary, so
// the unit keeps working when the invoking PATH entry was a symlink.
func executablePath() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return "", fmt.Errorf("resolve executable symlink: %w", err)
	}

	return resolved, nil
}

// printCmdErr prefixes one subcommand's failure line.
func (a *App) printCmdErr(command string, err error) {
	a.printErrf("%s: %v\n", command, err)
}
