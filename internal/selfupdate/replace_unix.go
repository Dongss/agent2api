//go:build !windows

package selfupdate

import (
	"fmt"
	"os"
)

// swap renames the staged binary over the running one. Unix lets a running
// executable be replaced: the process keeps its open inode, and the next
// invocation gets the new file.
func swap(dest, staged string) error {
	if err := os.Rename(staged, dest); err != nil {
		return fmt.Errorf("cannot install the new binary at %s: %w", dest, err)
	}
	return nil
}
