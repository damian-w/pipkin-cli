package pipkin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func statusTestInstallation(t *testing.T, installed bool) string {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "app")
	t.Setenv("PIPKIN_HOME", app)
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	if installed {
		if err := os.MkdirAll(filepath.Dir(installedBinary()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(installedBinary(), []byte("synthetic file; never execute"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func statusTestTree(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			relative += "/"
		}
		entries = append(entries, relative)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

func statusTestRunning(t *testing.T, publish bool) pidRecord {
	t.Helper()
	statusTestInstallation(t, true)
	lock, err := lockInstance()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	record := pidRecord{PID: 4242, processIdentity: processIdentity{Started: "synthetic-start", Executable: installedBinary()}}
	previous := inspectProcess
	inspectProcess = func(pid int) (processIdentity, error) {
		if pid != record.PID {
			return processIdentity{}, os.ErrNotExist
		}
		return record.processIdentity, nil
	}
	t.Cleanup(func() { inspectProcess = previous })
	if publish {
		if err := writeJSON(pidPath(), record); err != nil {
			t.Fatal(err)
		}
	}
	return record
}

func TestStatusAbsentAndEmptyInstallationCreateNothing(t *testing.T) {
	for _, existingDirectory := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing empty directory"}[existingDirectory], func(t *testing.T) {
			root := statusTestInstallation(t, false)
			if existingDirectory {
				if err := os.Mkdir(appDir(), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before := statusTestTree(t, root)
			view, err := loadStatusView()
			if err != nil || view.installed || view.starting || view.pid != 0 {
				t.Fatalf("absent installation became a live state: %#v, %v", view, err)
			}
			var out bytes.Buffer
			if err := writeStatus(&out, view, testNow, "darwin"); err != nil || !strings.Contains(out.String(), "isn't installed") || !strings.Contains(out.String(), "pipkin install") {
				t.Fatalf("missing installation guidance: %q, %v", out.String(), err)
			}
			if after := statusTestTree(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("read-only status created files: before %v, after %v", before, after)
			}
		})
	}
}

func TestStatusInstalledStoppedDoesNotCreateHelperLock(t *testing.T) {
	root := statusTestInstallation(t, true)
	before := statusTestTree(t, root)
	view, err := loadStatusView()
	if err != nil || !view.installed || view.starting || view.pid != 0 {
		t.Fatalf("stopped installation state: %#v, %v", view, err)
	}
	var out bytes.Buffer
	if err := writeStatus(&out, view, testNow, "darwin"); err != nil || !strings.Contains(out.String(), "is stopped") || !strings.Contains(out.String(), "pipkin start") {
		t.Fatalf("stopped guidance: %q, %v", out.String(), err)
	}
	if after := statusTestTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("stopped status created files: before %v, after %v", before, after)
	}
}

func TestStatusStartingPublicationAndSnapshotRaces(t *testing.T) {
	for _, stage := range []string{"unpublished identity", "missing snapshot", "previous helper snapshot"} {
		t.Run(stage, func(t *testing.T) {
			record := statusTestRunning(t, stage != "unpublished identity")
			if stage == "previous helper snapshot" {
				if err := writeJSON(statusPath(), Status{SchemaVersion: 1, PID: record.PID - 1,
					Device: map[string]string{"port": "stale-port", "firmware": "9.8.7"}}); err != nil {
					t.Fatal(err)
				}
			}
			view, err := loadStatusView()
			if err != nil || !view.starting {
				t.Fatalf("startup race became a failure: %#v, %v", view, err)
			}
			var out bytes.Buffer
			if err := writeStatus(&out, view, testNow, "darwin"); err != nil {
				t.Fatal(err)
			}
			if text := out.String(); !strings.Contains(text, "starting up") || strings.Contains(text, "stale-port") || strings.Contains(text, "4242") || strings.Contains(text, "connected") {
				t.Fatalf("startup output used stale display/PID data: %q", text)
			}
		})
	}
}

func TestStatusVerifiedRunningReadsOnlyMatchingCachedSnapshot(t *testing.T) {
	record := statusTestRunning(t, true)
	snapshot := Status{SchemaVersion: 1, PID: record.PID, Updated: 1,
		Device:   map[string]string{"port": "synthetic-port", "firmware": "9.8.7"},
		Readings: map[string]*Reading{"codex": {SessionNoCap: true, Observed: testNow}}}
	if err := writeJSON(statusPath(), snapshot); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statusPath())
	if err != nil {
		t.Fatal(err)
	}
	view, err := loadStatusView()
	if err != nil || view.pid != record.PID || view.starting || !view.installed {
		t.Fatalf("verified running view: %#v, %v", view, err)
	}
	var out bytes.Buffer
	if err := writeStatus(&out, view, testNow, "darwin"); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "Pipkin is running. Display connected") || !strings.Contains(text, "9.8.7") || strings.Contains(text, "4242") || strings.Contains(text, "synthetic-port") {
		t.Fatalf("running status leaked PID/normal port or rejected unchanged cache: %q", text)
	}
	after, err := os.ReadFile(statusPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reading status changed its saved JSON")
	}
	for _, path := range []string{configPath(), filepath.Join(appDir(), "config.lock"), logPath()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("status unexpectedly created %s", filepath.Base(path))
		}
	}
}

