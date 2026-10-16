package service_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/service"
)

// sidecarOptions is the model server one plan supervises, with the paths a
// setup run would have produced.
func sidecarOptions() service.SidecarOptions {
	return service.SidecarOptions{
		Home:        linuxDataHome,
		Server:      "/home/operator/.cairnos/model/runtime/llama-server",
		Model:       "/home/operator/.cairnos/model/models/gemma.gguf",
		Port:        8089,
		Threads:     6,
		ContextSize: 32768,
	}
}

// TestSidecarSystemdUnitStartsTheServerWithItsModel proves the unit the plan
// writes starts the installed binary against the installed weights, on
// loopback, with the chat template that carries tool calls.
func TestSidecarSystemdUnitStartsTheServerWithItsModel(t *testing.T) {
	t.Parallel()

	plan, err := service.PlanSidecarSetup(linuxHost(), sidecarOptions())
	if err != nil {
		t.Fatalf("PlanSidecarSetup: %v", err)
	}

	unit := planFile(t, &plan, "/home/operator/.config/systemd/user/cairnos-llama.service")

	for _, want := range []string{
		`"/home/operator/.cairnos/model/runtime/llama-server"`,
		"-m /home/operator/.cairnos/model/models/gemma.gguf",
		"--host 127.0.0.1",
		"--port 8089",
		"--threads 6",
		"--ctx-size 32768",
		"--jinja",
	} {
		if !strings.Contains(unit.Content, want) {
			t.Errorf("unit is missing %q:\n%s", want, unit.Content)
		}
	}
}

// TestSidecarUnitIsSeparateFromTheServeUnit proves the model server gets its
// own unit rather than replacing the API one, so restarting either leaves the
// other running.
func TestSidecarUnitIsSeparateFromTheServeUnit(t *testing.T) {
	t.Parallel()

	plan, err := service.PlanSidecarSetup(linuxHost(), sidecarOptions())
	if err != nil {
		t.Fatalf("PlanSidecarSetup: %v", err)
	}

	for _, file := range plan.Files {
		if strings.HasSuffix(file.Path, "/cairnos.service") {
			t.Errorf("the sidecar plan rewrites the serve unit at %s", file.Path)
		}
	}

	var enabled bool

	for _, command := range plan.Commands {
		if strings.Join(command.Argv, " ") == "systemctl --user enable cairnos-llama.service" {
			enabled = true
		}
	}

	if !enabled {
		t.Error("the plan never enables the sidecar unit")
	}
}

// TestSidecarLaunchdAgentQuotesPathsForTheShell proves the LaunchAgent command
// line survives a home directory with a space in it, which macOS hands out
// routinely.
func TestSidecarLaunchdAgentQuotesPathsForTheShell(t *testing.T) {
	t.Parallel()

	opts := sidecarOptions()
	opts.Server = "/Users/first last/.cairnos/model/runtime/llama-server"

	plan, err := service.PlanSidecarSetup(darwinHost(), opts)
	if err != nil {
		t.Fatalf("PlanSidecarSetup: %v", err)
	}

	plist := planFile(t, &plan, "/Users/operator/Library/LaunchAgents/dev.cairnos.llama.plist")

	if !strings.Contains(plist.Content, `'/Users/first last/.cairnos/model/runtime/llama-server'`) {
		t.Errorf("plist does not quote the server path:\n%s", plist.Content)
	}

	if !strings.Contains(plist.Content, "<string>dev.cairnos.llama</string>") {
		t.Errorf("plist carries the wrong label:\n%s", plist.Content)
	}
}

// unsupportedHosts are the shapes with no per-user supervisor this package
// drives: Windows, and a Linux box with no systemd.
func unsupportedHosts() map[string]*service.Host {
	noSystemd := linuxHost()
	noSystemd.HasSystemd = false

	return map[string]*service.Host{
		goosWindows:   {GOOS: goosWindows, EUID: -1},
		caseNoSystemd: noSystemd,
	}
}

