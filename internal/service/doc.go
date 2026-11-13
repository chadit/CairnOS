// Package service installs and manages cairnos serve under the host's process
// supervisor. Planners are pure: they turn Options and a Host snapshot
// into a Plan of files and commands, so tests and --dry-run inspect exactly
// what setup would do before anything touches the system. This round ships
// systemd (Linux, user and system units) and the launchd LaunchAgent (macOS,
// user mode); runit, Windows, and the macOS LaunchDaemon are still owed.
package service
