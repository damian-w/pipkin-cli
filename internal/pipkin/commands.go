package pipkin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var repository = envOr("PIPKIN_REPOSITORY", "damian-w/pipkin-cli")

func lockInstallation() (io.Closer, error) { return acquireFileLock(installationLockPath()) }

func authorizeReplacement(noticeOut, warningOut io.Writer) {
	authorizeInstalledHelperWithNoticeFor(runtime.GOOS, claudeDesktopSignedIn, (*exec.Cmd).Run, noticeOut, warningOut)
}

func installCommand() error {
	return runInstallCommand(os.Stdout, installCommandDeps{
		buildVersion: version, goos: runtime.GOOS,
		lock: lockInstallation,
		stage: func() (*stagedBinaries, error) {
			source, err := os.Executable()
			if err != nil {
				return nil, err
			}
			if resolved, err := filepath.EvalSymlinks(source); err == nil {
				source = resolved
			}
			return stageLocalBinaries(source, filepath.Dir(installedBinary()), runtime.GOOS, runtime.GOARCH)
		},
		launcherMissing: launcherMissing, config: initializedInstallConfig,
		actions: helperInstallationActions(), activate: activateBinaries,
		configure: installLauncher, undo: removeLauncher,
		authorize: authorizeReplacement,
		hint:      commandHint, pathFix: installPathFix(runtime.GOOS, launcherPath(), onPath(filepath.Dir(launcherPath()))),
		permissionHint: serialPermissionHint,
	})
}

func onPath(folder string) bool {
	return slices.Contains(filepath.SplitList(os.Getenv("PATH")), folder)
}

func terminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return '\uFFFD'
		}
		return r
	}, value)
}

func startCommand() error {
	maintenance, err := acquireFileLock(maintenanceLockPath())
	if err != nil {
		return commandLockError("start", err)
	}
	maintenance.Close()
	if _, err := os.Stat(installedBinary()); err != nil {
		return errors.New("Pipkin is not installed. Run 'pipkin install' first.")
	}
	running, err := helperRunning()
	if err != nil {
		return err
	}
	if err := startHelper(); err != nil {
		return commandPhaseError{message: "Pipkin couldn't start.", cause: err}
	}
	if running {
		fmt.Println("Pipkin is already running.")
	} else {
		fmt.Println("Pipkin is running.")
	}
	return nil
}

func stopCommand() error {
	running, err := helperRunning()
	if err != nil {
		return err
	}
	if err := stopHelper(); err != nil {
		return commandPhaseError{message: "Pipkin couldn't stop.", cause: err}
	}
	if running {
		fmt.Println("Pipkin is stopped. It starts again when you next log in to this computer.")
	} else {
		fmt.Println("Pipkin was already stopped.")
	}
	fmt.Printf("To start it now: %s\n", commandHint("start"))
	return nil
}

func restartCommand() error {
	lock, err := acquireFileLock(installationLockPath())
	if err != nil {
		return commandLockError("restart", err)
	}
	defer lock.Close()
	if _, err := os.Stat(installedBinary()); err != nil {
		return errors.New("Pipkin is not installed. Run 'pipkin install' first.")
	}
	maintenance, err := acquireFileLock(maintenanceLockPath())
	if err != nil {
		return commandLockError("restart", err)
	}
	// runCommand needs this lock before it can publish the new helper's PID.
	if err := maintenance.Close(); err != nil {
		return err
	}
	if err := restartHelper(stopHelper, startHelper); err != nil {
		return commandPhaseError{message: "Pipkin couldn't restart.", cause: err}
	}
	fmt.Println("Pipkin is running again.")
	return nil
}

func restartHelper(stop, start func() error) error {
	if err := stop(); err != nil {
		return fmt.Errorf("Couldn't stop Pipkin for restart: %w", err)
	}
	if err := start(); err != nil {
		return fmt.Errorf("Couldn't restart Pipkin. Run %s to try again: %w", commandHint("start"), err)
	}
	return nil
}

func authorizeInstalledHelperWithNoticeFor(goos string, signedIn func() bool, run func(*exec.Cmd) error, noticeOut, warningOut io.Writer) {
	if goos != "darwin" || !signedIn() {
		return
	}
	_ = writeOutputLine(noticeOut, "", authorizePromptNotice)
	// The updater is still the old executable. Keychain permission must be given
	// to the replacement at its final path, before its first background read.
	command := exec.Command(installedBinary(), "authorize")
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, io.Discard, io.Discard
	command.Env = append(os.Environ(), "PIPKIN_AUTHORIZE_NESTED=1")
	if run(command) != nil {
		fmt.Fprintf(warningOut, "Pipkin can't read Claude's sign-in yet. Run %s.\n", commandHint("authorize"))
	}
}

func latestTag() (string, error) {
	client := &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Head(fmt.Sprintf("https://github.com/%s/releases/latest", repository))
	if err != nil {
		return "", err
	}
	response.Body.Close()
	location := response.Header.Get("Location")
	prefix := fmt.Sprintf("https://github.com/%s/releases/tag/", repository)
	if response.StatusCode < 300 || response.StatusCode >= 400 || !strings.HasPrefix(location, prefix) {
		return "", errors.New("no published release found")
	}
	tag := strings.TrimPrefix(location, prefix)
	if tag == "" || strings.ContainsAny(tag, "/?#") {
		return "", errors.New("invalid release version")
	}
	return tag, nil
}

func newerVersion(candidate, current string) bool {
	parse := func(text string) []int {
		text, _, _ = strings.Cut(strings.TrimPrefix(text, "v"), "-")
		var parts []int
		for _, part := range strings.Split(text, ".") {
			value, _ := strconv.Atoi(part)
			parts = append(parts, value)
		}
		return parts
	}
	a, b := parse(candidate), parse(current)
	for i := 0; i < max(len(a), len(b)); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return strings.Contains(current, "-")
}

func updateCommand() error {
	return runUpdateCommand(os.Stdout, updateCommandDeps{
		buildVersion: version, latest: latestTag,
		lock: lockInstallation,
		stage: func(tag string) (*stagedBinaries, error) {
			release := fmt.Sprintf("https://github.com/%s/releases/download/%s", repository, tag)
			return stageReleaseBinaries(release, filepath.Dir(installedBinary()), runtime.GOOS, runtime.GOARCH)
		},
		config: initializedConfig, actions: helperInstallationActions(), activate: activateBinaries,
		authorize: authorizeReplacement,
		advisory: func(out io.Writer) {
			writeFirmwareAdvisory(out, cachedDisplayFirmware, latestFirmwareAdvisoryTag, commandHint)
		},
	})
}

func uninstallCommand() error {
	return runUninstallCommand(os.Stdout, uninstallDependencies{
		goos: runtime.GOOS, target: installationRemovalTarget,
		lock: lockInstallation,
		stop: stopHelper, startup: removeAutostart, launcher: removeLauncher, remove: removeInstallation,
	})
}
