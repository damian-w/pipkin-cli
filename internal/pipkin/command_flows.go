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
	activate           func(*stagedBinaries, installationActions) (string, error)
	configure, undo    func() error
	authorize          func(io.Writer, io.Writer)
	hint               func(...string) string
	pathFix            string
	persistentPathFix  string
	permissionHint     func() string
}

type updateCommandDeps struct {
	buildVersion string
	latest       func() (string, error)
	lock         func() (io.Closer, error)
	stage        func(string) (*stagedBinaries, error)
	config       func() (Config, error)
	actions      installationActions
	activate     func(*stagedBinaries, installationActions) (string, error)
	authorize    func(io.Writer, io.Writer)
	advisory     func(io.Writer) error
}

func commandLockError(action string, err error) error {
	message := "Couldn't " + action + " Pipkin. Check the settings folder's permissions."
	if errors.Is(err, errLockBusy) {
		message = "Another Pipkin install, update or firmware operation is running. Try again shortly."
	} else if !errors.Is(err, os.ErrPermission) {
		message = "Couldn't " + action + " Pipkin. Try again."
	}
	return commandFlowError{message: message, cause: err}
}

type commandFlowError struct {
	message string
	cause   error
}

func (e commandFlowError) Error() string { return e.message }
func (e commandFlowError) Unwrap() error { return e.cause }

// commandPhaseError puts the outcome first and the technical cause after it.
type commandPhaseError struct {
	message string
	cause   error
}

