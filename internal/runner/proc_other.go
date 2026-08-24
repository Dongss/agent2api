//go:build !unix && !windows

package runner

import "os/exec"

// newProcTree returns the fallback for platforms with neither process groups
// nor job objects: the child is killed on its own, and anything it started
// outlives it.
func newProcTree() procTree { return unmanaged{} }

type unmanaged struct{}

func (unmanaged) prepare(*exec.Cmd)        {}
func (unmanaged) adopt(*exec.Cmd) error    { return nil }
func (unmanaged) kill(cmd *exec.Cmd) error { return killDirect(cmd) }
func (unmanaged) close()                   {}
