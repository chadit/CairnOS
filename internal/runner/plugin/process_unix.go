//go:build !windows

package plugin

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// closeGrace is how long stopProcess waits after SIGTERM before escalating to
// SIGKILL on the process group. Unix only; Windows has no graceful step.
const closeGrace = 3 * time.Second

// processTree holds the plugin's process group id, which Setpgid makes equal
// to the child's own pid. One signal to the group reaches the whole tree.
type processTree struct {
	group int
}

// configureProcessTree gives the plugin its own process group before Start, so
// stopProcess can take down the whole tree, including children the plugin
// spawned, with one signal.
func (p *Plugin) configureProcessTree() {
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// adoptProcessTree records the group Setpgid created at Start. The child leads
// its own group, so the group id is its pid.
func (p *Plugin) adoptProcessTree() error {
	p.tree.group = p.cmd.Process.Pid

	return nil
}

// stopProcess signals the negative group id, which reaches the whole group,
// and waits for the child to be reaped. SIGTERM first for a graceful drain,
// SIGKILL after the grace period; SIGKILL cannot be caught, so the final wait
// always returns. An unrecorded group is refused: group 0 is the host's own,
// and signaling it would kill the engine.
func (p *Plugin) stopProcess() error {
	select {
	case <-p.exited:
		return nil
	default:
	}

	if p.tree.group <= 0 {
		return errNoProcessGroup
	}

	if err := syscall.Kill(-p.tree.group, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			<-p.exited

			return nil
		}

		return fmt.Errorf("terminate plugin process group: %w", err)
	}

	timer := time.NewTimer(closeGrace)
	defer timer.Stop()

	select {
	case <-p.exited:
		return nil
	case <-timer.C:
		_ = syscall.Kill(-p.tree.group, syscall.SIGKILL)

		<-p.exited

		return nil
	}
}
