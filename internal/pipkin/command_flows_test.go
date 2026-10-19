package pipkin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
)

type flowTestLock struct{ closed bool }

func (lock *flowTestLock) Close() error { lock.closed = true; return nil }

func TestCommandPhaseErrorKeepsCauseAndSeparatesSafeDetails(t *testing.T) {
	want := errors.New("synthetic service failure\x1b[31m\n" + strings.Repeat("detail ", 30))
	err := commandPhaseError{message: "Pipkin couldn't start. Check the details below.", cause: want}
	if !errors.Is(err, want) || !strings.HasPrefix(err.Error(), "Pipkin couldn't start.") || !strings.Contains(err.Error(), "\nDetails: ") {
		t.Fatal("phase error lost its summary, technical details or cause")
	}
	checkTerminalLines(t, err.Error(), true)
}

func TestInstallFlowSummarizesAfterActivationAndUsesExactPATHHint(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprint(denied), func(t *testing.T) {
			var output bytes.Buffer
			var events []string
			lock := &flowTestLock{}
			stage := &stagedBinaries{folder: filepath.Join(t.TempDir(), "stage")}
			if err := os.Mkdir(stage.folder, 0700); err != nil {
				t.Fatal(err)
			}
			launcher := "/Users/O'Brien test/.local/bin/pipkin"
			deps := installCommandDeps{
				buildVersion: "9.8.7", goos: "darwin",
				lock:            func() (io.Closer, error) { events = append(events, "lock"); return lock, nil },
				stage:           func() (*stagedBinaries, error) { events = append(events, "stage"); return stage, nil },
				launcherMissing: func() (bool, error) { return true, nil },
				config: func() (Config, string, error) {
					events = append(events, "config")
					return Config{}, "/synthetic/config.bad", nil
				},
				configure: func() error { events = append(events, "configure"); return nil },
				undo:      func() error { t.Fatal("successful flow undid the launcher"); return nil },
				authorize: func(_ io.Writer, out io.Writer) {
					events = append(events, "authorize")
					if output.Len() != 0 {
						t.Fatal("install success was reported before activation")
					}
					if denied {
						fmt.Fprintln(out, "Pipkin can't read Claude's sign-in yet. Run the authorize command.")
					}
				},
				hint:    func(args ...string) string { return commandHintFor("darwin", launcher, false, args...) },
				pathFix: installPathFix("darwin", launcher, false), permissionHint: func() string { return "" },
			}
			deps.activate = func(_ *stagedBinaries, actions installationActions) error {
				events = append(events, "activate")
				actions.authorize()
				if err := actions.configure(); err != nil {
					t.Fatal(err)
				}
				return nil
			}
			if err := runInstallCommand(&output, deps); err != nil {
				t.Fatal(err)
			}
			if !lock.closed || !slices.Equal(events, []string{"lock", "stage", "config", "activate", "authorize", "configure"}) {
				t.Fatalf("install flow changed its order or leaked its lock: %v", events)
			}
			if _, err := os.Stat(stage.folder); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("staging was not cleaned")
			}
			text := output.String()
			if !strings.HasPrefix(text, "Pipkin 9.8.7 is installed and running.") || !strings.Contains(text, "when you log in") || !strings.Contains(text, "The old file is at /synthetic/config.bad") {
				t.Fatalf("install omitted its summary or config backup: %s", text)
			}
			if !strings.Contains(text, "Next: "+deps.hint("status")) || !strings.Contains(text, deps.pathFix) {
				t.Fatalf("install did not provide shell-safe usable commands: %s", text)
			}
			if strings.Contains(text, ".zshrc") || strings.Contains(text, ".bashrc") || strings.Contains(text, "detected automatically") {
				t.Fatal("install edits shell startup or claims access that was not verified")
			}
			if strings.Contains(text, "sign-in yet") != denied {
				t.Fatal("authorization result was lost or invented")
			}
		})
	}
}

func TestInstallFlowConfigFailurePreservesBackupAndDoesNotActivate(t *testing.T) {
	lock := &flowTestLock{}
	want := errors.New("synthetic settings error")
	deps := installCommandDeps{
		lock:            func() (io.Closer, error) { return lock, nil },
		stage:           func() (*stagedBinaries, error) { return &stagedBinaries{folder: t.TempDir()}, nil },
		launcherMissing: func() (bool, error) { return false, nil },
		config:          func() (Config, string, error) { return Config{}, "/synthetic/backup", want },
		activate: func(*stagedBinaries, installationActions) error {
			t.Fatal("invalid config activated binaries")
			return nil
		},
	}
	var output bytes.Buffer
	err := runInstallCommand(&output, deps)
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "/synthetic/backup") || output.Len() != 0 || !lock.closed {
		t.Fatalf("config repair failure lost its cause, backup or lock: %v", err)
	}
}

