package service_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/service"
)

const (
	testBinary    = "/usr/local/bin/cairnos"
	testLabel     = "test"
	cmdReload     = "reload"
	cmdSupervisor = "supervisor"

	// The fixture account and the paths each platform derives from it.
	testUser       = "operator"
	linuxDataHome  = "/home/operator/.cairnos"
	darwinDataHome = "/Users/operator/.cairnos"
	userUnitPath   = "/home/operator/.config/systemd/user/cairnos.service"
	systemUnitPath = "/etc/systemd/system/cairnos.service"

	// Host shapes the unsupported-platform cases share across files.
	caseNoSystemd = "linux without systemd"
	goosWindows   = "windows"
)

// linuxHost is a plain systemd workstation user, the default install target.
func linuxHost() *service.Host {
	return &service.Host{
		GOOS:       "linux",
		EUID:       1000,
		Username:   testUser,
		UID:        "1000",
		HomeDir:    "/home/operator",
		ConfigDir:  "/home/operator/.config",
		HasSystemd: true,
		HasUser:    func(string) bool { return false },
	}
}

// darwinHost is a macOS user for the LaunchAgent path.
func darwinHost() *service.Host {
	return &service.Host{
		GOOS:      "darwin",
		EUID:      501,
		Username:  testUser,
		UID:       "501",
		HomeDir:   "/Users/operator",
		ConfigDir: "/Users/operator/Library/Application Support",
	}
}

func userOptions() service.Options {
	return service.Options{
		Home:   linuxDataHome,
		Binary: testBinary,
	}
}

// planFile returns the plan file at path, failing the test when absent.
func planFile(t *testing.T, plan *service.Plan, path string) service.File {
	t.Helper()

	for _, file := range plan.Files {
		if file.Path == path {
			return file
		}
	}

	t.Fatalf("plan has no file %s; files: %+v", path, plan.Files)

	return service.File{}
}

// hasCommand reports whether the plan runs a command whose argv joins to want.
func hasCommand(commands []service.Command, want string) bool {
	for _, command := range commands {
		if strings.Join(command.Argv, " ") == want {
			return true
		}
	}

	return false
}

func TestPlanSetupSystemdUserWritesUnitEnvAndLinger(t *testing.T) {
	t.Parallel()

	plan, err := service.PlanSetup(linuxHost(), userOptions())
	if err != nil {
		t.Fatalf("PlanSetup: %v", err)
	}

	if plan.Label != "systemd (user)" {
		t.Errorf("label = %q", plan.Label)
	}

	unit := planFile(t, &plan, userUnitPath)
	for _, want := range []string{
		`ExecStart="` + testBinary + `" serve --dir "/home/operator/.cairnos"`,
		"EnvironmentFile=-/home/operator/.cairnos/env",
		"Restart=always",
	} {
		if !strings.Contains(unit.Content, want) {
			t.Errorf("unit missing %q:\n%s", want, unit.Content)
		}
	}

	env := planFile(t, &plan, "/home/operator/.cairnos/env")
	if !env.KeepExisting {
		t.Error("env file must keep operator edits across re-runs")
	}

	if env.Perm != 0o600 {
		t.Errorf("env perm = %#o, want 0600", env.Perm)
	}

	for _, want := range []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable cairnos.service",
		"systemctl --user restart cairnos.service",
	} {
		if !hasCommand(plan.Commands, want) {
			t.Errorf("plan missing command %q", want)
		}
	}

	linger := plan.Commands[len(plan.Commands)-1]
	if !linger.Optional || !strings.Contains(linger.Hint, "enable-linger operator") {
		t.Errorf("linger step must be optional with a sudo hint, got %+v", linger)
	}
}

