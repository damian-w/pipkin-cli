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

// Uninstall must verify the directory before touching the helper, startup
// entries or command launcher. A resolved relative override is not consent to
// recursively remove the current directory.
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
		return target, errors.New("Uninstall requires an absolute Pipkin folder. Unset a relative PIPKIN_HOME override and try again.")
	}
	dir = filepath.Clean(dir)
	info, err := os.Lstat(dir)
	if err != nil {
		return target, fmt.Errorf("Couldn't verify the Pipkin folder for uninstall: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return target, errors.New("Uninstall requires a real Pipkin folder, not a file or directory link.")
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return target, err
	}
	if filepath.Dir(canonical) == canonical {
		return target, errors.New("Refusing to uninstall from a filesystem root.")
	}
	for _, protected := range []string{home, work} {
		if !filepath.IsAbs(protected) {
			return target, errors.New("Couldn't verify protected folders for uninstall.")
		}
		resolved, err := filepath.EvalSymlinks(protected)
		if err != nil {
			return target, fmt.Errorf("Couldn't verify protected folders for uninstall: %w", err)
		}
		for {
			ancestor, err := os.Stat(resolved)
			if err != nil {
				return target, fmt.Errorf("Couldn't verify protected folders for uninstall: %w", err)
			}
			if os.SameFile(info, ancestor) {
				return target, errors.New("Refusing to remove your home folder, current folder or one of their parents. Run uninstall from outside Pipkin's folder and check PIPKIN_HOME.")
			}
			parent := filepath.Dir(resolved)
			if parent == resolved {
				break
			}
			resolved = parent
		}
	}
	if err := validateRemovalContents(canonical); err != nil {
		return target, err
	}
	return removalTarget{dir: canonical, info: info, home: home, work: work}, nil
}

func validateRemovalContents(dir string) error {
	for _, marker := range []string{"config.json", filepath.Join("bin", executableName("pipkin"))} {
		info, err := os.Lstat(filepath.Join(dir, marker))
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("This folder is not a verified Pipkin installation. Uninstall requires config.json and the installed Pipkin app; check PIPKIN_HOME.")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		directory := name == "bin" || name == "cache" || name == "tools"
		file := name == "config.json" || name == "config.lock" || name == "status.json" || name == "boards.json" ||
			name == "helper.lock" || name == "helper.pid" || name == "helper.log" || name == "helper.log.1" || name == ".DS_Store" ||
			strings.HasPrefix(name, "config.json.backup-") || strings.HasPrefix(name, ".pipkin-")
		info, err := entry.Info()
		if err != nil || directory && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) || !directory && (!file || !info.Mode().IsRegular()) {
			return errors.New("The Pipkin folder contains unrecognized files or links. Move those files elsewhere before uninstalling; nothing was removed.")
		}
	}
	entries, err = os.ReadDir(filepath.Join(dir, "bin"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		known := name == executableName("pipkin") || name == executableName("pipkin")+".old" || name == ".DS_Store"
		if runtime.GOOS == "windows" {
			known = known || name == "pipkinw.exe" || name == "pipkinw.exe.old"
		}
		info, err := entry.Info()
		staging := strings.HasPrefix(name, ".pipkin-stage-")
		if err != nil || staging && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) || !staging && (!known || !info.Mode().IsRegular()) {
			return errors.New("Pipkin's bin folder contains unrecognized files or links. Move those files elsewhere before uninstalling; nothing was removed.")
		}
	}
	// In particular, delayed cleanup must never recurse through a junction or
	// link in a cache. WalkDir inspects links themselves instead of following.
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("The Pipkin folder contains a link or special file. Move it elsewhere before uninstalling; nothing was removed.")
		}
		return nil
	})
}

func (target removalTarget) recheck() error {
	current, err := inspectRemovalTarget(target.dir, target.home, target.work)
	if err != nil {
		return err
	}
	if !os.SameFile(target.info, current.info) {
		return errors.New("The Pipkin folder changed during uninstall. Nothing was deleted; try again.")
	}
	return nil
}

func stageVerifiedInstallationRemoval(target removalTarget) (string, error) {
	if err := target.recheck(); err != nil {
		return "", err
	}
	tombstone, err := stageInstallationRemoval(target.dir)
	if err != nil || tombstone == "" {
		return tombstone, err
	}
	info, checkErr := os.Lstat(tombstone)
	if checkErr == nil && !os.SameFile(target.info, info) {
		checkErr = errors.New("the Pipkin folder changed during uninstall")
	}
	if checkErr == nil {
		checkErr = validateRemovalContents(tombstone)
	}
	if checkErr != nil {
		// Keep an unexpected directory intact, including if another installation
		// appeared at the original path while this one was being staged.
		if _, err := os.Lstat(target.dir); errors.Is(err, os.ErrNotExist) {
			if restoreErr := os.Rename(tombstone, target.dir); restoreErr == nil {
				return "", checkErr
			}
		}
		return "", fmt.Errorf("%w; files were retained at %s", checkErr, terminalText(tombstone))
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
		return err
	}
	if deps.goos == "windows" {
		return writeOutputLine(out, "", "Pipkin removal is scheduled after this command exits.")
	}
	return writeOutputLine(out, "", "Pipkin was removed.")
}
