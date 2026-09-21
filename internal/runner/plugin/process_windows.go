//go:build windows

package plugin

import (
	"fmt"
	"unsafe"

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

// configureProcessTree has nothing to do before Start: a job takes a running
// process, so the whole setup waits for adoptProcessTree.
func (*Plugin) configureProcessTree() {}

// adoptProcessTree creates the job object and puts the started child in it.
// KILL_ON_JOB_CLOSE means a host that dies without calling Close still takes
// the plugin's tree with it, since the handle closes with the process.
func (p *Plugin) adoptProcessTree() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create plugin job object: %w", err)
	}

	p.tree.job = job

	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}

	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return fmt.Errorf("set plugin job object limits: %w", err)
	}

	return p.assignProcess()
}

// assignProcess hands the running child to the job. os/exec keeps the process
// handle private, so the host reopens it by pid; Start has already returned,
// so the process is alive to open.
func (p *Plugin) assignProcess() error {
	const rights = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE

	handle, err := windows.OpenProcess(rights, false, uint32(p.cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("open plugin process: %w", err)
	}

	defer func() { _ = windows.CloseHandle(handle) }()

	if err := windows.AssignProcessToJobObject(p.tree.job, handle); err != nil {
		return fmt.Errorf("assign plugin to job object: %w", err)
	}

	return nil
}

// stopProcess kills the job object, which takes every process in the tree with
// it, then waits for the child to be reaped. Windows has no SIGTERM, so the
// graceful drain Unix gets from closeGrace has no equivalent step here.
func (p *Plugin) stopProcess() error {
	defer func() {
		_ = windows.CloseHandle(p.tree.job)
		p.tree.job = 0
	}()

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
