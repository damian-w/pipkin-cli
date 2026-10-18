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
)

type flowTestLock struct{ closed bool }

func TestCommandPhaseErrorKeepsCauseAndSeparatesSafeDetails(t *testing.T) {
	want := errors.New("synthetic service failure\x1b[31m\n" + strings.Repeat("detail ", 30))
	err := commandPhaseError{message: "Pipkin couldn't start. Check the details below.", cause: want}
	if !errors.Is(err, want) || !strings.HasPrefix(err.Error(), "Pipkin couldn't start.") || !strings.Contains(err.Error(), "\nDetails: ") {
		t.Fatal("phase error lost its summary, technical details or cause")
	}
	for _, line := range strings.Split(err.Error(), "\n") {
		if len(line) > outputWidth || strings.ContainsAny(line, "\x1b\r\t") {
			t.Fatal("phase error printed unsafe or unbounded terminal details")
		}
	}
}

func (lock *flowTestLock) Close() error { lock.closed = true; return nil }

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
			deps.activate = func(_ *stagedBinaries, actions installationActions) (string, error) {
				events = append(events, "activate")
				actions.authorize()
				if err := actions.configure(); err != nil {
					t.Fatal(err)
				}
				return "synthetic startup", nil
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
		activate: func(*stagedBinaries, installationActions) (string, error) {
			t.Fatal("invalid config activated binaries")
			return "", nil
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
				activate: func(_ *stagedBinaries, actions installationActions) (string, error) {
					events = append(events, "activate")
					actions.authorize()
					if strings.Contains(output.String(), "Synthetic authorization") {
						t.Fatal("authorization warning escaped before success")
					}
					return "synthetic", nil
				},
				advisory: func(io.Writer) error {
					events = append(events, "advisory")
					return errors.New("optional advice unavailable")
				},
			}
			if err := runUpdateCommand(&output, deps); err != nil {
				t.Fatalf("advisory failure changed successful update: %v", err)
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
		err := writeFirmwareAdvisory(&output, func() (string, error) { return current, nil }, func(ctx context.Context) (string, error) {
			calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("firmware check has no deadline")
			}
			return "v1.1.0", nil
		}, func(args ...string) string { return commandHintFor("linux", "/synthetic/pipkin", false, args...) })
		if err != nil || calls != map[bool]int{true: 1, false: 0}[current == "1.0.0"] {
			t.Fatalf("invalid saved firmware initiated advice: %v", err)
		}
		if current == "1.0.0" && (!strings.Contains(output.String(), "Display firmware 1.1.0 is available (yours is 1.0.0)") || !strings.Contains(output.String(), "'/synthetic/pipkin' flash")) {
			t.Fatalf("firmware advice invented a live state or unusable command: %s", output.String())
		}
	}
	if err := writeFirmwareAdvisory(io.Discard, func() (string, error) { return "1.0.0", nil }, func(context.Context) (string, error) { return "", errors.New("offline") }, nil); err != nil {
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

func TestInstalledAuthorizationCapturesAndDiscardsBoundedChildOutput(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	for _, failed := range []bool{false, true} {
		var output bytes.Buffer
		authorizeInstalledHelperFor("darwin", func() bool { return true }, func(cmd *exec.Cmd) error {
			if cmd.Path != installedBinary() || !slices.Contains(cmd.Env, "PIPKIN_AUTHORIZE_NESTED=1") {
				t.Fatal("authorization did not use the nested installed replacement")
			}
			for _, writer := range []io.Writer{cmd.Stdout, cmd.Stderr} {
				if _, err := writer.Write(bytes.Repeat([]byte("synthetic-private-output"), 1024)); err != nil {
					t.Fatal(err)
				}
			}
			if cmd.Stderr != io.Discard {
				t.Fatal("child stderr was not discarded")
			}
			if _, ok := cmd.Stdout.(*authorizeNoticeWriter); !ok {
				t.Fatal("child stdout was not restricted to the permission notice")
			}
			if failed {
				return errors.New("synthetic failure")
			}
			return nil
		}, &output)
		if strings.Contains(output.String(), "synthetic-private") || strings.Contains(output.String(), "authorize") != failed {
			t.Fatal("raw child output leaked or authorization failure was lost")
		}
	}
}

func TestInstalledAuthorizationForwardsOnlyStaticNoticeBeforeReturning(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	var live, warnings, notice bytes.Buffer
	if err := writeOutputLine(&notice, "", authorizePromptNotice); err != nil {
		t.Fatal(err)
	}
	authorizeInstalledHelperWithNoticeFor("darwin", func() bool { return true }, func(cmd *exec.Cmd) error {
		// Exercise arbitrary chunk boundaries and an overlapping prefix after
		// private output. Only the known safe notice can appear immediately.
		data := append([]byte("private-canary macO"), notice.Bytes()...)
		for _, b := range data {
			if _, err := cmd.Stdout.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
		if live.String() != notice.String() || warnings.Len() != 0 {
			t.Fatal("permission notice was delayed or private child output escaped")
		}
		_, _ = cmd.Stdout.Write(notice.Bytes())
		_, _ = io.WriteString(cmd.Stdout, "Claude's saved sign-in is ready.\nprivate-canary")
		_, _ = io.WriteString(cmd.Stderr, "private-canary")
		return errors.New("private-canary")
	}, &live, &warnings)
	if live.String() != notice.String() || !strings.Contains(warnings.String(), "authorize") || strings.Contains(live.String()+warnings.String(), "private-canary") {
		t.Fatal("authorization duplicated its notice, leaked private output or lost recovery")
	}
}

func TestPersistentPATHHintsAreOptionalAndDoNotWriteStartupFiles(t *testing.T) {
	home := t.TempDir()
	launcher := filepath.Join(home, "O'Brien", "bin", "pipkin")
	for _, tc := range []struct{ goos, shell, zDotDir, suffix string }{
		{"darwin", "/bin/zsh", "", ".zshrc"},
		{"darwin", "/bin/bash", "", ".bash_profile"},
		{"linux", "/bin/bash", "", ".bashrc"},
		{"darwin", "/bin/zsh", filepath.Join(home, "custom"), "custom/.zshrc"},
	} {
		got := installPersistentPathFix(tc.goos, tc.shell, home, tc.zDotDir, launcher, false)
		startup := filepath.Join(home, tc.suffix)
		want := `printf '\n%s\n' ` + shellQuote(installPathFix(tc.goos, launcher, false)) + " >> " + shellQuote(startup)
		if got != want {
			t.Fatalf("startup hint is not exact or safely quoted: %q", got)
		}
		if _, err := os.Stat(startup); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("printing a hint touched the startup file")
		}
	}
	for _, tc := range []struct {
		goos, shell, zDotDir string
		onPATH               bool
	}{
		{"darwin", "/bin/fish", "", false},
		{"darwin", "", "", false},
		{"darwin", "/bin/zsh", "relative", false},
		{"windows", "/bin/bash", "", false},
		{"darwin", "/bin/zsh", "", true},
	} {
		if got := installPersistentPathFix(tc.goos, tc.shell, home, tc.zDotDir, launcher, tc.onPATH); got != "" {
			t.Fatalf("unknown or unneeded shell received a startup change: %q", got)
		}
	}
}

func TestPersistentPATHHintPreservesBashLoginSettings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX login shell")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	for _, tc := range []struct {
		name, active string
		files        []string
	}{
		{"profile", ".profile", []string{".profile"}},
		{"bash login", ".bash_login", []string{".bash_login", ".profile"}},
		{"bash profile", ".bash_profile", []string{".bash_profile", ".bash_login", ".profile"}},
		{"no login file", ".bash_profile", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			launcher := filepath.Join(home, "O'Brien", "é", "bin", "pipkin")
			for _, name := range tc.files {
				if err := os.WriteFile(filepath.Join(home, name), []byte("export PIPKIN_TEST_LOGIN="+name+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Only these subprocesses receive the temporary HOME. Go's own home
			// and build caches remain unchanged, and no user profile is sourced.
			run := func(login bool, script string) string {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				args := []string{"--noprofile", "--norc", "-c", script}
				if login {
					args = []string{"--login", "-c", script}
				}
				command := exec.CommandContext(ctx, "/bin/bash", args...)
				command.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "BASH_ENV=/dev/null", "ENV=/dev/null", "LC_ALL=C"}
				data, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("temporary-home shell failed: %v: %s", err, data)
				}
				return string(data)
			}
			sentinel := `printf '%s' "${PIPKIN_TEST_LOGIN:-missing}"`
			before := run(true, sentinel)
			want := tc.active
			if len(tc.files) == 0 {
				want = "missing"
			}
			if before != want {
				t.Fatalf("login fixture loaded %q, want %q", before, want)
			}
			hint := installPersistentPathFix("darwin", "/bin/bash", home, "", launcher, false)
			if hint == "" || !strings.HasSuffix(hint, " >> "+shellQuote(filepath.Join(home, tc.active))) {
				t.Fatalf("hint did not select the active login file: %q", hint)
			}
			run(false, hint)
			if after := run(true, sentinel); after != before {
				t.Fatalf("persistent hint hid existing settings: before=%q after=%q", before, after)
			}
			if path := run(true, `printf '%s' "$PATH"`); !strings.HasPrefix(path, filepath.Dir(launcher)+":") {
				t.Fatalf("login shell did not load the PATH addition: %q", path)
			}
			for _, name := range tc.files {
				if name == tc.active {
					continue
				}
				if data, err := os.ReadFile(filepath.Join(home, name)); err != nil || string(data) != "export PIPKIN_TEST_LOGIN="+name+"\n" {
					t.Fatal("hint changed an inactive login file")
				}
			}
			for _, name := range []string{".bash_profile", ".bash_login", ".profile"} {
				if name == tc.active {
					break
				}
				if _, err := os.Lstat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("hint created a higher-priority login file")
				}
			}
		})
	}
}

