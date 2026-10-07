package pipkin

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestUpdateActivationAuthorizesInstalledReplacementBeforeStart(t *testing.T) {
	for _, denied := range []bool{false, true} {
		name := "authorized"
		if denied {
			name = "authorization canceled"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("PIPKIN_HOME", t.TempDir())
			source := filepath.Join(t.TempDir(), executableName("pipkin"))
			target := filepath.Dir(installedBinary())
			for _, artifact := range binaryManifest(runtime.GOOS, runtime.GOARCH) {
				writeTestBinary(t, filepath.Join(filepath.Dir(source), artifact.name), "new "+artifact.name)
				writeTestBinary(t, filepath.Join(target, artifact.name), "old "+artifact.name)
			}
			staged, err := stageLocalBinaries(source, target, runtime.GOOS, runtime.GOARCH)
			if err != nil {
				t.Fatal(err)
			}
			defer staged.close()
			var output bytes.Buffer
			var events []string
			mainName := executableName("pipkin")
			actions := installationActions{
				running:  func() (int, error) { return 42, nil },
				snapshot: noAutostartSnapshot,
				stop:     func() error { events = append(events, "stop"); return nil },
				register: func() (string, error) {
					events = append(events, "register")
					return "test startup", nil
				},
				authorize: func() {
					authorizeInstalledHelperFor("darwin", func() bool { return true }, func(command *exec.Cmd) error {
						events = append(events, "authorize")
						if command.Path != installedBinary() || command.Path == source || !slices.Equal(command.Args, []string{installedBinary(), "authorize"}) {
							t.Fatalf("authorization did not use the installed replacement: path=%q args=%q", command.Path, command.Args)
						}
						if command.Stdin != os.Stdin || command.Stdout != &output || command.Stderr != os.Stderr {
							t.Fatal("authorization did not retain the foreground command's streams")
						}
						checkTestBinary(t, command.Path, "new "+mainName)
						checkTestBinary(t, command.Path+".old", "old "+mainName)
						if denied {
							return errors.New("Keychain authorization canceled")
						}
						return nil
					}, &output)
				},
				configure: func() error { events = append(events, "configure"); return nil },
				start: func() error {
					events = append(events, "start")
					checkTestBinary(t, installedBinary(), "new "+mainName)
					return nil
				},
			}
			mechanism, err := activateBinaries(staged, actions)
			if err != nil || mechanism != "test startup" {
				t.Fatalf("activation = %q, %v", mechanism, err)
			}
			if !slices.Equal(events, []string{"stop", "register", "authorize", "configure", "start"}) {
				t.Fatalf("activation order = %v", events)
			}
			if denied {
				if !strings.Contains(output.String(), "pipkin authorize") || !strings.Contains(output.String(), "pipkin restart") {
					t.Fatalf("authorization cancellation omitted actionable recovery: %q", output.String())
				}
			} else if output.Len() != 0 {
				t.Fatalf("successful authorization reported a warning: %q", output.String())
			}
			for _, artifact := range staged.artifacts {
				checkTestBinary(t, filepath.Join(target, artifact.name), "new "+artifact.name)
				if _, err := os.Stat(filepath.Join(target, artifact.name) + ".old"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("successful activation retained an unused backup: %v", err)
				}
			}
		})
	}
}

func TestInstalledAuthorizationSkipsOtherPlatformsAndAbsentClaude(t *testing.T) {
	for _, goos := range []string{"linux", "windows", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			var output bytes.Buffer
			lookups, commands := 0, 0
			authorizeInstalledHelperFor(goos, func() bool {
				lookups++
				return false
			}, func(*exec.Cmd) error {
				commands++
				return errors.New("must not execute")
			}, &output)
			wantedLookups := 0
			if goos == "darwin" {
				wantedLookups = 1
			}
			if lookups != wantedLookups || commands != 0 || output.Len() != 0 {
				t.Fatalf("authorization on %s: lookups=%d commands=%d output=%q", goos, lookups, commands, output.String())
			}
		})
	}
}

func TestHelperInstallationActionsIncludesAuthorization(t *testing.T) {
	if helperInstallationActions().authorize == nil {
		t.Fatal("install and update no longer authorize the installed replacement")
	}
}

func TestRestartHelperOrdersStopBeforeStartAndPreservesErrors(t *testing.T) {
	stopError, startError := errors.New("stop failed"), errors.New("start failed")
	for _, test := range []struct {
		name              string
		stopErr, startErr error
		want              []string
		wantErr           error
	}{
		{"success", nil, nil, []string{"stop", "start"}, nil},
		{"stop failure", stopError, nil, []string{"stop"}, stopError},
		{"start failure", nil, startError, []string{"stop", "start"}, startError},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			err := restartHelper(func() error {
				events = append(events, "stop")
				return test.stopErr
			}, func() error {
				events = append(events, "start")
				return test.startErr
			})
			if !slices.Equal(events, test.want) || !errors.Is(err, test.wantErr) {
				t.Fatalf("restart = %v, %v; want %v, %v", events, err, test.want, test.wantErr)
			}
			if test.startErr != nil && !strings.Contains(err.Error(), "pipkin start") {
				t.Fatalf("start failure omitted recovery: %v", err)
			}
		})
	}
}

func TestRestartCommandRejectsBusyLocksAndMissingInstallation(t *testing.T) {
	for _, scenario := range []string{"installation lock", "maintenance lock", "missing installation"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("PIPKIN_HOME", t.TempDir())
			// An unpublished helper lock makes any unexpected lifecycle entrance
			// fail before a service-manager command or process launch is reachable.
			guard, err := lockInstance()
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			if scenario != "missing installation" {
				writeTestBinary(t, installedBinary(), "test installation")
			}
			if scenario != "missing installation" {
				path := installationLockPath()
				if scenario == "maintenance lock" {
					path = maintenanceLockPath()
				}
				lock, err := acquireFileLock(path)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			err = restartCommand()
			if scenario == "missing installation" {
				if err == nil || !strings.Contains(err.Error(), "Pipkin is not installed") {
					t.Fatalf("missing installation = %v", err)
				}
			} else if !errors.Is(err, errLockBusy) {
				t.Fatalf("busy %s = %v", scenario, err)
			}
			if scenario != "installation lock" {
				lock, err := acquireFileLock(installationLockPath())
				if err != nil {
					t.Fatalf("restart failure leaked the installation lock: %v", err)
				}
				lock.Close()
			}
		})
	}
}
