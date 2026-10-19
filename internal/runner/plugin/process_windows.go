//go:build windows

package plugin

import (
	"errors"
	"fmt"
	"log/slog"
	"math"

	"golang.org/x/sys/windows"
)

// jobKillExitCode is what TerminateJobObject reports for every process it
// kills. Nothing reads it; the host already treats the exit as a crash.
const jobKillExitCode = 1

// processTree holds the job object the plugin process runs inside. Windows has
// no process groups to signal, so a job is how the host reaches the whole tree.
type processTree struct {
	job windows.Handle
}

func (*Plugin) configureProcessTree() {
	// A job object can only take a process that is already running, so all of
	// the setup happens in adoptProcessTree after Start.
}

// adoptProcessTree puts the started child in a new job object, so stopProcess
// reaches every process the plugin spawned. The job has no kill-on-close limit,
// since setting one needs unsafe. The stdin pipe from New covers a host that
// dies without Close: the plugin exits on its own, while its children keep
// running, as on Unix.
func (p *Plugin) adoptProcessTree() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create plugin job object: %w", err)
	}

	if err := p.assignProcess(job); err != nil {
		// With no job recorded, stopProcess falls back to killing the child.
		if closeErr := windows.CloseHandle(job); closeErr != nil {
			slog.Warn("close unused plugin job object", "pid", p.cmd.Process.Pid, "error", closeErr)
		}

		return err
	}

	// Recorded only once the child is in it: terminating a job the child never
	// joined stops nothing, and stopProcess would then wait forever.
	p.tree.job = job

	return nil
}

// assignProcess hands the running child to job. os/exec keeps the process
// handle private, so the host reopens it by pid; Start has already returned,
// so the process is alive to open.
func (p *Plugin) assignProcess(job windows.Handle) error {
	const rights = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE

	pid := p.cmd.Process.Pid
	if pid < 0 || int64(pid) > math.MaxUint32 {
		return fmt.Errorf("%w: %d", errProcessID, pid)
	}

	handle, err := windows.OpenProcess(rights, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open plugin process: %w", err)
	}

	assignErr := windows.AssignProcessToJobObject(job, handle)

	// The job assignment is what the start needs. A handle that will not close
	// leaks one handle and is no reason to fail the plugin, so it only warns.
	if closeErr := windows.CloseHandle(handle); closeErr != nil {
		slog.Warn("close plugin process handle", "pid", p.cmd.Process.Pid, "error", closeErr)
	}

	if assignErr != nil {
		return fmt.Errorf("assign plugin to job object: %w", assignErr)
	}

	return nil
}

// stopProcess kills the job object, which takes every process in the tree with
// it, waits for the child to be reaped, then releases the job handle. Windows
// has no SIGTERM, so the graceful drain Unix gets from closeGrace has no
// equivalent step here.
func (p *Plugin) stopProcess() error {
	endErr := p.endTree()

	return errors.Join(endErr, p.closeJob())
}

// endTree kills the tree unless the child is already gone, then waits for the
// reap.
func (p *Plugin) endTree() error {
	select {
	case <-p.exited:
		return nil
	default:
	}

	if err := p.killTree(); err != nil {
		return err
	}

	<-p.exited

	return nil
}

// closeJob releases the job handle once the tree is down. A zero handle means
// adoptProcessTree failed before the job existed, so there is nothing to close.
func (p *Plugin) closeJob() error {
	if p.tree.job == 0 {
		return nil
	}

	err := windows.CloseHandle(p.tree.job)
	p.tree.job = 0

	if err != nil {
		return fmt.Errorf("close plugin job object: %w", err)
	}

	return nil
}

// killTree kills the job object, falling back to the child alone when the job
// is missing or refuses. A zero handle means adoptProcessTree failed before the
// job existed, so nothing else would end the child and stopProcess would hang.
func (p *Plugin) killTree() error {
	if p.tree.job != 0 {
		if err := windows.TerminateJobObject(p.tree.job, jobKillExitCode); err == nil {
			return nil
		}
	}

	if err := p.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("terminate plugin process tree: %w", err)
	}

	return nil
}
