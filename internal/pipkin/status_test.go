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
		writeTestBinary(t, installedBinary(), "synthetic file; never execute")
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

func TestStatusReadOnlyStatesCreateNothing(t *testing.T) {
	for _, c := range []struct{ name, want string }{{"absent", "pipkin install"}, {"existing empty directory", "pipkin install"}, {"stopped", "pipkin start"}} {
		t.Run(c.name, func(t *testing.T) {
			root := statusTestInstallation(t, c.name == "stopped")
			if c.name == "existing empty directory" {
				if err := os.Mkdir(appDir(), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before := statusTestTree(t, root)
			view, err := loadStatusView()
			if err != nil || view.installed != (c.name == "stopped") || view.starting || view.pid != 0 {
				t.Fatalf("read-only state became live: %#v, %v", view, err)
			}
			var out bytes.Buffer
			if err := writeStatus(&out, view, testNow, "darwin"); err != nil || !strings.Contains(out.String(), c.want) {
				t.Fatalf("missing guidance: %q, %v", out.String(), err)
			}
			if after := statusTestTree(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("read-only status created files: before %v, after %v", before, after)
			}
		})
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
	// Firmware that names its board says which display is connected.
	out.Reset()
	view.snapshot.Device["board"] = "es3c40p"
	if err := writeStatus(&out, view, testNow, "darwin"); err != nil || !strings.Contains(out.String(), "Pipkin is running. 4-inch display connected (firmware 9.8.7).") {
		t.Fatal(out.String(), err)
	}
	for _, path := range []string{configPath(), filepath.Join(appDir(), "config.lock"), logPath()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("status unexpectedly created %s", filepath.Base(path))
		}
	}
}

func TestStatusInvalidIdentityAndSnapshotAreErrors(t *testing.T) {
	for _, stage := range []string{"reused PID", "malformed snapshot", "empty snapshot", "unsupported snapshot schema"} {
		t.Run(stage, func(t *testing.T) {
			record := statusTestRunning(t, true)
			switch stage {
			case "reused PID":
				inspectProcess = func(int) (processIdentity, error) {
					return processIdentity{Started: "new-start", Executable: installedBinary()}, nil
				}
			case "malformed snapshot", "empty snapshot":
				data := map[string]string{"malformed snapshot": "{", "empty snapshot": "{}"}[stage]
				if err := os.WriteFile(statusPath(), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unsupported snapshot schema":
				if err := writeJSON(statusPath(), Status{SchemaVersion: 2, PID: record.PID}); err != nil {
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

func TestStatusWriterErrorsPropagateAcrossOutputStages(t *testing.T) {
	for _, view := range []statusView{
		{}, {installed: true}, {starting: true},
		{pid: 4242, snapshot: Status{Device: map[string]string{"port": "port", "firmware": "9.8.7"}}},
		{pid: 4242, snapshot: Status{Device: map[string]string{"error": "port: resource busy"}}},
	} {
		if err := writeStatus(&outputTestWriter{err: io.ErrClosedPipe}, view, testNow, "darwin"); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("initial status writer failure lost: %v", err)
		}
	}
	view := statusView{pid: 4242, snapshot: Status{Device: map[string]string{"port": "port"}}}
	for _, writes := range []int{1, 2, 3} {
		if err := writeStatus(&outputTestWriter{writes: writes, err: io.ErrClosedPipe}, view, testNow, "darwin"); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("writer failure after %d writes lost: %v", writes, err)
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
		{"", "darwin", "update its firmware", "Last connection attempt"},
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

func TestStatusJSONMissingSnapshotKeepsContractAndCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	t.Setenv("PIPKIN_HOME", dir)
	var out bytes.Buffer
	err := statusJSONTo(&out)
	if err == nil || err.Error() != "no helper snapshot yet; run `pipkin usage --json` for a fresh read" || out.Len() != 0 {
		t.Fatalf("missing-snapshot JSON contract changed: %v %s", err, out.String())
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing JSON snapshot created files")
	}
}

func TestStatusJSONPreservesCachedFieldsAndDoesNotRewriteSnapshot(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	saved := Status{SchemaVersion: 1, Version: "fixture", PID: 42, Updated: testNow,
		Device:     map[string]string{"port": "COM9", "firmware": "1.0.0"},
		States:     map[string]string{"codex": "unavailable", "claude": "signed_out"},
		Readings:   map[string]*Reading{"codex": {Account: "invented", Observed: testNow, SessionNoCap: true}, "claude": {Account: "claude"}},
		CodexError: "Codex: synthetic cached error", ClaudeError: "Claude: no existing sign-in found",
		Tasks: &TaskSnapshot{SchemaVersion: 1, ObservedAt: testNow, Providers: map[string]TaskProvider{
			"codex": {Coverage: "partial", Tasks: []Task{{ID: "codex:invented", Status: "working"}}},
		}}}
	if err := writeJSON(statusPath(), saved); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statusPath())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := statusJSONTo(&out); err != nil {
		t.Fatal(err)
	}
	expected := saved
	expected.PID = 0
	expected.Tasks = &TaskSnapshot{SchemaVersion: 1, ObservedAt: testNow, Providers: map[string]TaskProvider{"codex": {Coverage: "unavailable"}}}
	want, err := json.MarshalIndent(expected, "", "  ")
	if err != nil || out.String() != string(want)+"\n" {
		t.Fatalf("cached JSON contract changed: %v\n%s", err, out.String())
	}
	after, err := os.ReadFile(statusPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reading JSON rewrote the cached snapshot")
	}
}