func TestPlanSetupSystemdSystemCreatesAccountOnlyWhenMissing(t *testing.T) {
	t.Parallel()

	host := linuxHost()
	host.EUID = 0

	opts := service.Options{System: true, Home: service.SystemHome, Binary: testBinary}

	plan, err := service.PlanSetup(host, opts)
	if err != nil {
		t.Fatalf("PlanSetup: %v", err)
	}

	if !hasCommand(plan.Commands, "useradd --system --user-group --home-dir /var/lib/cairnos --shell /usr/sbin/nologin cairnos") {
		t.Error("missing account should add the useradd step")
	}

	if !hasCommand(plan.Commands, "chown -R cairnos:cairnos /var/lib/cairnos") {
		t.Error("data dir must be handed to the service account")
	}

	unit := planFile(t, &plan, systemUnitPath)
	if !strings.Contains(unit.Content, "User=cairnos") {
		t.Errorf("system unit must run as the service account:\n%s", unit.Content)
	}

	planFile(t, &plan, "/etc/cairnos/env")

	host.HasUser = func(string) bool { return true }

	plan, err = service.PlanSetup(host, opts)
	if err != nil {
		t.Fatalf("PlanSetup with existing account: %v", err)
	}

	for _, command := range plan.Commands {
		if command.Argv[0] == "useradd" {
			t.Error("existing account must not be re-created")
		}
	}
}

// TestServeUnitsRestartAlwaysSoADrainedExitComesBack pins the directive a
// drain-then-restart update depends on: `cairnos update` asks the service to
// finish its runs and exit 0, and on-failure would leave the host stopped.
func TestServeUnitsRestartAlwaysSoADrainedExitComesBack(t *testing.T) {
	t.Parallel()

	root := linuxHost()
	root.EUID = 0

	cases := []struct {
		host     *service.Host
		name     string
		unitPath string
		opts     service.Options
	}{
		{
			name: "user", host: linuxHost(), opts: userOptions(),
			unitPath: userUnitPath,
		},
		{
			name: "system", host: root,
			opts:     service.Options{System: true, Home: service.SystemHome, Binary: testBinary},
			unitPath: systemUnitPath,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			plan, err := service.PlanSetup(testCase.host, testCase.opts)
			if err != nil {
				t.Fatalf("PlanSetup: %v", err)
			}

			unit := planFile(t, &plan, testCase.unitPath)
			if !strings.Contains(unit.Content, "Restart=always") {
				t.Errorf("serve unit must restart on a clean exit:\n%s", unit.Content)
			}

			if !strings.Contains(unit.Content, "RestartSec=2") {
				t.Errorf("serve unit must keep its restart delay:\n%s", unit.Content)
			}
		})
	}
}

func TestPlanSetupLaunchdBuildsAgentWithEnvSourcingWrapper(t *testing.T) {
	t.Parallel()

	opts := service.Options{Home: darwinDataHome, Binary: testBinary}

	plan, err := service.PlanSetup(darwinHost(), opts)
	if err != nil {
		t.Fatalf("PlanSetup: %v", err)
	}

	plist := planFile(t, &plan, "/Users/operator/Library/LaunchAgents/dev.cairnos.serve.plist")
	for _, want := range []string{
		"<string>dev.cairnos.serve</string>",
		"exec '/usr/local/bin/cairnos' serve --dir '/Users/operator/.cairnos'",
		". '/Users/operator/.cairnos/env'",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(plist.Content, want) {
			t.Errorf("plist missing %q:\n%s", want, plist.Content)
		}
	}

	if !hasCommand(plan.Commands, "launchctl bootstrap gui/501 /Users/operator/Library/LaunchAgents/dev.cairnos.serve.plist") {
		t.Error("plan must bootstrap the agent")
	}

	bootout := plan.Commands[0]
	if strings.Join(bootout.Argv, " ") != "launchctl bootout gui/501/dev.cairnos.serve" || !bootout.Optional {
		t.Errorf("first command must be the tolerated bootout, got %+v", bootout)
	}
}

func TestPlanSetupQuotesAwkwardPathsInLaunchdShellLine(t *testing.T) {
	t.Parallel()

	host := darwinHost()
	host.HomeDir = "/Users/o'brien"

	opts := service.Options{Home: "/Users/o'brien/My Cairn", Binary: testBinary}

	plan, err := service.PlanSetup(host, opts)
	if err != nil {
		t.Fatalf("PlanSetup: %v", err)
	}

	plist := planFile(t, &plan, "/Users/o'brien/Library/LaunchAgents/dev.cairnos.serve.plist")
	if !strings.Contains(plist.Content, `'/Users/o'\''brien/My Cairn'`) {
		t.Errorf("home with a quote and a space must be shell-quoted:\n%s", plist.Content)
	}
}

