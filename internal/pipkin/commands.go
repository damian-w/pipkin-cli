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
	"strconv"
	"strings"
	"time"
	"unicode"
)

var repository = envOr("PIPKIN_REPOSITORY", "damian-w/pipkin-cli")

func installLauncher() error {
	if runtime.GOOS == "windows" {
		return updateUserPath(filepath.Dir(installedBinary()), false)
	}
	if err := os.MkdirAll(filepath.Dir(launcherPath()), 0o755); err != nil {
		return err
	}
	if target, err := os.Readlink(launcherPath()); err == nil && target == installedBinary() {
		return nil
	}
	return os.Symlink(installedBinary(), launcherPath())
}

func launcherMissing() (bool, error) {
	if runtime.GOOS == "windows" {
		present, err := userPathContains(filepath.Dir(installedBinary()))
		return !present, err
	}
	if _, err := os.Lstat(launcherPath()); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if target, err := os.Readlink(launcherPath()); err == nil && target == installedBinary() {
		return false, nil
	}
	return false, errors.New("an unrelated pipkin command already exists; left unchanged")
}

func installCommand(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("install takes no options; existing Codex and Claude sessions are detected automatically")
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(source); err == nil {
		source = resolved
	}
	lock, err := acquireFileLock(installationLockPath())
	if err != nil {
		return fmt.Errorf("could not lock installation: %w", err)
	}
	defer lock.Close()
	staged, err := stageLocalBinaries(source, filepath.Dir(installedBinary()), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("could not prepare the helper: %w", err)
	}
	defer staged.close()
	needsLauncher, err := launcherMissing()
	if err != nil {
		return err
	}
	if _, err := initializedConfig(); err != nil {
		return err
	}
	launcherCreated := false
	actions := helperInstallationActions()
	actions.configure = func() error {
		if _, err := updateConfig(removeLegacyClaudeHook); err != nil {
			return err
		}
		// PATH may be written even if notifying Windows fails; roll it back too.
		launcherCreated = needsLauncher
		if err := installLauncher(); err != nil {
			return err
		}
		return nil
	}
	actions.undoConfigure = func() error {
		if !launcherCreated {
			return nil
		}
		if runtime.GOOS == "windows" {
			return updateUserPath(filepath.Dir(installedBinary()), true)
		}
		if target, err := os.Readlink(launcherPath()); err == nil && target == installedBinary() {
			return os.Remove(launcherPath())
		}
		return nil
	}
	mechanism, err := activateBinaries(staged, actions)
	if err != nil {
		return fmt.Errorf("could not install the helper: %w", err)
	}
	fmt.Printf("Installed Pipkin helper %s.\n", version)
	fmt.Println("Codex and Claude usage is detected automatically from your existing app sessions.")
	fmt.Printf("Starts automatically via %s.\n", mechanism)
	fmt.Println("Helper running.")
	if runtime.GOOS == "windows" && staged.missingCompanion {
		fmt.Println("No windowless helper was supplied; startup uses the console helper.")
	}
	if runtime.GOOS == "windows" {
		fmt.Println("Open a new terminal to use the `pipkin` command.")
	} else if !onPath(filepath.Dir(launcherPath())) {
		fmt.Println("Add ~/.local/bin to your PATH, or run ~/.local/bin/pipkin directly.")
	}
	if hint := serialPermissionHint(); hint != "" {
		fmt.Println(hint)
	}
	return nil
}

func onPath(folder string) bool {
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == folder {
			return true
		}
	}
	return false
}

func describeWindow(label string, window *Window, now int64) string {
	label = terminalText(label)
	if window == nil {
		return label + " unknown"
	}
	if window.NotStarted {
		return label + " starts with your first message"
	}
	text := fmt.Sprintf("%s %.0f%% left", label, float64(1000-window.Used)/10)
	if window.Reset != 0 {
		left := window.Reset - now
		switch {
		case left <= 0:
			text += " (awaiting refresh)"
		case left >= 86400:
			text += fmt.Sprintf(" (resets in %dd %dh)", left/86400, left%86400/3600)
		default:
			text += fmt.Sprintf(" (resets in %dh %dm)", left/3600, left%3600/60)
		}
	}
	return text
}

func terminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return '\uFFFD'
		}
		return r
	}, value)
}

func statusCommand() error {
	_, err := os.Stat(installedBinary())
	installed := err == nil
	pid, err := runningPID()
	if err != nil {
		return err
	}
	switch {
	case pid != 0:
		fmt.Printf("Pipkin helper %s: running (pid %d)\n", version, pid)
	case installed:
		fmt.Printf("Pipkin helper %s: stopped. Start it with `pipkin start`.\n", version)
		return nil
	default:
		fmt.Printf("Pipkin helper %s: not installed\n", version)
		return nil
	}
	var status Status
	if err := readJSON(statusPath(), &status); err != nil {
		return fmt.Errorf("could not read helper snapshot: %w", err)
	}
	if port := status.Device["port"]; port != "" {
		fmt.Printf("  Display: connected on %s (firmware %s)\n", port, status.Device["firmware"])
	} else if reason := status.Device["error"]; reason != "" {
		fmt.Printf("  Display: not found (%s)\n", reason)
	} else {
		fmt.Println("  Display: not found")
	}
	now := time.Now().Unix()
	for _, provider := range []struct{ key, name string }{{"codex", "Codex"}, {"claude", "Claude"}} {
		reading := status.Readings[provider.key]
		switch {
		case reading != nil:
			session := describeWindow("session", reading.Session, now)
			if reading.SessionNoCap {
				session = "session no cap"
			}
			line := session + ", " +
				describeWindow("weekly", reading.Weekly, now)
			if reading.Observed != 0 {
				line += fmt.Sprintf(", read %d min ago", (now-reading.Observed)/60)
			}
			if message := status.providerError(provider.key); message != "" {
				line += "; " + message
			}
			fmt.Printf("  %s: %s\n", provider.name, line)
		case status.providerError(provider.key) != "":
			fmt.Printf("  %s: no reading (%s)\n", provider.name, status.providerError(provider.key))
		default:
			fmt.Printf("  %s: no reading yet\n", provider.name)
		}
	}
	return nil
}

