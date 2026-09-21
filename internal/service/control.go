package service

// cmdShow is the systemctl verb that reads one property off a unit.
const cmdShow = "show"

// Systemd reports whether systemctl drives this install. `cairnos update` asks
// because systemd is the supervisor that answers with a main process id, which
// is what lets the update read the build the service is actually running.
func (i Install) Systemd() bool {
	return len(i.StatusArgv) > 0 && i.StatusArgv[0] == systemctlBin
}

// RestartArgv restarts this install's unit in the scope it was discovered in.
func (i Install) RestartArgv() []string {
	return i.systemctlArgv(cmdRestart)
}

// MainPIDArgv prints the unit's main process id, or "0" when it is not
// running. The update reads /proc/<pid>/exe from that answer, so a binary
// already replaced on disk is never mistaken for the one in memory.
func (i Install) MainPIDArgv() []string {
	return i.systemctlArgv(cmdShow, "-p", "MainPID", "--value")
}

// systemctlArgv builds one invocation with this install's scope and unit, so
// neither --user nor the unit name is written at a call site.
func (i Install) systemctlArgv(verb ...string) []string {
	argv := []string{systemctlBin}

	if !i.System {
		argv = append(argv, userScope)
	}

	argv = append(argv, verb...)

	return append(argv, unitName)
}
