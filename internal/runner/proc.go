package runner

import "os/exec"

// procTree lets a timeout or cancellation take down a child's whole process
// tree. An agent CLI is often a wrapper, so killing it alone leaves the worker
// running. Any platform may implement none of this; the fallback is killing the
// direct child.
type procTree interface {
	// prepare configures cmd before Start. A failure is not worth reporting: an
	// ungrouped child still runs.
	prepare(cmd *exec.Cmd)

	// adopt finishes the setup once the child exists. An error means the tree
	// is not reclaimable as a unit, so kill will fall back to the child alone.
	adopt(cmd *exec.Cmd) error

	// kill ends the child and everything it spawned.
	kill(cmd *exec.Cmd) error

	// close releases whatever prepare acquired. It runs on every exit path.
	close()
}

// killDirect is the fallback every platform shares: end the process we started
// and nothing else.
func killDirect(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