func startCommand() error {
	maintenance, err := acquireFileLock(maintenanceLockPath())
	if err != nil {
		return fmt.Errorf("could not start the helper while firmware maintenance is in progress: %w", err)
	}
	maintenance.Close()
	if _, err := os.Stat(installedBinary()); err != nil {
		return errors.New("Pipkin is not installed")
	}
	if err := startHelper(); err != nil {
		return err
	}
	fmt.Println("Pipkin helper running.")
	return nil
}

func stopCommand() error {
	if err := stopHelper(); err != nil {
		return err
	}
	fmt.Println("Pipkin helper stopped. It starts again at next sign-in; `pipkin start` resumes now.")
	return nil
}

func restartCommand() error {
	lock, err := acquireFileLock(installationLockPath())
	if err != nil {
		return fmt.Errorf("could not restart the helper during another installation or firmware operation: %w", err)
	}
	defer lock.Close()
	if _, err := os.Stat(installedBinary()); err != nil {
		return errors.New("Pipkin is not installed")
	}
	maintenance, err := acquireFileLock(maintenanceLockPath())
	if err != nil {
		return fmt.Errorf("could not restart the helper while firmware maintenance is in progress: %w", err)
	}
	// runCommand needs this lock before it can publish the new helper's PID.
	if err := maintenance.Close(); err != nil {
		return err
	}
	if err := restartHelper(stopHelper, startHelper); err != nil {
		return err
	}
	fmt.Println("Pipkin helper restarted.")
	return nil
}

func restartHelper(stop, start func() error) error {
	if err := stop(); err != nil {
		return fmt.Errorf("could not stop the helper for restart: %w", err)
	}
	if err := start(); err != nil {
		return fmt.Errorf("could not restart the helper; run pipkin start to try again: %w", err)
	}
	return nil
}

func authorizeInstalledHelper() {
	authorizeInstalledHelperFor(runtime.GOOS, claudeDesktopSignedIn, (*exec.Cmd).Run, os.Stdout)
}

func authorizeInstalledHelperFor(goos string, signedIn func() bool, run func(*exec.Cmd) error, output io.Writer) {
	if goos != "darwin" || !signedIn() {
		return
	}
	// The updater is still the old executable. Keychain permission must be given
	// to the replacement at its final path, before its first background read.
	command := exec.Command(installedBinary(), "authorize")
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, output, os.Stderr
	if run(command) != nil {
		fmt.Fprintln(output, "Claude permission is not ready; run pipkin authorize, then pipkin restart to try again.")
	}
}

var httpClient = &http.Client{Timeout: 60 * time.Second}

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
	tag, err := latestTag()
	if err != nil {
		return fmt.Errorf("update check failed: %w", err)
	}
	if !newerVersion(tag, version) {
		fmt.Printf("Pipkin helper %s is up to date.\n", version)
		return nil
	}
	lock, err := acquireFileLock(installationLockPath())
	if err != nil {
		return fmt.Errorf("could not lock installation: %w", err)
	}
	defer lock.Close()
	release := fmt.Sprintf("https://github.com/%s/releases/download/%s", repository, tag)
	staged, err := stageReleaseBinaries(release, filepath.Dir(installedBinary()), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("update failed: %w; installed binaries were unchanged", err)
	}
	defer staged.close()
	if _, err := initializedConfig(); err != nil {
		return err
	}
	actions := helperInstallationActions()
	actions.configure = func() error {
		_, err := updateConfig(removeLegacyClaudeHook)
		return err
	}
	if _, err := activateBinaries(staged, actions); err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	fmt.Printf("Updated Pipkin helper %s -> %s.\n", version, strings.TrimPrefix(tag, "v"))
	return nil
}

func uninstallCommand() error {
	lock, err := acquireFileLock(installationLockPath())
	if err != nil {
		return fmt.Errorf("could not lock installation: %w", err)
	}
	defer lock.Close()
	if err := stopHelper(); err != nil {
		return err
	}
	if err := removeAutostart(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if err := updateUserPath(filepath.Dir(installedBinary()), true); err != nil {
			return err
		}
	} else if target, err := os.Readlink(launcherPath()); err == nil && target == installedBinary() {
		if err := os.Remove(launcherPath()); err != nil {
			return err
		}
	}
	if err := removeInstallation(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		fmt.Println("Pipkin helper removal scheduled after this command exits.")
	} else {
		fmt.Println("Pipkin helper removed.")
	}
	return nil
}
