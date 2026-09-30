package builtins_test

import (
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// configuredUsage is the runner every validation case shares: two launchers and
// one reachable host, so a refusal can only come from the task.
func configuredUsage(t *testing.T) *builtins.Usage {
	t.Helper()

	return newUsage(t, map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{codexLauncher},
	}, builtins.UsageOptions{
		Hosts: map[string]builtins.UsageHost{
			buildBox: {SSH: buildBoxAlias},
			quietBox: {},
		},
	})
}

// assertValidateCode checks the code of a Validate refusal.
func assertValidateCode(t *testing.T, err error, code string) {
	t.Helper()

	if err == nil {
		t.Fatalf("validate accepted the task, want %s", code)
	}

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Code != code {
		t.Fatalf("code = %q, want %q", classified.Code, code)
	}
}

func TestUsageValidateAcceptsAnAbsentHostAndLaunchers(t *testing.T) {
	t.Parallel()

	if err := configuredUsage(t).Validate(usageTask(nil)); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestUsageValidateRefusesAnUnknownHost(t *testing.T) {
	t.Parallel()

	err := configuredUsage(t).Validate(usageTask(map[string]any{usageHostKey: "other_box"}))
	assertValidateCode(t, err, "usage.unknown_host")
}

func TestUsageValidateRefusesAHostWithoutAnAlias(t *testing.T) {
	t.Parallel()

	err := configuredUsage(t).Validate(usageTask(map[string]any{usageHostKey: quietBox}))
	assertValidateCode(t, err, "usage.host_no_ssh")
}

func TestUsageValidateRefusesANonStringHost(t *testing.T) {
	t.Parallel()

	err := configuredUsage(t).Validate(usageTask(map[string]any{usageHostKey: int64(7)}))
	assertValidateCode(t, err, "usage.bad_host")
}

func TestUsageValidateDefersAnUnresolvedHost(t *testing.T) {
	t.Parallel()

	task := usageTask(map[string]any{usageHostKey: "${{ inputs.box }}"})
	task.Resolved = false

	if err := configuredUsage(t).Validate(task); err != nil {
		t.Fatalf("validate refused an unresolved reference: %v", err)
	}
}

func TestUsageValidateRefusesAnUnknownLauncher(t *testing.T) {
	t.Parallel()

	task := usageTask(map[string]any{usageLaunchers: []any{claudeLauncher, "typo-agent"}})
	assertValidateCode(t, configuredUsage(t).Validate(task), "usage.unknown_launcher")
}

func TestUsageValidateRefusesALaunchersValueThatIsNotAList(t *testing.T) {
	t.Parallel()

	task := usageTask(map[string]any{usageLaunchers: claudeLauncher})
	assertValidateCode(t, configuredUsage(t).Validate(task), "usage.bad_launchers")
}

func TestUsageValidateRefusesANonStringLauncherEntry(t *testing.T) {
	t.Parallel()

	task := usageTask(map[string]any{usageLaunchers: []any{int64(3)}})
	assertValidateCode(t, configuredUsage(t).Validate(task), "usage.bad_launchers")
}

func TestUsageConfigRefusesALauncherCarryingAShellCharacter(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, launcher string }{
		{"a space", "claude agent"},
		{"a semicolon", "claude;rm"},
		{"a dollar sign", "claude$HOME"},
		{"a backtick", "claude`id`"},
		{"a leading dash", "-claude"},
		{"a relative path", "bin/claude"},
		{"a trailing slash", "/usr/bin/"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := builtins.NewUsage(map[string]any{
				claudeConfigKey: []any{testCase.launcher},
			}, builtins.UsageOptions{})

			if !errors.Is(err, builtins.ErrConfigValue) {
				t.Fatalf("error = %v, want an invalid config value for %q", err, testCase.launcher)
			}
		})
	}
}

func TestUsageConfigAcceptsBareNamesAndResolvablePaths(t *testing.T) {
	t.Parallel()

	accepted := []string{claudeLauncher, "/usr/local/bin/claude", "~/.local/bin/" + claudeLauncher}

	for _, launcher := range accepted {
		if _, err := builtins.NewUsage(map[string]any{
			claudeConfigKey: []any{launcher},
		}, builtins.UsageOptions{}); err != nil {
			t.Fatalf("config refused %q: %v", launcher, err)
		}
	}
}

func TestUsageConfigRefusesANonPositiveTimeout(t *testing.T) {
	t.Parallel()

	for _, seconds := range []int64{0, -1} {
		_, err := builtins.NewUsage(map[string]any{usageTimeoutKey: seconds}, builtins.UsageOptions{})
		if !errors.Is(err, builtins.ErrConfigValue) {
			t.Fatalf("timeout %d gave %v, want an invalid config value", seconds, err)
		}
	}
}

func TestUsageConfigRefusesALauncherListedUnderBothProtocols(t *testing.T) {
	t.Parallel()

	_, err := builtins.NewUsage(map[string]any{
		claudeConfigKey: []any{claudeLauncher},
		codexConfigKey:  []any{claudeLauncher},
	}, builtins.UsageOptions{})

	if !errors.Is(err, builtins.ErrConfigValue) {
		t.Fatalf("error = %v, want an invalid config value", err)
	}
}

func TestUsageWithNoConfiguredLaunchersFailsPermanently(t *testing.T) {
	t.Parallel()

	usage := newUsage(t, map[string]any{
		claudeConfigKey: []any{},
		codexConfigKey:  []any{},
	}, builtins.UsageOptions{})

	_, err := usage.Execute(t.Context(), usageTask(nil))
	assertSweepFailure(t, err, runner.ClassPermanent)
}

func TestUsageDefaultsToTheTwoBareCommands(t *testing.T) {
	dir := t.TempDir()
	writeShim(t, dir, "claude", claudeScript)
	writeShim(t, dir, "codex", codexScript)

	t.Parallel()

	usage := newUsage(t, nil, builtins.UsageOptions{Lookup: pathEnv(dir)})

	result, err := usage.Execute(t.Context(), usageTask(nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// An absent config block still probes the two bare commands, which is what
	// makes the runner useful on a host that never wrote the block.
	got := report(t, result)
	if len(findLogin(t, got, "claude").Windows) == 0 || len(findLogin(t, got, "codex").Windows) == 0 {
		t.Fatalf("report %v, want both default launchers reporting windows", got)
	}
}
