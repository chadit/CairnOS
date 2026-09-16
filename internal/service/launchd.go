package service

import (
	"fmt"
	"path/filepath"
	"strings"
)

// launchdAgentPlist is the LaunchAgent shape. launchd has no equivalent of
// systemd's EnvironmentFile and secrets must stay out of the plist, so the
// job runs a /bin/sh preamble that sources the env file and execs serve.
// KeepAlive on SuccessfulExit=false restarts a crash but honors a clean stop.
const launchdAgentPlist = `<?xml version="1.0" encoding="UTF-8"?>
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

// planLaunchdSetup builds the user-mode LaunchAgent plan. Idempotence comes
// from booting the old job out before bootstrapping the rewritten plist; the
// bootout is optional because a first install has nothing to boot out.
func planLaunchdSetup(host *Host, opts Options) Plan {
	plistPath := launchdAgentPath(host)
	envFile := envFilePath(opts.Home, false)
	logPath := filepath.Join(opts.Home, "logs", "serve.log")

	secretsFile := SecretsEnvFile(opts.Home, false)

	// launchd has no EnvironmentFile, so the preamble sources both files in the
	// same order systemd would read them: the hand-written one first, then the
	// file `cairnos secrets pull` generates, so a pulled value wins a collision.
	// Each test guards its own source, which is what the systemd dash does.
	shellLine := fmt.Sprintf(
		"set -a; [ -f %s ] && . %s; [ -f %s ] && . %s; set +a; exec %s serve --dir %s",
		shQuote(envFile), shQuote(envFile),
		shQuote(secretsFile), shQuote(secretsFile),
		shQuote(opts.Binary), shQuote(opts.Home),
	)

	plist := fmt.Sprintf(
		launchdAgentPlist,
		launchdLabel, xmlEscape(shellLine), xmlEscape(logPath), xmlEscape(logPath),
	)

	return Plan{
		Label: labelLaunchdUser,
		Dirs: []Dir{
			{Path: opts.Home, Perm: dirPerm},
			{Path: filepath.Join(opts.Home, "logs"), Perm: dirPerm},
		},
		Files: []File{
			{Path: plistPath, Content: plist, Perm: filePerm},
			{Path: envFile, Content: envFileTemplate, Perm: filePerm, KeepExisting: true},
		},
		Commands: []Command{
			{Argv: []string{launchctlBin, cmdBootout, launchdTarget(host)}, Optional: true},
			{Argv: []string{launchctlBin, cmdBootstrap, "gui/" + host.UID, plistPath}},
		},
		Notices: []string{
			"config and data: " + opts.Home,
			"secrets env file: " + envFile,
			"logs: " + logPath,
		},
	}
}

// planLaunchdUninstall boots the job out and removes the plist, keeping data
// and the env file.
func planLaunchdUninstall(host *Host) Plan {
	return Plan{
		Label: labelLaunchdUser,
		Commands: []Command{
			{Argv: []string{launchctlBin, cmdBootout, launchdTarget(host)}, Optional: true},
		},
		Removals: []string{launchdAgentPath(host)},
		Notices:  []string{"config, data, and the env file were left in place"},
	}
}

// launchdAgentPath is the per-user LaunchAgent location.
func launchdAgentPath(host *Host) string {
	return filepath.Join(host.HomeDir, "Library", "LaunchAgents", launchdLabel+".plist")
}

// launchdTarget names the job inside the user's GUI domain, the form
// launchctl bootout and print both take.
func launchdTarget(host *Host) string {
	return "gui/" + host.UID + "/" + launchdLabel
}

// shQuote wraps one path for the /bin/sh -c line: single quotes, with any
// embedded single quote closed, escaped, and reopened.
func shQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// xmlEscape covers the characters XML text cannot carry raw.
func xmlEscape(text string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

	return replacer.Replace(text)
}
