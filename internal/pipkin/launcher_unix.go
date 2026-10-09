//go:build darwin || linux

package pipkin

import (
	"errors"
	"os"
	"path/filepath"
)

func ownsLauncher() bool {
	target, err := os.Readlink(launcherPath())
	return err == nil && target == installedBinary()
}

func installLauncher() error {
	if ownsLauncher() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(launcherPath()), 0o755); err != nil {
		return err
	}
	return os.Symlink(installedBinary(), launcherPath())
}

func launcherMissing() (bool, error) {
	if _, err := os.Lstat(launcherPath()); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if ownsLauncher() {
		return false, nil
	}
	return false, errors.New("an unrelated pipkin command already exists; left unchanged")
}

func removeLauncher() error {
	if ownsLauncher() {
		return os.Remove(launcherPath())
	}
	return nil
}

func removeInstallation() error { return os.RemoveAll(appDir()) }
