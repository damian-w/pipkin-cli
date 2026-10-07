package pipkin

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func noAutostartSnapshot() (func() error, error) {
	return func() error { return nil }, nil
}

func writeTestBinary(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
}

func checkTestBinary(t *testing.T, path, text string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != text {
		t.Fatalf("%s = %q, %v; want %q", filepath.Base(path), data, err, text)
	}
}

func TestDownloadRejectsOversizedBodies(t *testing.T) {
	for _, test := range []struct {
		name, data string
		chunked    bool
		wantError  bool
	}{
		{"exact limit", "abc", false, false},
		{"declared too large", "abcd", false, true},
		{"chunked too large", "abcd", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.chunked {
					w.(http.Flusher).Flush()
				}
				fmt.Fprint(w, test.data)
			}))
			defer server.Close()
			var data bytes.Buffer
			err := downloadTo(server.URL, &data, 3)
			if (err != nil) != test.wantError {
				t.Fatalf("download = %q, %v", data.String(), err)
			}
			if err == nil && data.String() != test.data {
				t.Fatalf("download silently truncated to %q", data.String())
			}
		})
	}
}

func TestChecksumManifestRejectsAmbiguity(t *testing.T) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("binary")))
	sums, err := parseChecksums([]byte(strings.ToUpper(digest) + " *pipkin\r\n"))
	if err != nil || sums["pipkin"] != digest {
		t.Fatalf("checksums = %v, %v", sums, err)
	}
	for _, data := range []string{"corrupt\n", "invalid pipkin\n", digest + " pipkin\n" + digest + " *pipkin\n"} {
		if _, err := parseChecksums([]byte(data)); err == nil {
			t.Fatalf("accepted ambiguous/invalid checksum manifest %q", data)
		}
	}
}

func TestReleasePreparationVerifiesWholeSet(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%v", corrupt), func(t *testing.T) {
			manifest := binaryManifest("windows", "arm64")
			data := []byte("new release executable")
			digest := fmt.Sprintf("%x", sha256.Sum256(data))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/SHA256SUMS" {
					for _, artifact := range manifest {
						fmt.Fprintf(w, "%s  %s\n", digest, artifact.asset)
					}
					return
				}
				if corrupt && r.URL.Path == "/"+manifest[1].asset {
					fmt.Fprint(w, "corrupt companion")
					return
				}
				w.Write(data)
			}))
			defer server.Close()
			target := t.TempDir()
			for _, artifact := range manifest {
				writeTestBinary(t, filepath.Join(target, artifact.name), "old "+artifact.name)
			}
			staged, err := stageReleaseBinaries(server.URL, target, "windows", "arm64")
			if corrupt {
				if err == nil {
					staged.close()
					t.Fatal("accepted a corrupt second executable")
				}
				entries, err := os.ReadDir(target)
				if err != nil || len(entries) != len(manifest) {
					t.Fatalf("failed preparation left staging files: %v, %v", entries, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer staged.close()
				for _, artifact := range staged.artifacts {
					checkTestBinary(t, artifact.staged, string(data))
				}
			}
			for _, artifact := range manifest {
				checkTestBinary(t, filepath.Join(target, artifact.name), "old "+artifact.name)
			}
		})
	}
}