// TestSidecarRefusesUnsupportedHosts proves an unsupported platform says so
// instead of installing something that never starts.
func TestSidecarRefusesUnsupportedHosts(t *testing.T) {
	t.Parallel()

	for name, host := range unsupportedHosts() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := service.PlanSidecarSetup(host, sidecarOptions()); !errors.Is(err, service.ErrSidecarUnsupported) {
				t.Errorf("setup err = %v, want ErrSidecarUnsupported", err)
			}

			if _, err := service.PlanSidecarUninstall(host); !errors.Is(err, service.ErrSidecarUnsupported) {
				t.Errorf("uninstall err = %v, want ErrSidecarUnsupported", err)
			}

			if _, err := service.SidecarInstall(host); !errors.Is(err, service.ErrSidecarUnsupported) {
				t.Errorf("install err = %v, want ErrSidecarUnsupported", err)
			}
		})
	}
}

// TestSidecarRefusesRootUserMode proves a root shell is turned away, since a
// user-mode job installed by root lands in root's manager, not the operator's.
func TestSidecarRefusesRootUserMode(t *testing.T) {
	t.Parallel()

	host := linuxHost()
	host.EUID = 0

	_, err := service.PlanSidecarSetup(host, sidecarOptions())
	if !errors.Is(err, service.ErrRootUserMode) {
		t.Fatalf("err = %v, want ErrRootUserMode", err)
	}
}

// TestSidecarUninstallKeepsTheDownload proves removing the service never
// deletes the multi-gigabyte weights it was serving.
func TestSidecarUninstallKeepsTheDownload(t *testing.T) {
	t.Parallel()

	plan, err := service.PlanSidecarUninstall(linuxHost())
	if err != nil {
		t.Fatalf("PlanSidecarUninstall: %v", err)
	}

	for _, path := range plan.Removals {
		if strings.Contains(path, "/model/") {
			t.Errorf("uninstall removes %s, which holds the download", path)
		}
	}

	if len(plan.Removals) != 1 || !strings.HasSuffix(plan.Removals[0], "cairnos-llama.service") {
		t.Errorf("removals = %v, want just the sidecar unit", plan.Removals)
	}
}

// TestSidecarInstallNamesTheStatusProbe proves status has a file to look for
// and a command to ask, per supervisor.
func TestSidecarInstallNamesTheStatusProbe(t *testing.T) {
	t.Parallel()

	install, err := service.SidecarInstall(darwinHost())
	if err != nil {
		t.Fatalf("SidecarInstall: %v", err)
	}

	if install.UnitPath != "/Users/operator/Library/LaunchAgents/dev.cairnos.llama.plist" {
		t.Errorf("unit path = %q", install.UnitPath)
	}

	if strings.Join(install.StatusArgv, " ") != "launchctl print gui/501/dev.cairnos.llama" {
		t.Errorf("status argv = %v", install.StatusArgv)
	}
}

// TestSidecarSystemdUnitEscapesQuotedPaths proves a path carrying a backslash
// or a double quote survives the unit file intact. systemd treats a backslash
// inside double quotes as an escape, so an unescaped one would change the path
// the server loads, and an unescaped quote would end the argument early and
// split the line.
func TestSidecarSystemdUnitEscapesQuotedPaths(t *testing.T) {
	t.Parallel()

	opts := sidecarOptions()
	opts.Server = `/home/operator/od"d path/llama-server`
	opts.Model = `/home/operator/back\slash/gemma.gguf`

	plan, err := service.PlanSidecarSetup(linuxHost(), opts)
	if err != nil {
		t.Fatalf("PlanSidecarSetup: %v", err)
	}

	unit := planFile(t, &plan, "/home/operator/.config/systemd/user/cairnos-llama.service")

	if !strings.Contains(unit.Content, `"/home/operator/od\"d path/llama-server"`) {
		t.Errorf("the quote in the server path was not escaped:\n%s", unit.Content)
	}

	if !strings.Contains(unit.Content, `"/home/operator/back\\slash/gemma.gguf"`) {
		t.Errorf("the backslash in the model path was not escaped:\n%s", unit.Content)
	}
}