func TestPersistentPATHHintWithholdsUnverifiableBashLoginFile(t *testing.T) {
	t.Run("non-file", func(t *testing.T) {
		home := t.TempDir()
		if err := os.Mkdir(filepath.Join(home, ".bash_profile"), 0o700); err != nil {
			t.Fatal(err)
		}
		if hint := installPersistentPathFix("darwin", "/bin/bash", home, "", filepath.Join(home, "pipkin"), false); hint != "" {
			t.Fatalf("non-file login configuration received an append command: %q", hint)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, ".bash_profile")
		if err := os.WriteFile(path, []byte("private fixture"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if file, err := os.Open(path); err == nil {
			file.Close()
			t.Skip("runner can read files without read permission")
		}
		if hint := installPersistentPathFix("darwin", "/bin/bash", home, "", filepath.Join(home, "pipkin"), false); hint != "" {
			t.Fatalf("unreadable login configuration received an append command: %q", hint)
		}
	})
	t.Run("dangling symlink", func(t *testing.T) {
		home := t.TempDir()
		if err := os.Symlink(filepath.Join(home, "missing"), filepath.Join(home, ".bash_profile")); err != nil {
			t.Skipf("symlink fixture is unavailable: %v", err)
		}
		if hint := installPersistentPathFix("darwin", "/bin/bash", home, "", filepath.Join(home, "pipkin"), false); hint != "" {
			t.Fatalf("dangling login symlink received an append command: %q", hint)
		}
	})
}

func TestCommandHintsEncodeControlCharactersWithoutChangingThePath(t *testing.T) {
	value := "/synthetic/a'\\\n\x1b[31m/pipkin"
	got := commandHintFor("darwin", value, false, "status")
	if strings.ContainsAny(got, "\n\r\x1b") || !strings.Contains(got, `\012`) || !strings.Contains(got, `\033`) {
		t.Fatalf("shell hint contains terminal controls or lost the exact path: %q", got)
	}
	// Parse only the quoted value and print it. No suggested command runs and
	// no startup file is opened; this verifies the shell receives the exact path.
	if bash, err := exec.LookPath("bash"); err == nil && runtime.GOOS != "windows" {
		cmd := exec.Command(bash, "-c", "printf '%s' "+shellQuote(value))
		if output, err := cmd.Output(); err != nil || string(output) != value {
			t.Fatalf("shell-safe hint did not round trip its path: %q, %v", output, err)
		}
	}
	got = commandHintFor("windows", "C:\\synthetic\\a\x1b\n\\pipkin.exe", false, "status")
	if strings.ContainsAny(got, "\n\r\x1b") || !strings.Contains(got, "[char]::ConvertFromUtf32(27)") || !strings.Contains(got, "[char]::ConvertFromUtf32(10)") {
		t.Fatalf("PowerShell hint contains raw terminal controls: %q", got)
	}
}