func TestSourceInstallDropsStaleWindowsCompanion(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source", "pipkin.exe")
	target := filepath.Join(dir, "installed")
	writeTestBinary(t, source, "new console")
	writeTestBinary(t, filepath.Join(target, "pipkin.exe"), "old console")
	writeTestBinary(t, filepath.Join(target, "pipkinw.exe"), "old companion")
	staged, err := stageLocalBinaries(source, target, "windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.close()
	if !staged.missingCompanion {
		t.Fatal("did not report an absent source companion")
	}
	replacement, err := staged.replace()
	if err != nil {
		t.Fatal(err)
	}
	checkTestBinary(t, filepath.Join(target, "pipkin.exe"), "new console")
	if _, err := os.Stat(filepath.Join(target, "pipkinw.exe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old companion still selected: %v", err)
	}
	if err := replacement.rollback(); err != nil {
		t.Fatal(err)
	}
	checkTestBinary(t, filepath.Join(target, "pipkin.exe"), "old console")
	checkTestBinary(t, filepath.Join(target, "pipkinw.exe"), "old companion")
}

func TestReplacementFailureRestoresEntireBinarySet(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source", "pipkin.exe")
	target := filepath.Join(dir, "installed")
	for _, name := range []string{"pipkin.exe", "pipkinw.exe"} {
		writeTestBinary(t, filepath.Join(filepath.Dir(source), name), "new "+name)
		writeTestBinary(t, filepath.Join(target, name), "old "+name)
	}
	staged, err := stageLocalBinaries(source, target, "windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.close()
	// The first rename succeeds; the second fails after its old executable was backed up.
	if err := os.Remove(staged.artifacts[1].staged); err != nil {
		t.Fatal(err)
	}
	if _, err := staged.replace(); err == nil {
		t.Fatal("accepted a missing second staged executable")
	}
	for _, name := range []string{"pipkin.exe", "pipkinw.exe"} {
		checkTestBinary(t, filepath.Join(target, name), "old "+name)
	}
}

func TestActivationFailureRestoresServiceAndRunningHelper(t *testing.T) {
	for _, failure := range []string{"registration", "configuration", "startup"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source", "pipkin.exe")
			target := filepath.Join(dir, "installed")
			writeTestBinary(t, source, "new console")
			writeTestBinary(t, filepath.Join(target, "pipkin.exe"), "old console")
			writeTestBinary(t, filepath.Join(target, "pipkinw.exe"), "old companion")
			staged, err := stageLocalBinaries(source, target, "windows", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			defer staged.close()
			var events []string
			service := func() string {
				if _, err := os.Stat(filepath.Join(target, "pipkinw.exe")); err == nil {
					return "companion"
				}
				return "console"
			}
			actions := installationActions{
				running: func() (int, error) { return 123, nil },
				snapshot: func() (func() error, error) {
					previous := service()
					return func() error { events = append(events, "restore "+previous); return nil }, nil
				},
				stop: func() error { events = append(events, "stop"); return nil },
				register: func() (string, error) {
					events = append(events, "register "+service())
					if failure == "registration" && service() == "console" {
						return "", errors.New("registration failed")
					}
					return "test service", nil
				},
				configure: func() error {
					events = append(events, "configure")
					if failure == "configuration" {
						return errors.New("configuration failed")
					}
					return nil
				},
				start: func() error {
					events = append(events, "start "+service())
					if failure == "startup" && service() == "console" {
						return errors.New("startup failed")
					}
					checkTestBinary(t, filepath.Join(target, "pipkin.exe"), "old console")
					return nil
				},
			}
			if _, err := activateBinaries(staged, actions); err == nil {
				t.Fatal("activation unexpectedly succeeded")
			}
			checkTestBinary(t, filepath.Join(target, "pipkin.exe"), "old console")
			checkTestBinary(t, filepath.Join(target, "pipkinw.exe"), "old companion")
			if len(events) < 4 || !slices.Equal(events[len(events)-2:], []string{"restore companion", "start companion"}) {
				t.Fatalf("old service/helper not restored: %v", events)
			}
		})
	}
}

func TestFailedFreshInstallRemovesAutostart(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source", "pipkin")
	target := filepath.Join(dir, "installed")
	writeTestBinary(t, source, "new console")
	staged, err := stageLocalBinaries(source, target, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.close()
	unregistered := false
	actions := installationActions{
		running: func() (int, error) { return 0, nil },
		stop:    func() error { return nil },
		start:   func() error { return errors.New("startup failed") },
		register: func() (string, error) {
			return "test service", nil
		},
		snapshot: func() (func() error, error) {
			return func() error { unregistered = true; return nil }, nil
		},
	}
	if _, err := activateBinaries(staged, actions); err == nil {
		t.Fatal("fresh install unexpectedly succeeded")
	}
	if !unregistered {
		t.Fatal("failed fresh install left an autostart entry")
	}
	if _, err := os.Stat(filepath.Join(target, "pipkin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed fresh install left a binary: %v", err)
	}
}

// A child executes a copy from the installation directory, exercising Windows'
// running-executable rename rules as well as a source path equal to its target.
func TestInstalledExecutableCanBeStaged(t *testing.T) {
	if os.Getenv("PIPKIN_TEST_INSTALLED_EXECUTABLE") == "1" {
		source, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		staged, err := stageLocalBinaries(source, filepath.Dir(source), runtime.GOOS, runtime.GOARCH)
		if err != nil {
			t.Fatal(err)
		}
		defer staged.close()
		replacement, err := staged.replace()
		if err != nil {
			t.Fatal(err)
		}
		replacement.finish()
		after, err := os.Stat(source)
		if err != nil || after.Size() != before.Size() {
			t.Fatalf("same-directory replacement changed binary size: %v, %v", after, err)
		}
		return
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	path := filepath.Join(t.TempDir(), executableName("pipkin"))
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	copyErr := copyBounded(output, input, maxBinaryBytes)
	closeErr := output.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(path, "-test.run=^TestInstalledExecutableCanBeStaged$")
	child.Env = append(os.Environ(), "PIPKIN_TEST_INSTALLED_EXECUTABLE=1")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("installed executable could not replace itself: %v\n%s", err, output)
	}
}

func TestFreshLauncherIsRemovedOnStartupFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PIPKIN_HOME", filepath.Join(dir, "app"))
	source := filepath.Join(dir, "source", "pipkin")
	writeTestBinary(t, source, "new console")
	staged, err := stageLocalBinaries(source, filepath.Dir(installedBinary()), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.close()
	actions := installationActions{
		running:       func() (int, error) { return 0, nil },
		stop:          func() error { return nil },
		start:         func() error { return errors.New("startup failed") },
		register:      func() (string, error) { return "test service", nil },
		snapshot:      noAutostartSnapshot,
		configure:     installLauncher,
		undoConfigure: func() error { return os.Remove(launcherPath()) },
	}
	if _, err := activateBinaries(staged, actions); err == nil {
		t.Fatal("fresh install unexpectedly succeeded")
	}
	if _, err := os.Lstat(launcherPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed install left its launcher: %v", err)
	}
}

func TestStoppedReplacementFailurePreservesBackup(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source", "pipkin")
	target := filepath.Join(dir, "installed")
	writeTestBinary(t, source, "new console")
	writeTestBinary(t, filepath.Join(target, "pipkin"), "old console")
	staged, err := stageLocalBinaries(source, target, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.close()
	stops := 0
	actions := installationActions{
		running:  func() (int, error) { return 123, nil },
		snapshot: noAutostartSnapshot,
		stop: func() error {
			stops++
			if stops > 1 {
				return errors.New("replacement still running")
			}
			return nil
		},
		start:    func() error { return errors.New("startup readiness failed") },
		register: func() (string, error) { return "test service", nil },
	}
	if _, err := activateBinaries(staged, actions); err == nil || !strings.Contains(err.Error(), ".old backups") {
		t.Fatalf("unsafe recovery failure was hidden: %v", err)
	}
	checkTestBinary(t, filepath.Join(target, "pipkin.old"), "old console")
	checkTestBinary(t, filepath.Join(target, "pipkin"), "new console")
}

func TestUpgradePreservesOnlyRecoveryCopy(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source", "pipkin")
	target := filepath.Join(dir, "installed")
	writeTestBinary(t, source, "new console")
	writeTestBinary(t, filepath.Join(target, "pipkin.old"), "only working executable")
	staged, err := stageLocalBinaries(source, target, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.close()
	stopped := false
	actions := installationActions{stop: func() error { stopped = true; return nil }}
	if _, err := activateBinaries(staged, actions); err == nil || !strings.Contains(err.Error(), "recovery backup") {
		t.Fatalf("sole recovery backup was not protected: %v", err)
	}
	if stopped {
		t.Fatal("stopped the helper before identifying its sole recovery copy")
	}
	checkTestBinary(t, filepath.Join(target, "pipkin.old"), "only working executable")
}

func TestIncompleteManifestAvoidsBinaryDownloads(t *testing.T) {
	var requests atomic.Int32
	manifest := binaryManifest("windows", "amd64")
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("binary")))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/SHA256SUMS" {
			fmt.Fprintf(w, "%s  %s\n", digest, manifest[0].asset)
			return
		}
		requests.Add(1)
		fmt.Fprint(w, "binary")
	}))
	defer server.Close()
	if staged, err := stageReleaseBinaries(server.URL, t.TempDir(), "windows", "amd64"); err == nil {
		staged.close()
		t.Fatal("accepted a manifest missing its companion checksum")
	}
	if requests.Load() != 0 {
		t.Fatal("downloaded an executable before checking all required checksums")
	}
}

