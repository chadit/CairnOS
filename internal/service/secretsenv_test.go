package service_test

import (
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/service"
)

// The two environment files every mode loads: the hand-written one and the file
// `cairnos secrets pull` generates.
const (
	userEnvFile     = "/home/operator/.cairnos/env"
	userSecretsFile = "/home/operator/.cairnos/secrets.env"
)

// TestSystemdUserUnitLoadsBothEnvironmentFiles covers FR10 for the mode the NAS
// runs. The generated file is a second EnvironmentFile rather than being merged
// into the first, so a pull can replace it whole without touching an operator's
// own lines.
func TestSystemdUserUnitLoadsBothEnvironmentFiles(t *testing.T) {
	t.Parallel()

	plan, err := service.PlanSetup(linuxHost(), userOptions())
	if err != nil {
		t.Fatalf("PlanSetup: %v", err)
	}

	unit := planFile(t, &plan, "/home/operator/.config/systemd/user/cairnos.service")

	for _, want := range []string{
		"EnvironmentFile=-" + userEnvFile,
		"EnvironmentFile=-" + userSecretsFile,
	} {
		if !strings.Contains(unit.Content, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit.Content)
		}
	}

	// Order decides which file wins a duplicate key, and the generated one has to
	// be able to override a stale hand-written line.
	if strings.Index(unit.Content, userEnvFile) > strings.Index(unit.Content, userSecretsFile) {
		t.Fatalf("the generated file must come second:\n%s", unit.Content)
	}
}

// TestSystemdSystemUnitLoadsBothEnvironmentFiles is the same guarantee for a
// root install, where both files live under /etc.
func TestSystemdSystemUnitLoadsBothEnvironmentFiles(t *testing.T) {
	t.Parallel()

	host := linuxHost()
	host.EUID = 0

	plan, err := service.PlanSetup(host, service.Options{
		Home: service.SystemHome, Binary: testBinary, System: true,
	})
	if err != nil {
		t.Fatalf("PlanSetup: %v", err)
	}

	unit := planFile(t, &plan, "/etc/systemd/system/cairnos.service")

	for _, want := range []string{
		"EnvironmentFile=-/etc/cairnos/env",
		"EnvironmentFile=-/etc/cairnos/secrets.env",
	} {
		if !strings.Contains(unit.Content, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit.Content)
		}
	}
}

// TestLaunchdPreambleSourcesBothEnvironmentFiles covers the macOS half of FR10.
// launchd has no EnvironmentFile, so the preamble has to guard each source the
// way systemd's dash does, or a host without a vault fails to start.
func TestLaunchdPreambleSourcesBothEnvironmentFiles(t *testing.T) {
	t.Parallel()

	plan, err := service.PlanSetup(darwinHost(), service.Options{
		Home: "/Users/operator/.cairnos", Binary: testBinary,
	})
	if err != nil {
		t.Fatalf("PlanSetup: %v", err)
	}

	plist := planFile(t, &plan, "/Users/operator/Library/LaunchAgents/dev.cairnos.serve.plist")

	for _, want := range []string{
		"[ -f '/Users/operator/.cairnos/env' ] &amp;&amp; . '/Users/operator/.cairnos/env'",
		"[ -f '/Users/operator/.cairnos/secrets.env' ] &amp;&amp; . '/Users/operator/.cairnos/secrets.env'",
	} {
		if !strings.Contains(plist.Content, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist.Content)
		}
	}
}

// TestSetupDoesNotScaffoldTheGeneratedFile keeps the two files' ownership clear:
// setup seeds the hand-written one and leaves the other to `secrets pull`, so an
// empty placeholder never shadows a value the unit expects.
func TestSetupDoesNotScaffoldTheGeneratedFile(t *testing.T) {
	t.Parallel()

	plan, err := service.PlanSetup(linuxHost(), userOptions())
	if err != nil {
		t.Fatalf("PlanSetup: %v", err)
	}

	for _, file := range plan.Files {
		if file.Path == userSecretsFile {
			t.Fatalf("setup scaffolds %s; the pull owns that file", userSecretsFile)
		}
	}
}
