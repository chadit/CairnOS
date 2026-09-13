package service

import "fmt"

// systemdUserUnit is the per-user unit. ExecStart arguments are quoted so a
// home or binary path with spaces survives systemd's word splitting; the
// EnvironmentFile dash makes a deleted env file a non-event instead of a
// start failure.
const systemdUserUnit = `[Unit]
Description=CairnOS runtime
After=network-online.target
Wants=network-online.target

[Service]
ExecStart="%s" serve --dir "%s"
EnvironmentFile=-%s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`

// systemdSystemUnit adds the dedicated service account; everything else
// matches the user unit so both modes behave the same way.
const systemdSystemUnit = `[Unit]
Description=CairnOS runtime
After=network-online.target
Wants=network-online.target

[Service]
User=%s
Group=%s
ExecStart="%s" serve --dir "%s"
EnvironmentFile=-%s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`

// systemUser is the account a system-mode service runs as, so root is only
// the installer, never the runtime.
const systemUser = "cairnos"

// planSystemdSetup builds the install plan for either systemd mode.
func planSystemdSetup(host *Host, opts Options) Plan {
	if opts.System {
		return planSystemdSystemSetup(host, opts)
	}

	unitPath := userUnitPath(host)
	envFile := envPath(opts.Home)

	return Plan{
		Label: labelSystemdUser,
		Dirs:  []Dir{{Path: opts.Home, Perm: dirPerm}},
		Files: []File{
			{Path: unitPath, Content: fmt.Sprintf(systemdUserUnit, opts.Binary, opts.Home, envFile), Perm: filePerm},
			{Path: envFile, Content: envFileTemplate, Perm: filePerm, KeepExisting: true},
		},
		Commands: []Command{
			{Argv: []string{systemctlBin, userScope, cmdDaemonReload}},
			{Argv: []string{systemctlBin, userScope, cmdEnable, unitName}},
			{Argv: []string{systemctlBin, userScope, cmdRestart, unitName}},
			{
				Argv:     []string{"loginctl", "enable-linger", host.Username},
				Optional: true,
				Hint: "linger keeps the service running after logout; run: sudo loginctl enable-linger " +
					host.Username,
			},
		},
		Notices: []string{
			"config and data: " + opts.Home,
			"secrets env file: " + envFile,
		},
	}
}

// planSystemdSystemSetup is the root path: a dedicated account, the unit
// under /etc, and the env file under /etc/cairnos. The account creation step
// only appears when the account is missing (or the host cannot say), so a
// re-run stays clean.
func planSystemdSystemSetup(host *Host, opts Options) Plan {
	commands := make([]Command, 0, 6)

	if host.HasUser == nil || !host.HasUser(systemUser) {
		commands = append(commands, Command{Argv: []string{
			"useradd", "--system", "--user-group",
			"--home-dir", opts.Home, "--shell", "/usr/sbin/nologin", systemUser,
		}})
	}

	commands = append(
		commands,
		// The data dir must belong to the service account, not the installing
		// root, or the first run fails on the store file.
		Command{Argv: []string{"chown", "-R", systemUser + ":" + systemUser, opts.Home}},
		Command{Argv: []string{systemctlBin, cmdDaemonReload}},
		Command{Argv: []string{systemctlBin, cmdEnable, unitName}},
		Command{Argv: []string{systemctlBin, cmdRestart, unitName}},
	)

	unit := fmt.Sprintf(systemdSystemUnit, systemUser, systemUser, opts.Binary, opts.Home, systemEnvPath)

	return Plan{
		Label: labelSystemdSystem,
		Dirs: []Dir{
			{Path: opts.Home, Perm: dirPerm},
			{Path: "/etc/cairnos", Perm: dirPerm},
		},
		Files: []File{
			{Path: systemUnitPath, Content: unit, Perm: filePerm},
			{Path: systemEnvPath, Content: envFileTemplate, Perm: filePerm, KeepExisting: true},
		},
		Commands: commands,
		Notices: []string{
			"config and data: " + opts.Home + " (owned by " + systemUser + ")",
			"secrets env file: " + systemEnvPath,
		},
	}
}

// planSystemdUninstall releases the unit, removes its file, and reloads,
// leaving data and the env file where they are.
func planSystemdUninstall(host *Host, system bool) Plan {
	scope := func(argv ...string) []string {
		if system {
			return append([]string{systemctlBin}, argv...)
		}

		return append([]string{systemctlBin, userScope}, argv...)
	}

	label, unitPath := labelSystemdUser, userUnitPath(host)
	if system {
		label, unitPath = labelSystemdSystem, systemUnitPath
	}

	return Plan{
		Label: label,
		Commands: []Command{
			// Optional: uninstalling an already-stopped or half-removed
			// service should still clear the unit file.
			{Argv: scope(cmdDisable, "--now", unitName), Optional: true},
		},
		Removals: []string{unitPath},
		After: []Command{
			{Argv: scope(cmdDaemonReload)},
		},
		Notices: []string{"config, data, and the env file were left in place"},
	}
}