func TestUpdateRefreshesNewWindowlessService(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source", "pipkin.exe")
	target := filepath.Join(dir, "installed")
	writeTestBinary(t, source, "new console")
	writeTestBinary(t, filepath.Join(filepath.Dir(source), "pipkinw.exe"), "new companion")
	writeTestBinary(t, filepath.Join(target, "pipkin.exe"), "old console")
	staged, err := stageLocalBinaries(source, target, "windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.close()
	registered, started := false, false
	service := func() string {
		if _, err := os.Stat(filepath.Join(target, "pipkinw.exe")); err == nil {
			return "companion"
		}
		return "console"
	}
	actions := installationActions{
		running:  func() (int, error) { return 123, nil },
		snapshot: noAutostartSnapshot,
		stop:     func() error { return nil },
		register: func() (string, error) {
			if service() != "companion" {
				t.Fatal("startup still selects the old console executable")
			}
			registered = true
			return "test service", nil
		},
		start: func() error {
			if !registered {
				t.Fatal("started the new helper before refreshing its startup definition")
			}
			checkTestBinary(t, filepath.Join(target, "pipkin.exe"), "new console")
			checkTestBinary(t, filepath.Join(target, "pipkinw.exe"), "new companion")
			started = true
			return nil
		},
	}
	if _, err := activateBinaries(staged, actions); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("successful update did not start the helper")
	}
	if _, err := os.Stat(filepath.Join(target, "pipkin.exe.old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful replacement retained an unused backup: %v", err)
	}
}