func TestPlanSetupModeGuards(t *testing.T) {
	t.Parallel()

	rootLinux := linuxHost()
	rootLinux.EUID = 0

	noSystemd := linuxHost()
	noSystemd.HasSystemd = false

	cases := []struct {
		want error
		host *service.Host
		name string
		opts service.Options
	}{
		{name: "system mode without root", host: linuxHost(), opts: service.Options{System: true}, want: service.ErrNeedRoot},
		{name: "user mode as root", host: rootLinux, opts: service.Options{}, want: service.ErrRootUserMode},
		{name: "system mode on macOS", host: darwinHost(), opts: service.Options{System: true}, want: service.ErrDarwinSystem},
		{name: caseNoSystemd, host: noSystemd, opts: service.Options{}, want: service.ErrUnsupported},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if _, err := service.PlanSetup(testCase.host, testCase.opts); !errors.Is(err, testCase.want) {
				t.Errorf("PlanSetup error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestPlanUninstallReleasesUnitBeforeRemovalAndReloadsAfter(t *testing.T) {
	t.Parallel()

	plan, err := service.PlanUninstall(linuxHost(), false)
	if err != nil {
		t.Fatalf("PlanUninstall: %v", err)
	}

	disable := plan.Commands[0]
	if strings.Join(disable.Argv, " ") != "systemctl --user disable --now cairnos.service" || !disable.Optional {
		t.Errorf("uninstall must start with a tolerated disable, got %+v", disable)
	}

	if len(plan.Removals) != 1 || plan.Removals[0] != userUnitPath {
		t.Errorf("removals = %v", plan.Removals)
	}

	if !hasCommand(plan.After, "systemctl --user daemon-reload") {
		t.Error("daemon-reload must run after the unit file is gone")
	}

	if len(plan.Files) != 0 {
		t.Error("uninstall must not write files")
	}
}

func TestApplyWritesFilesKeepsExistingEnvAndRunsCommands(t *testing.T) {
	t.Parallel()

	const (
		unitContent = "unit"
		cmdLinger   = "linger"
	)

	home := t.TempDir()
	envFile := filepath.Join(home, "env")

	if err := os.WriteFile(envFile, []byte("OPERATOR_EDIT=1\n"), 0o600); err != nil {
		t.Fatalf("seed env: %v", err)
	}

	plan := service.Plan{
		Label: testLabel,
		Dirs:  []service.Dir{{Path: filepath.Join(home, "logs"), Perm: 0o750}},
		Files: []service.File{
			{Path: filepath.Join(home, "sub", "unit.txt"), Content: unitContent, Perm: 0o600},
			{Path: envFile, Content: "TEMPLATE=1\n", Perm: 0o600, KeepExisting: true},
		},
		Commands: []service.Command{
			{Argv: []string{cmdSupervisor, cmdReload}},
			{Argv: []string{cmdLinger}, Optional: true, Hint: "run linger yourself"},
		},
		Notices: []string{"done"},
	}

	var ran []string

	execer := func(_ context.Context, argv []string) (string, error) {
		joined := strings.Join(argv, " ")
		ran = append(ran, joined)

		if joined == cmdLinger {
			return "denied", errors.New("polkit said no")
		}

		return "", nil
	}

	notices, err := service.Apply(t.Context(), &plan, execer)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	unitBytes, err := os.ReadFile(filepath.Join(home, "sub", "unit.txt"))
	if err != nil || string(unitBytes) != unitContent {
		t.Errorf("unit file content = %q, err %v", unitBytes, err)
	}

	envBytes, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}

	if string(envBytes) != "OPERATOR_EDIT=1\n" {
		t.Errorf("existing env was rewritten to %q", envBytes)
	}

	if len(ran) != 2 {
		t.Errorf("commands ran = %v", ran)
	}

	joined := strings.Join(notices, "\n")
	if !strings.Contains(joined, "run linger yourself") || !strings.Contains(joined, "done") {
		t.Errorf("notices = %v", notices)
	}
}

func TestApplyStopsOnRequiredCommandFailureWithOutput(t *testing.T) {
	t.Parallel()

	plan := service.Plan{
		Label:    testLabel,
		Commands: []service.Command{{Argv: []string{cmdSupervisor, cmdReload}}},
	}

	execer := func(_ context.Context, _ []string) (string, error) {
		return "unit is masked", errors.New("exit 1")
	}

	_, err := service.Apply(t.Context(), &plan, execer)
	if !errors.Is(err, service.ErrCommandFailed) {
		t.Fatalf("Apply error = %v, want ErrCommandFailed", err)
	}

	// The message assertion is deliberate: the supervisor's own output is the
	// operator's only clue, so losing it is a regression errors.Is cannot see.
	if rendered := fmt.Sprint(err); !strings.Contains(rendered, "unit is masked") {
		t.Errorf("error must carry the supervisor output: %v", err)
	}
}

func TestApplyRemovesFilesAndToleratesMissingOnes(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	present := filepath.Join(home, "unit.txt")

	if err := os.WriteFile(present, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var order []string

	plan := service.Plan{
		Label:    testLabel,
		Removals: []string{present, filepath.Join(home, "never-existed.txt")},
		After:    []service.Command{{Argv: []string{cmdReload}}},
	}

	execer := func(_ context.Context, argv []string) (string, error) {
		if _, err := os.Stat(present); err == nil {
			t.Error("After command ran before the removal")
		}

		order = append(order, strings.Join(argv, " "))

		return "", nil
	}

	if _, err := service.Apply(t.Context(), &plan, execer); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(order) != 1 || order[0] != cmdReload {
		t.Errorf("after commands = %v", order)
	}
}

func TestDescribeListsFilesCommandsAndRemovals(t *testing.T) {
	t.Parallel()

	plan, err := service.PlanSetup(linuxHost(), userOptions())
	if err != nil {
		t.Fatalf("PlanSetup: %v", err)
	}

	text := service.Describe(&plan)
	for _, want := range []string{
		"plan: systemd (user)",
		"file: /home/operator/.config/systemd/user/cairnos.service (0o600)",
		"file: /home/operator/.cairnos/env (0o600, kept if present)",
		"run:  systemctl --user restart cairnos.service",
		"run:  loginctl enable-linger operator (failure tolerated)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("describe missing %q:\n%s", want, text)
		}
	}
}

func TestDiscoverReportsOnlyInstallsWhoseUnitFilesExist(t *testing.T) {
	t.Parallel()

	installs := service.Discover(linuxHost(), func(path string) bool {
		return path == systemUnitPath
	})

	if len(installs) != 1 || installs[0].Label != "systemd (system)" {
		t.Fatalf("installs = %+v", installs)
	}

	if strings.Join(installs[0].StatusArgv, " ") != "systemctl is-active cairnos.service" {
		t.Errorf("status argv = %v", installs[0].StatusArgv)
	}

	if got := service.Discover(linuxHost(), func(string) bool { return false }); len(got) != 0 {
		t.Errorf("nothing installed should discover nothing, got %+v", got)
	}
}

func TestSummarizeStatusReadsSystemdAndLaunchdShapes(t *testing.T) {
	t.Parallel()

	systemd := service.Install{StatusArgv: []string{"systemctl", "is-active", "cairnos.service"}}
	launchd := service.Install{StatusArgv: []string{"launchctl", "print", "gui/501/dev.cairnos.serve"}}

	cases := []struct {
		err     error
		name    string
		output  string
		want    string
		install service.Install
		healthy bool
	}{
		{name: "systemd active", install: systemd, output: "active\n", err: nil, want: "active", healthy: true},
		{name: "systemd inactive exits non-zero", install: systemd, output: "inactive\n", err: errors.New("exit 3"), want: "inactive", healthy: false},
		{name: "launchd running", install: launchd, output: "\tstate = running\n", err: nil, want: "running", healthy: true},
		{name: "launchd loaded without state line", install: launchd, output: "no state here", err: nil, want: "loaded", healthy: false},
		{name: "launchd not loaded", install: launchd, output: "", err: errors.New("exit 113"), want: "not loaded", healthy: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			state := service.SummarizeStatus(testCase.install, testCase.output, testCase.err)
			if state != testCase.want {
				t.Errorf("state = %q, want %q", state, testCase.want)
			}

			if service.StateHealthy(state) != testCase.healthy {
				t.Errorf("StateHealthy(%q) = %v, want %v", state, !testCase.healthy, testCase.healthy)
			}
		})
	}
}