func TestUpdateFlowAdvisesInBothSuccessBranchesWithoutChangingActivation(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(fmt.Sprint(newer), func(t *testing.T) {
			var output bytes.Buffer
			var events []string
			lock := &flowTestLock{}
			deps := updateCommandDeps{
				buildVersion: "1.0.0",
				latest: func() (string, error) {
					events = append(events, "latest")
					if !strings.Contains(output.String(), "Checking") {
						t.Fatal("update did not announce its check")
					}
					if newer {
						return "v1.1.0", nil
					}
					return "v1.0.0", nil
				},
				lock: func() (io.Closer, error) { events = append(events, "lock"); return lock, nil },
				stage: func(tag string) (*stagedBinaries, error) {
					events = append(events, "stage")
					if tag != "v1.1.0" || !strings.Contains(output.String(), "Downloading Pipkin 1.1.0") {
						t.Fatal("download announcement or selected tag changed")
					}
					return &stagedBinaries{folder: t.TempDir()}, nil
				},
				config: func() (Config, error) { events = append(events, "config"); return Config{}, nil },
				authorize: func(_ io.Writer, out io.Writer) {
					events = append(events, "authorize")
					fmt.Fprintln(out, "Synthetic authorization warning")
				},
				activate: func(_ *stagedBinaries, actions installationActions) error {
					events = append(events, "activate")
					actions.authorize()
					if strings.Contains(output.String(), "Synthetic authorization") {
						t.Fatal("authorization warning escaped before success")
					}
					return nil
				},
				advisory: func(io.Writer) { events = append(events, "advisory") },
			}
			if err := runUpdateCommand(&output, deps); err != nil {
				t.Fatalf("successful update failed: %v", err)
			}
			want := []string{"latest", "advisory"}
			if newer {
				want = []string{"latest", "lock", "stage", "config", "activate", "authorize", "advisory"}
			}
			if !slices.Equal(events, want) || newer && !lock.closed {
				t.Fatalf("update changed activation order or omitted advice: %v", events)
			}
			if newer && !strings.Contains(output.String(), "Pipkin 1.1.0 is updated and running.") || !newer && !strings.Contains(output.String(), "up to date") {
				t.Fatalf("update completion is missing: %s", output.String())
			}
		})
	}
}

func TestFirmwareAdviceUsesOnlySavedVersionAndBoundedOptionalLookup(t *testing.T) {
	for _, current := range []string{"", "invalid", "1.0.0"} {
		var output bytes.Buffer
		calls := 0
		writeFirmwareAdvisory(&output, func() (string, error) { return current, nil }, func(ctx context.Context) (string, error) {
			calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("firmware check has no deadline")
			}
			return "v1.1.0", nil
		}, func(args ...string) string { return commandHintFor("linux", "/synthetic/pipkin", false, args...) })
		if calls != map[bool]int{true: 1, false: 0}[current == "1.0.0"] {
			t.Fatalf("invalid saved firmware initiated advice: %d lookups", calls)
		}
		if current == "1.0.0" && (!strings.Contains(output.String(), "Display firmware 1.1.0 is available (yours is 1.0.0)") || !strings.Contains(output.String(), "'/synthetic/pipkin' flash")) {
			t.Fatalf("firmware advice invented a live state or unusable command: %s", output.String())
		}
	}
	var offline bytes.Buffer
	writeFirmwareAdvisory(&offline, func() (string, error) { return "1.0.0", nil }, func(context.Context) (string, error) { return "", errors.New("offline") }, nil)
	if offline.Len() != 0 {
		t.Fatal("optional network failure escaped the firmware advice")
	}
}

func TestCommandHintsAndLockFailuresRemainSafeAndClassified(t *testing.T) {
	if got := commandHintFor("windows", `C:\Users\O'Brien\Pipkin\pipkin.exe`, false, "status"); got != `& 'C:\Users\O''Brien\Pipkin\pipkin.exe' status` {
		t.Fatalf("PowerShell command is not quoted safely: %s", got)
	}
	if got := commandHintFor("darwin", "/unused", true, "status"); got != "pipkin status" {
		t.Fatal("available command gained unnecessary path syntax")
	}
	for _, cause := range []error{errLockBusy, os.ErrPermission, errors.New("unexpected lock error")} {
		err := commandLockError("start", cause)
		if !errors.Is(err, cause) || strings.Contains(err.Error(), "Another") != errors.Is(cause, errLockBusy) {
			t.Fatalf("lock failure lost classification or falsely claimed busy: %v", err)
		}
	}
	var output bytes.Buffer
	want := &url.Error{Op: "HEAD", URL: "https://github.com/synthetic", Err: errors.New("offline")}
	err := runUpdateCommand(&output, updateCommandDeps{latest: func() (string, error) { return "", want }})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "Check your internet connection") {
		t.Fatalf("update connectivity failure is not actionable: %v", err)
	}
}