func TestStatusInvalidIdentityAndSnapshotAreErrors(t *testing.T) {
	for _, stage := range []string{"malformed identity", "zero PID", "reused PID", "different executable", "unverifiable process", "malformed snapshot", "wrong snapshot type", "null snapshot", "empty snapshot", "unsupported snapshot schema", "snapshot is directory"} {
		t.Run(stage, func(t *testing.T) {
			record := statusTestRunning(t, true)
			switch stage {
			case "malformed identity":
				if err := os.WriteFile(pidPath(), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "zero PID":
				record.PID = 0
				if err := writeJSON(pidPath(), record); err != nil {
					t.Fatal(err)
				}
			case "reused PID":
				inspectProcess = func(int) (processIdentity, error) {
					return processIdentity{Started: "new-start", Executable: installedBinary()}, nil
				}
			case "different executable":
				inspectProcess = func(int) (processIdentity, error) {
					return processIdentity{Started: record.Started, Executable: filepath.Join(appDir(), "unrelated")}, nil
				}
			case "unverifiable process":
				inspectProcess = func(int) (processIdentity, error) { return processIdentity{}, os.ErrPermission }
			case "malformed snapshot", "wrong snapshot type", "null snapshot", "empty snapshot":
				data := "{"
				switch stage {
				case "wrong snapshot type":
					data = "[]"
				case "null snapshot":
					data = "null"
				case "empty snapshot":
					data = "{}"
				}
				if err := os.WriteFile(statusPath(), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unsupported snapshot schema":
				if err := writeJSON(statusPath(), Status{SchemaVersion: 2, PID: record.PID}); err != nil {
					t.Fatal(err)
				}
			case "snapshot is directory":
				if err := os.Mkdir(statusPath(), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			view, err := loadStatusView()
			if err == nil || view.starting {
				t.Fatalf("invalid state was called stopped/starting: %#v, %v", view, err)
			}
		})
	}
}

func TestStatusSettingsPathMustBeDirectory(t *testing.T) {
	statusTestInstallation(t, false)
	if err := os.WriteFile(appDir(), []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if view, err := loadStatusView(); err == nil || view.installed || view.starting {
		t.Fatalf("invalid settings path became not-installed success: %#v, %v", view, err)
	}
}

type statusFailWriter struct{ successfulWrites int }

func (w *statusFailWriter) Write(value []byte) (int, error) {
	if w.successfulWrites == 0 {
		return 0, io.ErrClosedPipe
	}
	w.successfulWrites--
	return len(value), nil
}

func TestStatusWriterErrorsPropagateAcrossOutputStages(t *testing.T) {
	for _, view := range []statusView{
		{}, {installed: true}, {starting: true},
		{pid: 4242, snapshot: Status{Device: map[string]string{"port": "port", "firmware": "9.8.7"}}},
		{pid: 4242, snapshot: Status{Device: map[string]string{"error": "port: resource busy"}}},
	} {
		if err := writeStatus(&statusFailWriter{}, view, testNow, "darwin"); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("initial status writer failure lost: %v", err)
		}
	}
	view := statusView{pid: 4242, snapshot: Status{Device: map[string]string{"port": "port"}}}
	for _, writes := range []int{1, 2, 3} {
		if err := writeStatus(&statusFailWriter{successfulWrites: writes}, view, testNow, "darwin"); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("writer failure after %d writes lost: %v", writes, err)
		}
	}
}

func TestStatusCachedStringsAreSafeBoundedAndDoNotMutateJSON(t *testing.T) {
	for _, connected := range []bool{false, true} {
		device := map[string]string{"error": "/dev/" + strings.Repeat("port", 40) + "\x1b[31m\r\n: permission denied"}
		if connected {
			device = map[string]string{"port": "private-port", "firmware": strings.Repeat("firmware", 40) + "\x1b[31m\r\n"}
		}
		view := statusView{pid: 987654321, snapshot: Status{Device: device,
			Readings:   map[string]*Reading{"codex": {SessionNoCap: true}},
			CodexError: "Codex: " + strings.Repeat("message", 40) + "\x1b[31m\r\n"}}
		before, _ := json.Marshal(view.snapshot)
		var out bytes.Buffer
		if err := writeStatus(&out, view, testNow, "darwin"); err != nil {
			t.Fatal(err)
		}
		for _, char := range out.String() {
			if char != '\n' && (char < ' ' || char > '~') {
				t.Fatalf("unsafe terminal character: %U", char)
			}
		}
		for _, line := range strings.Split(out.String(), "\n") {
			if len(line) > outputWidth {
				t.Fatalf("cached string exceeded terminal width: %d", len(line))
			}
		}
		if strings.Contains(out.String(), "987654321") {
			t.Fatal("human status printed PID noise")
		}
		after, _ := json.Marshal(view.snapshot)
		if !bytes.Equal(before, after) {
			t.Fatal("rendering changed cached JSON")
		}
	}
}

func TestStatusDisplayHintsUseErrorDetailNotPortName(t *testing.T) {
	for _, c := range []struct {
		reason, goos, want, avoid string
	}{
		{"/dev/resource busy: permission denied", "darwin", "Check its permissions", "Another app"},
		{"/dev/permission denied: resource busy", "darwin", "Another app may", "Check its permissions"},
		{"COM9: Access is denied.", "windows", "Another app may", "Another app is"},
		{"port: unfamiliar failure", "darwin", "USB data cable", "Another app"},
		{"", "darwin", "self-built display", "Last connection attempt"},
	} {
		var out bytes.Buffer
		if err := writeDisplayHint(&out, c.reason, c.goos); err != nil {
			t.Fatal(err)
		}
		if text := out.String(); !strings.Contains(text, c.want) || strings.Contains(text, c.avoid) {
			t.Fatalf("display hint misclassified %q: %q", c.reason, text)
		}
	}
}
