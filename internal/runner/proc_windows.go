//go:build windows

package runner

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// newProcTree returns the Windows implementation: a job object, which is the
// platform's equivalent of a process group for this purpose. Everything
// assigned to a job — and everything those processes go on to start — can be
// terminated together.
func newProcTree() procTree { return &jobObject{} }

type jobObject struct {
	handle windows.Handle
}

// prepare creates the job the child will join. There is nothing for a child to
// inherit at creation time on Windows, so adopt does the assigning.
//
// A failure is not fatal: handle stays zero, adopt does nothing, and kill falls
// back to the child alone.
func (j *jobObject) prepare(*exec.Cmd) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}

	// KILL_ON_JOB_CLOSE is the belt to kill's braces: whatever is still running
	// in the job dies when the last handle to it closes, so a gateway that
	// crashes outright does not leave a CLI behind either.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(handle)
		return
	}
	j.handle = handle
}

// adopt assigns the started child to the job.
//
// Between the child being created and this call, a grandchild it starts would
// escape the job. Closing that window means creating the process suspended,
// which os/exec cannot do — and a CLI spends its first milliseconds starting a
// runtime, so the window is theoretical. Worth knowing, not worth hand-rolling
// process creation for.
func (j *jobObject) adopt(cmd *exec.Cmd) error {
	if j.handle == 0 || cmd.Process == nil {
		return nil
	}
	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("runner: cannot open the child process to group it: %w", err)
	}
	defer windows.CloseHandle(proc)

	if err := windows.AssignProcessToJobObject(j.handle, proc); err != nil {
		return fmt.Errorf("runner: cannot assign the child to its job: %w", err)
	}
	return nil
}

// kill terminates every process in the job, falling back to the child alone if
// there is no job to terminate.
func (j *jobObject) kill(cmd *exec.Cmd) error {
	if j.handle != 0 {
		if err := windows.TerminateJobObject(j.handle, 1); err == nil {
			return nil
		}
	}
	return killDirect(cmd)
}

func (j *jobObject) close() {
	if j.handle != 0 {
		windows.CloseHandle(j.handle)
		j.handle = 0
	}
}
