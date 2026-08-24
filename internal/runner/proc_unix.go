//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// newProcTree returns the unix implementation: a process group, which the child
// inherits at exec and which can be signalled as a whole.
func newProcTree() procTree { return processGroup{} }

type processGroup struct{}

// prepare puts the child in its own process group, so its descendants are
// reachable by a single signal.
func (processGroup) prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// adopt has nothing to do: the group is set up at exec, before the child can
// spawn anything.
func (processGroup) adopt(*exec.Cmd) error { return nil }

// kill signals the whole group.
func (processGroup) kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative pid means "the group led by this pid".
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		// The group may already be gone, or prepare may not have run.
		return killDirect(cmd)
	}
	return nil
}

func (processGroup) close() {}
