package pipkin

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type removalTarget struct {
	dir        string
	info       os.FileInfo
	home, work string
}

// Uninstall verifies the folder before touching the helper, startup entry or
// launcher. A relative override never resolves to the current folder.
func installationRemovalTarget() (removalTarget, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return removalTarget{}, err
	}
	work, err := os.Getwd()
	if err != nil {
		return removalTarget{}, err
	}
	return inspectRemovalTarget(configuredAppDir(), home, work)
}

func inspectRemovalTarget(dir, home, work string) (removalTarget, error) {
	var target removalTarget
	if !filepath.IsAbs(dir) {
		return target, fmt.Errorf("Pipkin's folder must be a full path, not %s.", terminalText(dir))
	}
	dir = filepath.Clean(dir)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		// Already deleted: still remove automatic startup and the command.
		return removalTarget{dir: dir, home: home, work: work}, nil
	}
	if err != nil {
		return target, fmt.Errorf("Couldn't check Pipkin's folder: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return target, fmt.Errorf("%s is a file or link, not Pipkin's folder. Check PIPKIN_HOME.", terminalText(dir))
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return target, err
	}
	if filepath.Dir(canonical) == canonical {
		return target, errors.New("Pipkin's folder can't be a filesystem root. Check PIPKIN_HOME.")
	}
	for _, protected := range []string{home, work} {
		if !filepath.IsAbs(protected) {
			return target, errors.New("Couldn't check your home and current folders.")
		}
		resolved, err := filepath.EvalSymlinks(protected)
		if err != nil {
			return target, fmt.Errorf("Couldn't check your home and current folders: %w", err)
		}
		for {
			ancestor, err := os.Stat(resolved)
			if err != nil {
				return target, fmt.Errorf("Couldn't check your home and current folders: %w", err)
			}
			if os.SameFile(info, ancestor) {
				return target, errors.New("Pipkin's folder can't be your home or current folder, or contain them. Run uninstall from another folder and check PIPKIN_HOME.")
			}
			parent := filepath.Dir(resolved)
			if parent == resolved {
				break
			}
			resolved = parent
		}
	}
	if err := validateRemovalContents(canonical, dir); err != nil {
		return target, err
	}
	return removalTarget{dir: canonical, info: info, home: home, work: work}, nil
}

func validateRemovalContents(dir, shown string) error {
	for _, marker := range []string{"config.json", filepath.Join("bin", executableName("pipkin"))} {
		info, err := os.Lstat(filepath.Join(dir, marker))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%s doesn't look like Pipkin's folder, so it wasn't removed. Check PIPKIN_HOME.", terminalText(shown))
		}
	}
	unexpected := fmt.Errorf("%s has files or links Pipkin didn't create. Move them elsewhere, then try again.", terminalText(shown))
	// Delayed cleanup must never recurse through a link or junction.
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return unexpected
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := entry.Name()
		switch filepath.Dir(rel) {
		case ".":
			if rel == "." {
				return nil
			}
			directory := name == "bin" || name == "cache" || name == "tools"
			file := name == "config.json" || name == "config.lock" || name == "status.json" ||
				name == "helper.lock" || name == "helper.pid" || name == "helper.log" || name == "helper.log.1" || name == ".DS_Store" ||
				strings.HasPrefix(name, "config.json.backup-") || strings.HasPrefix(name, ".pipkin-")
			if directory != info.IsDir() || !directory && !file {
				return unexpected
			}
		case "bin":
			staging := strings.HasPrefix(name, ".pipkin-stage-")
			known := name == executableName("pipkin") || name == executableName("pipkin")+".old" || name == ".DS_Store" ||
				runtime.GOOS == "windows" && (name == "pipkinw.exe" || name == "pipkinw.exe.old")
			if staging != info.IsDir() || !staging && !known {
				return unexpected
			}
		}
		return nil
	})
}

func (target removalTarget) recheck() error {
	current, err := inspectRemovalTarget(target.dir, target.home, target.work)
	if err != nil {
		return err
	}
	if (target.info != nil || current.info != nil) && !os.SameFile(target.info, current.info) {
		return errors.New("Pipkin's folder changed during uninstall. Try again.")
	}
	return nil
}

func stageVerifiedInstallationRemoval(target removalTarget) (string, error) {
	if err := target.recheck(); err != nil || target.info == nil {
		return "", err
	}
	tombstone, err := stageInstallationRemoval(target.dir)
	if err != nil || tombstone == "" {
		return tombstone, err
	}
	info, checkErr := os.Lstat(tombstone)
	if checkErr == nil && !os.SameFile(target.info, info) {
		checkErr = errors.New("Pipkin's folder changed during uninstall.")
	}
	if checkErr == nil {
		checkErr = validateRemovalContents(tombstone, target.dir)
	}
	if checkErr != nil {
		// Keep an unexpected folder, and any new installation at the old path.
		if _, err := os.Lstat(target.dir); errors.Is(err, os.ErrNotExist) {
			if restoreErr := os.Rename(tombstone, target.dir); restoreErr == nil {
				return "", checkErr
			}
		}
		return "", fmt.Errorf("%w Its files were kept at %s.", checkErr, terminalText(tombstone))
	}
	return tombstone, nil
}

type uninstallDependencies struct {
	goos                    string
	target                  func() (removalTarget, error)
	lock                    func() (io.Closer, error)
	stop, startup, launcher func() error
	remove                  func(removalTarget) error
}

func runUninstallCommand(out io.Writer, deps uninstallDependencies) error {
	target, err := deps.target()
	if err != nil {
		return err
	}
	lock, err := deps.lock()
	if err != nil {
		return commandLockError("uninstall", err)
	}
	defer lock.Close()
	if err := target.recheck(); err != nil {
		return err
	}
	for _, action := range []func() error{deps.stop, deps.startup, deps.launcher} {
		if err := action(); err != nil {
			return err
		}
	}
	if err := deps.remove(target); err != nil {
		return fmt.Errorf("Pipkin was stopped and removed from startup, but its folder was kept. %w", err)
	}
	if deps.goos == "windows" {
		return writeOutputLine(out, "", "Pipkin was removed. Its folder is deleted when this command exits.")
	}
	return writeOutputLine(out, "", "Pipkin was removed.")
}
