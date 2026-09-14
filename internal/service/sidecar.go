package service

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Names the sidecar install uses on each supervisor. They sit beside the
// serve unit rather than replacing it: a node runs the API and the local
// model server as two supervised processes, so either can be restarted
// without taking the other down.
const (
	sidecarUnitName     = "cairnos-llama.service"
	sidecarLaunchdLabel = "dev.cairnos.llama"

	labelSidecarSystemd = "systemd (user, local model)"
	labelSidecarLaunchd = "launchd (user, local model)"
)

// sidecarSystemdUnit is the per-user unit for the model server. There is no
// EnvironmentFile: llama-server reads no secrets, and the one thing it needs
// from the host is the model path, which is already on the command line.
const sidecarSystemdUnit = `[Unit]
Description=CairnOS local model server
After=network.target

[Service]
ExecStart=%s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`

// sidecarPlist is the LaunchAgent for the model server. KeepAlive on
// SuccessfulExit=false restarts a crash while honoring a clean stop, the same
// bargain the serve agent makes.
const sidecarPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`

// sidecarLogName is the file the supervisor sends the server's output to,
// inside the CairnOS home's logs directory beside the serve log.
const sidecarLogName = "llama-server.log"

// SidecarOptions describe the model server one install supervises. Every path
// is absolute because a supervisor starts the process with no useful working
// directory.
type SidecarOptions struct {
	// Home is the CairnOS home, which is where the log lands.
	Home string
	// Server is the llama-server executable.
	Server string
	// Model is the GGUF the server loads.
	Model string
	// Port is the loopback port it listens on.
	Port int
	// Threads is how many CPU threads it may use.
	Threads int
	// ContextSize is the context window in tokens.
	ContextSize int
}

// PlanSidecarSetup builds the install plan for the local model server on this
// host. It is per-user only: the model server exists to answer this
// operator's agents, and a machine-wide one would need a service account that
// can read weights installed under a home directory.
func PlanSidecarSetup(host *Host, opts SidecarOptions) (Plan, error) {
	if err := guardSidecar(host); err != nil {
		return Plan{}, err
	}

	switch {
	case host.GOOS == goosDarwin:
		return planSidecarLaunchd(host, opts), nil
	case host.GOOS == goosLinux && host.HasSystemd:
		return planSidecarSystemd(host, opts), nil
	default:
		return Plan{}, fmt.Errorf("%w (GOOS %s)", ErrSidecarUnsupported, host.GOOS)
	}
}

// PlanSidecarUninstall stops the model server and removes its supervisor
// files, leaving the downloaded runtime and weights alone.
func PlanSidecarUninstall(host *Host) (Plan, error) {
	if err := guardSidecar(host); err != nil {
		return Plan{}, err
	}

	switch {
	case host.GOOS == goosDarwin:
		return Plan{
			Label:    labelSidecarLaunchd,
			Commands: []Command{{Argv: []string{launchctlBin, cmdBootout, sidecarLaunchdTarget(host)}, Optional: true}},
			Removals: []string{sidecarPlistPath(host)},
			Notices:  []string{noticeModelKept},
		}, nil
	case host.GOOS == goosLinux && host.HasSystemd:
		return Plan{
			Label: labelSidecarSystemd,
			Commands: []Command{
				{Argv: []string{systemctlBin, userScope, cmdDisable, "--now", sidecarUnitName}, Optional: true},
			},
			Removals: []string{sidecarUnitPath(host)},
			After:    []Command{{Argv: []string{systemctlBin, userScope, cmdDaemonReload}}},
			Notices:  []string{noticeModelKept},
		}, nil
	default:
		return Plan{}, fmt.Errorf("%w (GOOS %s)", ErrSidecarUnsupported, host.GOOS)
	}
}

// noticeModelKept is the promise sidecar uninstall makes: removing the
// service never deletes the multi-gigabyte download it was serving.
const noticeModelKept = "the downloaded runtime and model were left in place"

// SidecarInstall names where the model server's supervisor file lives on this
// host and how to ask for its state.
func SidecarInstall(host *Host) (Install, error) {
	switch {
	case host.GOOS == goosDarwin:
		return Install{
			Label:      labelSidecarLaunchd,
			UnitPath:   sidecarPlistPath(host),
			StatusArgv: []string{launchctlBin, cmdPrint, sidecarLaunchdTarget(host)},
		}, nil
	case host.GOOS == goosLinux && host.HasSystemd:
		return Install{
			Label:      labelSidecarSystemd,
			UnitPath:   sidecarUnitPath(host),
			StatusArgv: []string{systemctlBin, userScope, cmdIsActive, sidecarUnitName},
		}, nil
	default:
		return Install{}, fmt.Errorf("%w (GOOS %s)", ErrSidecarUnsupported, host.GOOS)
	}
}

// planSidecarSystemd writes the user unit and starts it.
func planSidecarSystemd(host *Host, opts SidecarOptions) Plan {
	unitPath := sidecarUnitPath(host)

	return Plan{
		Label: labelSidecarSystemd,
		Dirs:  []Dir{{Path: opts.Home, Perm: dirPerm}},
		Files: []File{
			{
				Path:    unitPath,
				Content: fmt.Sprintf(sidecarSystemdUnit, sidecarExecLine(opts)),
				Perm:    filePerm,
			},
		},
		Commands: []Command{
			{Argv: []string{systemctlBin, userScope, cmdDaemonReload}},
			{Argv: []string{systemctlBin, userScope, cmdEnable, sidecarUnitName}},
			{Argv: []string{systemctlBin, userScope, cmdRestart, sidecarUnitName}},
		},
		Notices: []string{sidecarEndpointNotice(opts.Port)},
	}
}

// planSidecarLaunchd writes the LaunchAgent and bootstraps it, booting out any
// previous job first so a re-run replaces rather than collides.
func planSidecarLaunchd(host *Host, opts SidecarOptions) Plan {
	plistPath := sidecarPlistPath(host)
	logPath := sidecarLogPath(opts.Home)
	shellLine := "exec " + sidecarShellCommand(opts)

	return Plan{
		Label: labelSidecarLaunchd,
		Dirs: []Dir{
			{Path: opts.Home, Perm: dirPerm},
			{Path: filepath.Join(opts.Home, "logs"), Perm: dirPerm},
		},
		Files: []File{
			{
				Path: plistPath,
				Content: fmt.Sprintf(sidecarPlist, sidecarLaunchdLabel,
					xmlEscape(shellLine), xmlEscape(logPath), xmlEscape(logPath)),
				Perm: filePerm,
			},
		},
		Commands: []Command{
			{Argv: []string{launchctlBin, cmdBootout, sidecarLaunchdTarget(host)}, Optional: true},
			{Argv: []string{launchctlBin, cmdBootstrap, "gui/" + host.UID, plistPath}},
		},
		Notices: []string{sidecarEndpointNotice(opts.Port), "logs: " + logPath},
	}
}

// guardSidecar refuses a root shell. A user-mode job installed by root lands
// in root's own manager rather than the operator's, so the install would
// report success while nothing the operator can see is running.
func guardSidecar(host *Host) error {
	if host.EUID == 0 {
		return ErrRootUserMode
	}

	return nil
}

// sidecarArgs is the llama-server command line, as separate arguments so each
// renderer quotes them the way its supervisor expects. --jinja turns on the
// model's own chat template, which is what carries Gemma's tool-call syntax;
// without it the server falls back to a generic template and tool calls come
// back as prose.
func sidecarArgs(opts SidecarOptions) []string {
	return []string{
		"-m", opts.Model,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(opts.Port),
		"--threads", strconv.Itoa(opts.Threads),
		"--ctx-size", strconv.Itoa(opts.ContextSize),
		"--jinja",
	}
}

// sidecarExecLine renders the ExecStart value for systemd, whose parser splits
// on whitespace unless an argument is double-quoted.
func sidecarExecLine(opts SidecarOptions) string {
	args := sidecarArgs(opts)
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, systemdQuote(opts.Server))

	for _, arg := range args {
		if strings.ContainsAny(arg, ` "\\`) {
			arg = systemdQuote(arg)
		}

		parts = append(parts, arg)
	}

	return strings.Join(parts, " ")
}