func (e commandPhaseError) Unwrap() error { return e.cause }
func (e commandPhaseError) Error() string {
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
	if _, err := deps.activate(staged, actions); err != nil {
		return installationConfigError(commandPhaseError{message: "Couldn't finish installing Pipkin.", cause: err}, backup)
	}
	if _, err := fmt.Fprintf(out, "Pipkin %s is installed and running.\nIt starts automatically when you log in.\n", deps.buildVersion); err != nil {
		return err
	}
	if backup != "" {
		if _, err := fmt.Fprintf(out, "Pipkin's settings were damaged, so they were reset. The old file is at %s.\n", terminalText(backup)); err != nil {
			return err
		}
	}
	if _, err := io.Copy(out, &authorization); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "\nNext: %s\n", deps.hint("status")); err != nil {
		return err
	}
	if deps.pathFix != "" {
		if _, err := fmt.Fprintf(out, "To use 'pipkin' in this terminal (zsh or bash), paste:\n  %s\n", deps.pathFix); err != nil {
			return err
		}
		if deps.persistentPathFix != "" {
			if _, err := fmt.Fprintf(out, "For future terminals (optional), paste:\n  %s\n", deps.persistentPathFix); err != nil {
				return err
			}
		}
	} else if deps.goos == "windows" {
		if _, err := fmt.Fprintln(out, "Open a new terminal to use the short 'pipkin' command."); err != nil {
			return err
		}
	}
	if deps.goos == "windows" && staged.missingCompanion {
		if _, err := fmt.Fprintln(out, "The windowless app was unavailable; automatic startup uses the console app."); err != nil {
			return err
		}
	}
	if hint := deps.permissionHint(); hint != "" {
		_, err = fmt.Fprintln(out, terminalText(hint))
	}
	return err
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
			return commandFlowError{message: "Couldn't reach GitHub to check for updates. Check your internet connection.", cause: err}
		}
		return commandPhaseError{message: "Couldn't check for Pipkin updates. Try again later.", cause: err}
	}
	if !newerVersion(tag, deps.buildVersion) {
		if _, err := fmt.Fprintf(out, "Pipkin %s is up to date.\n", deps.buildVersion); err != nil {
			return err
		}
		_ = deps.advisory(out)
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
	if _, err := deps.activate(staged, actions); err != nil {
		return commandPhaseError{message: "Couldn't finish updating Pipkin.", cause: err}
	}
	if _, err := fmt.Fprintf(out, "Pipkin %s is updated and running.\n", strings.TrimPrefix(tag, "v")); err != nil {
		return err
	}
	if _, err := io.Copy(out, &authorization); err != nil {
		return err
	}
	// Firmware advice is optional and never fails an update.
	_ = deps.advisory(out)
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

func installPersistentPathFix(goos, shell, home, zDotDir, launcher string, onPATH bool) string {
	if goos == "windows" || onPATH || !filepath.IsAbs(home) {
		return ""
	}
	var startup string
	switch filepath.Base(shell) {
	case "zsh":
		folder := home
		if zDotDir != "" {
			if !filepath.IsAbs(zDotDir) {
				return ""
			}
			folder = zDotDir
		}
		startup = filepath.Join(folder, ".zshrc")
	case "bash":
		startup = filepath.Join(home, ".bashrc")
		if goos == "darwin" {
			startup = bashLoginStartupFile(home)
			if startup == "" {
				return ""
			}
		}
	default:
		return ""
	}
	line := installPathFix(goos, launcher, false)
	return `printf '\n%s\n' ` + shellQuote(line) + " >> " + shellQuote(startup)
}

func bashLoginStartupFile(home string) string {
	// Bash reads only the first login file it finds, so never shadow one.
	for _, name := range []string{".bash_profile", ".bash_login", ".profile"} {
		path := filepath.Join(home, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return ""
		}
		if info.Mode()&os.ModeSymlink != 0 {
			info, err = os.Stat(path)
			if err != nil {
				return ""
			}
		}
		if !info.Mode().IsRegular() {
			return ""
		}
		file, err := os.Open(path)
		if err != nil {
			return ""
		}
		if err := file.Close(); err != nil {
			return ""
		}
		return path
	}
	return filepath.Join(home, ".bash_profile")
}

// authorizeNoticeWriter passes only the permission notice through from the child.
type authorizeNoticeWriter struct {
	out       io.Writer
	notice    []byte
	prefix    []int
	matched   int
	forwarded bool
}

func newAuthorizeNoticeWriter(out io.Writer) *authorizeNoticeWriter {
	var rendered bytes.Buffer
	_ = writeOutputLine(&rendered, "", authorizePromptNotice)
	notice := rendered.Bytes()
	prefix := make([]int, len(notice))
	for i, j := 1, 0; i < len(notice); i++ {
		for j > 0 && notice[i] != notice[j] {
			j = prefix[j-1]
		}
		if notice[i] == notice[j] {
			j++
		}
		prefix[i] = j
	}
	return &authorizeNoticeWriter{out: out, notice: notice, prefix: prefix}
}

func (w *authorizeNoticeWriter) Write(data []byte) (int, error) {
	if w.forwarded {
		return len(data), nil
	}
	for i, b := range data {
		for w.matched > 0 && b != w.notice[w.matched] {
			w.matched = w.prefix[w.matched-1]
		}
		if b == w.notice[w.matched] {
			w.matched++
		}
		if w.matched == len(w.notice) {
			w.forwarded = true
			n, err := w.out.Write(w.notice)
			if err == nil && n != len(w.notice) {
				err = io.ErrShortWrite
			}
			if err != nil {
				return i + 1, err
			}
			break
		}
	}
	return len(data), nil
}

func latestFirmwareAdvisoryTag(ctx context.Context) (string, error) {
	source := publicFirmwareSource()
	data, err := source.get(ctx, source.api+"/releases/latest", 1<<20)
	if err != nil {
		return "", err
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if json.Unmarshal(data, &release) != nil || release.Draft || release.Prerelease ||
		!strings.HasPrefix(release.Tag, "v") || !stableFirmwareVersion.MatchString(release.Tag) {
		return "", errors.New("no stable firmware release found")
	}
	return release.Tag, nil
}

func writeFirmwareAdvisory(out io.Writer, saved func() (string, error), latest func(context.Context) (string, error), hint func(...string) string) error {
	current, err := saved()
	if err != nil || !stableFirmwareVersion.MatchString(current) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tag, err := latest(ctx)
	if err != nil || !stableFirmwareVersion.MatchString(tag) || !newerVersion(tag, current) {
		return nil
	}
	_, err = fmt.Fprintf(out, "Display firmware %s is available (yours is %s). To install it, run: %s\n", strings.TrimPrefix(tag, "v"), strings.TrimPrefix(current, "v"), hint("flash"))
	return err
}

func cachedDisplayFirmware() (string, error) {
	data, err := readFileBounded(statusPath(), 1<<20)
	if err != nil {
		return "", err
	}
	var status struct {
		Device struct {
			Firmware string `json:"firmware"`
		} `json:"device"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return "", err
	}
	return status.Device.Firmware, nil
}
