package pipkin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const authorizePromptNotice = "If macOS asks for your Mac login password to read Claude's sign-in, enter it and choose Always Allow."

type installCommandDeps struct {
	buildVersion, goos string
	lock               func() (io.Closer, error)
	stage              func() (*stagedBinaries, error)
	launcherMissing    func() (bool, error)
	config             func() (Config, string, error)
	actions            installationActions
	activate           func(*stagedBinaries, installationActions) error
	configure, undo    func() error
	authorize          func(io.Writer, io.Writer)
	hint               func(...string) string
	pathFix            string
	permissionHint     func() string
}

type updateCommandDeps struct {
	buildVersion string
	latest       func() (string, error)
	lock         func() (io.Closer, error)
	stage        func(string) (*stagedBinaries, error)
	config       func() (Config, error)
	actions      installationActions
	activate     func(*stagedBinaries, installationActions) error
	authorize    func(io.Writer, io.Writer)
	advisory     func(io.Writer)
}

func commandLockError(action string, err error) error {
	message := "Couldn't " + action + " Pipkin. Check the settings folder's permissions."
	if errors.Is(err, errLockBusy) {
		message = "Another Pipkin install, update or firmware operation is running. Try again shortly."
	} else if !errors.Is(err, os.ErrPermission) {
		message = "Couldn't " + action + " Pipkin. Try again."
	}
	return commandPhaseError{message: message, cause: err, quiet: true}
}

// commandPhaseError puts the outcome first and the technical cause after it.
// A quiet error shows only the outcome.
type commandPhaseError struct {
	message string
	cause   error
	quiet   bool
}

func (e commandPhaseError) Unwrap() error { return e.cause }
func (e commandPhaseError) Error() string {
	if e.quiet {
		return e.message
	}
	var text bytes.Buffer
	_ = writeOutputLine(&text, "", e.message)
	_ = writeOutputLine(&text, "Details: ", e.cause.Error())
	return strings.TrimSuffix(text.String(), "\n")
}

func runInstallCommand(out io.Writer, deps installCommandDeps) error {
	lock, err := deps.lock()
	if err != nil {
		return commandLockError("install", err)
	}
	defer lock.Close()
	staged, err := deps.stage()
	if err != nil {
		return commandPhaseError{message: "Couldn't prepare Pipkin for installation.", cause: err}
	}
	defer staged.close()
	needsLauncher, err := deps.launcherMissing()
	if err != nil {
		return commandPhaseError{message: "Couldn't set up the 'pipkin' command.", cause: err}
	}
	_, backup, err := deps.config()
	if err != nil {
		return installationConfigError(err, backup)
	}
	launcherCreated := false
	actions := deps.actions
	actions.configure = func() error {
		launcherCreated = needsLauncher
		return deps.configure()
	}
	actions.undoConfigure = func() error {
		if launcherCreated {
			return deps.undo()
		}
		return nil
	}
	var authorization bytes.Buffer
	actions.authorize = func() { deps.authorize(out, &authorization) }
	if err := deps.activate(staged, actions); err != nil {
		return installationConfigError(commandPhaseError{message: "Couldn't finish installing Pipkin.", cause: err}, backup)
	}
	lines := lineOutput{w: out}
	lines.printf("Pipkin %s is installed and running.\nIt starts automatically when you log in.\n", deps.buildVersion)
	if backup != "" {
		lines.printf("Pipkin's settings were damaged, so they were reset. The old file is at %s.\n", terminalText(backup))
	}
	lines.printf("%s", authorization.String())
	lines.printf("\nNext: %s\n", deps.hint("status"))
	if deps.pathFix != "" {
		lines.printf("To use 'pipkin' in this terminal (zsh or bash), paste:\n  %s\n", deps.pathFix)
	} else if deps.goos == "windows" {
		lines.printf("Open a new terminal to use the short 'pipkin' command.\n")
	}
	if deps.goos == "windows" && staged.missingCompanion {
		lines.printf("The windowless app was unavailable; automatic startup uses the console app.\n")
	}
	if hint := deps.permissionHint(); hint != "" {
		lines.printf("%s\n", terminalText(hint))
	}
	return lines.err
}

func installationConfigError(err error, backup string) error {
	if backup == "" {
		return err
	}
	return fmt.Errorf("%w; previous settings were saved at %s", err, terminalText(backup))
}