// systemdQuote wraps one ExecStart argument in the double quotes systemd needs
// to keep a path with spaces in one piece. Inside those quotes systemd treats a
// backslash as an escape, so both it and the quote character are escaped first;
// otherwise a path with either would come back out altered or split the line.
func systemdQuote(arg string) string {
	escaped := strings.ReplaceAll(arg, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)

	return `"` + escaped + `"`
}

// sidecarShellCommand renders the command for the LaunchAgent, which starts
// the server through /bin/sh rather than exec'ing it directly.
func sidecarShellCommand(opts SidecarOptions) string {
	args := sidecarArgs(opts)
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shQuote(opts.Server))

	for _, arg := range args {
		parts = append(parts, shQuote(arg))
	}

	return strings.Join(parts, " ")
}

// sidecarEndpointNotice tells the operator where the server answers, which is
// the value they need for any other tool pointed at the same model.
func sidecarEndpointNotice(port int) string {
	return "local model endpoint: http://127.0.0.1:" + strconv.Itoa(port) + "/v1"
}

// sidecarUnitPath places the sidecar unit beside the serve unit under the XDG
// config home.
func sidecarUnitPath(host *Host) string {
	return filepath.Join(host.ConfigDir, "systemd", "user", sidecarUnitName)
}

// sidecarPlistPath is the per-user LaunchAgent location.
func sidecarPlistPath(host *Host) string {
	return filepath.Join(host.HomeDir, "Library", "LaunchAgents", sidecarLaunchdLabel+".plist")
}

// sidecarLaunchdTarget names the job inside the user's GUI domain.
func sidecarLaunchdTarget(host *Host) string {
	return "gui/" + host.UID + "/" + sidecarLaunchdLabel
}

// sidecarLogPath is where launchd sends the server's merged output.
func sidecarLogPath(home string) string {
	return filepath.Join(home, "logs", sidecarLogName)
}