func TestInstalledAuthorizationPrintsNoticeAndDiscardsChildOutput(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	var notice bytes.Buffer
	if err := writeOutputLine(&notice, "", authorizePromptNotice); err != nil {
		t.Fatal(err)
	}
	for _, failed := range []bool{false, true} {
		var live, warnings bytes.Buffer
		authorizeInstalledHelperWithNoticeFor("darwin", func() bool { return true }, func(cmd *exec.Cmd) error {
			if cmd.Path != installedBinary() || !slices.Equal(cmd.Args, []string{installedBinary(), "authorize"}) ||
				!slices.Contains(cmd.Env, "PIPKIN_AUTHORIZE_NESTED=1") {
				t.Fatalf("authorization did not use the nested installed replacement: path=%q args=%q", cmd.Path, cmd.Args)
			}
			if cmd.Stdin != os.Stdin || cmd.Stdout != io.Discard || cmd.Stderr != io.Discard {
				t.Fatal("child output was not discarded while retaining foreground input")
			}
			if live.String() != notice.String() {
				t.Fatal("permission notice was not shown before authorization")
			}
			if failed {
				return errors.New("synthetic failure")
			}
			return nil
		}, &live, &warnings)
		if live.String() != notice.String() || strings.Contains(warnings.String(), "authorize") != failed {
			t.Fatal("authorization duplicated its notice or lost recovery")
		}
	}
}

func TestCommandHintsEncodeControlCharactersWithoutChangingThePath(t *testing.T) {
	value := "/synthetic/a'\\\n\x1b[31m/pipkin"
	got := commandHintFor("darwin", value, false, "status")
	if strings.ContainsAny(got, "\n\r\x1b") || !strings.Contains(got, `\012`) || !strings.Contains(got, `\033`) {
		t.Fatalf("shell hint contains terminal controls or lost the exact path: %q", got)
	}
	got = commandHintFor("windows", "C:\\synthetic\\a\x1b\n\\pipkin.exe", false, "status")
	if strings.ContainsAny(got, "\n\r\x1b") || !strings.Contains(got, "[char]::ConvertFromUtf32(27)") || !strings.Contains(got, "[char]::ConvertFromUtf32(10)") {
		t.Fatalf("PowerShell hint contains raw terminal controls: %q", got)
	}
}

func TestCommandHintArgumentsPreserveExactPaths(t *testing.T) {
	values := []string{"/tmp/serial-é", "/tmp/串口 'quoted' $(literal);back\\slash", "/tmp/serial-\n\r\t\x1b", "/synthetic/a'\\\n\x1b[31m/pipkin"}
	for _, value := range values {
		for _, goos := range []string{"darwin", "linux", "windows"} {
			command := flashRecoveryHint(goos, value)
			if strings.IndexFunc(command, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
				t.Fatalf("command hint contains terminal controls or multiple physical lines: %q", command)
			}
		}
		if runtime.GOOS == "windows" {
			continue
		}
		// Parse only the quoted value and print it. No suggested command runs and
		// no startup file is opened; this verifies the shell receives the exact path.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		parse := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-c", "printf '%s' "+shellQuote(value))
		parse.Env = []string{"PATH=/usr/bin:/bin", "BASH_ENV=/dev/null", "ENV=/dev/null"}
		data, err := parse.Output()
		cancel()
		if err != nil || string(data) != value {
			t.Fatalf("argument changed its POSIX path: got=%q want=%q err=%v", data, value, err)
		}
	}
}

func TestCommandHintPowerShellArgumentsPreserveExactPaths(t *testing.T) {
	name := "pwsh"
	if runtime.GOOS == "windows" {
		name = "powershell.exe"
	}
	powerShell, err := exec.LookPath(name)
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	values := []string{"C:\\serial-é", "C:\\串口 'quoted' $(literal);back\\slash", "C:\\serial-\n\r\t\x1b"}
	var script strings.Builder
	script.WriteString("[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false);")
	for _, value := range values {
		script.WriteString("[Console]::Write(" + powerShellQuote(value) + ");[Console]::Write([char]0);")
	}
	// PowerShell can take over 15 seconds to start cold on CI runners.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script.String())
	data, err := command.Output()
	if err != nil {
		t.Fatalf("PowerShell argument parser failed: %v", err)
	}
	parts := bytes.Split(data, []byte{0})
	if len(parts) != len(values)+1 || len(parts[len(parts)-1]) != 0 {
		t.Fatalf("PowerShell changed argument boundaries: %q", data)
	}
	for i, value := range values {
		if string(parts[i]) != value {
			t.Fatalf("argument changed its PowerShell path: got=%q want=%q", parts[i], value)
		}
	}
}
