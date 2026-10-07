package pipkin

import (
	"errors"
	"os"
	"path/filepath"
)

// A unique sibling keeps delayed cleanup from deleting a fresh installation.
func stageInstallationRemoval(dir string) (string, error) {
	tombstone, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+"-remove-*")
	if err != nil {
		return "", err
	}
	if err := os.Remove(tombstone); err != nil {
		return "", err
	}
	if err := os.Rename(dir, tombstone); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return tombstone, nil
}
