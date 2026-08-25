package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
)

// replace installs bin over dest.
//
// The new binary is written beside the old one and renamed into place, so the
// swap is atomic: an update killed halfway leaves either the old binary or the
// new one on disk, never a truncated file that no longer runs. Writing to the
// same directory is what makes the rename atomic — a temp file on another
// filesystem would degrade to a copy.
func replace(dest string, bin []byte) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".agent2api-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	staged := tmp.Name()
	// Cleans up every failure path below; a no-op once the rename has moved it.
	defer os.Remove(staged)

	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot write %s: %w", staged, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write %s: %w", staged, err)
	}
	// CreateTemp makes the file 0600; an executable needs the x bits, and the
	// release binary is not secret.
	if err := os.Chmod(staged, 0o755); err != nil {
		return fmt.Errorf("cannot make %s executable: %w", staged, err)
	}
	if err := swap(dest, staged); err != nil {
		return err
	}
	return nil
}