func TestRollbackPreservesAbsentAutostartWithInstalledBinary(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source", "pipkin")
	target := filepath.Join(dir, "installed")
	writeTestBinary(t, source, "new console")
	writeTestBinary(t, filepath.Join(target, "pipkin"), "old console")
	staged, err := stageLocalBinaries(source, target, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.close()
	registration := ""
	restarted := false
	actions := installationActions{
		running: func() (int, error) { return 123, nil },
		snapshot: func() (func() error, error) {
			previous := registration
			return func() error { registration = previous; return nil }, nil
		},
		stop: func() error { return nil },
		register: func() (string, error) {
			registration = "new automatic startup"
			return registration, nil
		},
		start: func() error {
			data, err := os.ReadFile(filepath.Join(target, "pipkin"))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) == "new console" {
				if registration == "" {
					t.Fatal("unchanged executable path did not refresh startup registration")
				}
				return errors.New("new helper failed")
			}
			if registration != "" {
				t.Fatal("rollback introduced automatic startup that did not previously exist")
			}
			restarted = true
			return nil
		},
	}
	if _, err := activateBinaries(staged, actions); err == nil {
		t.Fatal("update unexpectedly succeeded")
	}
	if !restarted || registration != "" {
		t.Fatalf("old helper/registration not restored: restarted=%v, registration=%q", restarted, registration)
	}
}

func TestAutostartSnapshotFailureLeavesHelperRunning(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source", "pipkin")
	target := filepath.Join(dir, "installed")
	writeTestBinary(t, source, "new console")
	writeTestBinary(t, filepath.Join(target, "pipkin"), "old console")
	staged, err := stageLocalBinaries(source, target, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.close()
	stopped := false
	actions := installationActions{
		running:  func() (int, error) { return 123, nil },
		snapshot: func() (func() error, error) { return nil, errors.New("startup definition unreadable") },
		stop:     func() error { stopped = true; return nil },
	}
	if _, err := activateBinaries(staged, actions); err == nil {
		t.Fatal("continued without a readable startup snapshot")
	}
	if stopped {
		t.Fatal("stopped helper before validating recovery data")
	}
	checkTestBinary(t, filepath.Join(target, "pipkin"), "old console")
}