func runUpdateCommand(out io.Writer, deps updateCommandDeps) error {
	if _, err := fmt.Fprintln(out, "Checking for Pipkin updates..."); err != nil {
		return err
	}
	tag, err := deps.latest()
	if err != nil {
		var network *url.Error
		if errors.As(err, &network) {
			return commandPhaseError{message: "Couldn't reach GitHub to check for updates. Check your internet connection.", cause: err, quiet: true}
		}
		return commandPhaseError{message: "Couldn't check for Pipkin updates. Try again later.", cause: err}
	}
	if !newerVersion(tag, deps.buildVersion) {
		if _, err := fmt.Fprintf(out, "Pipkin %s is up to date.\n", deps.buildVersion); err != nil {
			return err
		}
		deps.advisory(out)
		return nil
	}
	lock, err := deps.lock()
	if err != nil {
		return commandLockError("update", err)
	}
	defer lock.Close()
	if _, err := fmt.Fprintf(out, "Downloading Pipkin %s...\n", strings.TrimPrefix(tag, "v")); err != nil {
		return err
	}
	staged, err := deps.stage(tag)
	if err != nil {
		return commandPhaseError{message: "Couldn't download Pipkin " + strings.TrimPrefix(tag, "v") + ". Installed files were not replaced.", cause: err}
	}
	defer staged.close()
	if _, err := deps.config(); err != nil {
		return commandPhaseError{message: "Pipkin's settings couldn't be read.", cause: err}
	}
	var authorization bytes.Buffer
	actions := deps.actions
	actions.authorize = func() { deps.authorize(out, &authorization) }
	if err := deps.activate(staged, actions); err != nil {
		return commandPhaseError{message: "Couldn't finish updating Pipkin.", cause: err}
	}
	lines := lineOutput{w: out}
	lines.printf("Pipkin %s is updated and running.\n%s", strings.TrimPrefix(tag, "v"), authorization.String())
	if lines.err != nil {
		return lines.err
	}
	// Firmware advice is optional and never fails an update.
	deps.advisory(out)
	return nil
}

func shellQuote(value string) string {
	if utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return !unicode.IsPrint(r) }) == -1 {
		return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	// ANSI-C quoting keeps control characters out of the terminal.
	var quoted strings.Builder
	quoted.WriteString("$'")
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if r == utf8.RuneError && size == 1 || !unicode.IsPrint(r) {
			for _, b := range []byte(value[:size]) {
				fmt.Fprintf(&quoted, "\\%03o", b)
			}
		} else {
			if r == '\\' || r == '\'' {
				quoted.WriteByte('\\')
			}
			quoted.WriteString(value[:size])
		}
		value = value[size:]
	}
	quoted.WriteByte('\'')
	return quoted.String()
}

func powerShellQuote(value string) string {
	if strings.IndexFunc(value, func(r rune) bool { return !unicode.IsPrint(r) }) == -1 {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	parts := []string{"''"}
	var plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			parts = append(parts, "'"+strings.ReplaceAll(plain.String(), "'", "''")+"'")
			plain.Reset()
		}
	}
	for _, r := range value {
		if unicode.IsPrint(r) {
			plain.WriteRune(r)
		} else {
			flush()
			parts = append(parts, fmt.Sprintf("[char]::ConvertFromUtf32(%d)", r))
		}
	}
	flush()
	return "(" + strings.Join(parts, " + ") + ")"
}

func commandHintFor(goos, executable string, onPATH bool, args ...string) string {
	command := "pipkin"
	if !onPATH {
		if goos == "windows" {
			command = "& " + powerShellQuote(executable)
		} else {
			command = shellQuote(executable)
		}
	}
	for _, arg := range args {
		if arg != "" && strings.IndexFunc(arg, func(c rune) bool {
			return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_./-", c))
		}) == -1 {
			command += " " + arg
			continue
		}
		if goos == "windows" {
			command += " " + powerShellQuote(arg)
		} else {
			command += " " + shellQuote(arg)
		}
	}
	return command
}

func commandHint(args ...string) string {
	executable := launcherPath()
	if runtime.GOOS == "windows" {
		executable = installedBinary()
	}
	return commandHintFor(runtime.GOOS, executable, onPath(filepath.Dir(executable)), args...)
}

func installPathFix(goos, launcher string, onPATH bool) string {
	if goos == "windows" || onPATH {
		return ""
	}
	return "export PATH=" + shellQuote(filepath.Dir(launcher)) + `:"$PATH"`
}

func latestFirmwareAdvisoryTag(ctx context.Context) (string, error) {
	source := publicFirmwareSource()
	release, err := source.release(ctx, source.api+"/releases/latest")
	return release.Tag, err
}

func writeFirmwareAdvisory(out io.Writer, saved func() (string, error), latest func(context.Context) (string, error), hint func(...string) string) {
	current, err := saved()
	if err != nil || !stableFirmwareVersion.MatchString(current) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tag, err := latest(ctx)
	if err != nil || !stableFirmwareVersion.MatchString(tag) || !newerVersion(tag, current) {
		return
	}
	fmt.Fprintf(out, "Display firmware %s is available (yours is %s). To install it, run: %s\n", strings.TrimPrefix(tag, "v"), strings.TrimPrefix(current, "v"), hint("flash"))
}

func cachedDisplayFirmware() (string, error) {
	data, err := readFileBounded(statusPath(), 1<<20)
	if err != nil {
		return "", err
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil {
		return "", err
	}
	return status.Device["firmware"], nil
}
