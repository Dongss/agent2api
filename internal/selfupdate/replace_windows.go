//go:build windows

package selfupdate

import (
	"fmt"
	"os"
)

// swap installs the staged binary on Windows, which refuses to overwrite the
// executable of a running process. Renaming the old file out of the way is
// allowed, though, so the swap becomes two renames.
//
// The displaced file cannot be deleted while this process holds it open, so it
// is left behind for the next update to clear rather than failing an update
// that has otherwise succeeded.
func swap(dest, staged string) error {
	old := dest + ".old"
	// A leftover from a previous update: it is no longer running, so it goes now.
	_ = os.Remove(old)

	if err := os.Rename(dest, old); err != nil {
		return fmt.Errorf("cannot move the running binary %s aside: %w", dest, err)
	}
	if err := os.Rename(staged, dest); err != nil {
		// Put the old binary back: a failed update must not leave the command
		// missing from PATH.
		_ = os.Rename(old, dest)
		return fmt.Errorf("cannot install the new binary at %s: %w", dest, err)
	}
	_ = os.Remove(old)
	return nil
}
