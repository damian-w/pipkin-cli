package pipkin

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func isolateLinuxService(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("PIPKIN_HOME", filepath.Join(t.TempDir(), "Pipkin %Q $money \"quotes\" \\home"))
	original := serviceCommand
	t.Cleanup(func() { serviceCommand = original })
}

func TestLinuxAutostartPreservesHomeAndValidCommandEscaping(t *testing.T) {
	isolateLinuxService(t)
	if err := writeFileAtomic(installedBinary(), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	serviceCommand = func(string, ...string) error { return nil }
	if _, err := registerAutostart(); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(systemdUnitPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), " run --home ") {
		t.Fatal("unit omitted installation directory")
	}
	if validator, err := exec.LookPath("systemd-analyze"); err == nil {
		if output, err := exec.Command(validator, "verify", systemdUnitPath()).CombinedOutput(); err != nil {
			t.Fatalf("systemd rejected startup definition: %v\n%s", err, output)
		}
	}
	serviceCommand = func(string, ...string) error { return errors.New("no systemd session") }
	if _, err := registerAutostart(); err != nil {
		t.Fatal(err)
	}
	entry, err := os.ReadFile(desktopEntryPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entry), " run --home ") {
		t.Fatal("desktop entry omitted installation directory")
	}
	if validator, err := exec.LookPath("desktop-file-validate"); err == nil {
		if output, err := exec.Command(validator, desktopEntryPath()).CombinedOutput(); err != nil {
			t.Fatalf("desktop validator rejected startup definition: %v\n%s", err, output)
		}
	}
}

func TestLinuxAutostartRegistrationReportsEnableFailure(t *testing.T) {
	isolateLinuxService(t)
	wanted := errors.New("enable denied")
	if err := writeFileAtomic(desktopEntryPath(), []byte("existing fallback"), 0644); err != nil {
		t.Fatal(err)
	}
	serviceCommand = func(_ string, args ...string) error {
		if len(args) > 1 && args[1] == "enable" {
			return wanted
		}
		return nil
	}
	if _, err := registerAutostart(); !errors.Is(err, wanted) {
		t.Fatalf("register error = %v", err)
	}
	if data, err := os.ReadFile(desktopEntryPath()); err != nil || string(data) != "existing fallback" {
		t.Fatal("lost previous startup before registration succeeded")
	}
}

func TestLinuxAutostartRollbackPreservesAbsentRegistration(t *testing.T) {
	isolateLinuxService(t)
	disabled := false
	serviceCommand = func(_ string, args ...string) error {
		if args[1] == "disable" {
			disabled = true
		}
		return nil
	}
	restore, err := snapshotAutostart()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registerAutostart(); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if !disabled {
		t.Fatal("rollback left newly registered service enabled")
	}
	for _, path := range []string{systemdUnitPath(), desktopEntryPath()} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback created prior absent registration: %s, %v", path, err)
		}
	}
}

func TestLinuxAutostartRollbackRestoresExistingDefinitions(t *testing.T) {
	isolateLinuxService(t)
	serviceCommand = func(string, ...string) error { return nil }
	if err := writeFileAtomic(systemdUnitPath(), []byte("old unit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(desktopEntryPath(), []byte("old fallback"), 0640); err != nil {
		t.Fatal(err)
	}
	restore, err := snapshotAutostart()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registerAutostart(); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{systemdUnitPath(): "old unit", desktopEntryPath(): "old fallback"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != expected {
			t.Fatalf("restore %s = %q, %v", path, data, err)
		}
	}
}

func TestLinuxServiceCommandsReportFailure(t *testing.T) {
	isolateLinuxService(t)
	if err := writeFileAtomic(systemdUnitPath(), nil, 0644); err != nil {
		t.Fatal(err)
	}
	wanted := errors.New("service command denied")
	serviceCommand = func(_ string, args ...string) error {
		if len(args) > 1 && args[1] == "show-environment" {
			return nil
		}
		return wanted
	}
	if err := startService(); !errors.Is(err, wanted) {
		t.Fatalf("start = %v", err)
	}
	if err := stopService(); !errors.Is(err, wanted) {
		t.Fatalf("stop = %v", err)
	}
	if err := removeAutostart(); !errors.Is(err, wanted) {
		t.Fatalf("remove = %v", err)
	}
	if _, err := os.Stat(systemdUnitPath()); err != nil {
		t.Fatal("removed unit despite failed disable")
	}
}

func TestStopHelperVerifiesPIDBeforeServiceOrSignal(t *testing.T) {
	isolateLinuxService(t)
	lock, err := lockInstance()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := os.WriteFile(pidPath(), []byte("-1"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	serviceCommand = func(string, ...string) error { calls++; return nil }
	if err := stopHelper(); err == nil || calls != 0 {
		t.Fatalf("unverified stop: error=%v, calls=%d", err, calls)
	}
}

func TestStopHelperPropagatesServiceFailureAfterSafeTermination(t *testing.T) {
	isolateLinuxService(t)
	if err := writeFileAtomic(systemdUnitPath(), nil, 0644); err != nil {
		t.Fatal(err)
	}
	lock, err := lockInstance()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	identity := processIdentity{Started: "start", Executable: installedBinary()}
	if err := writeJSON(pidPath(), pidRecord{42, identity}); err != nil {
		t.Fatal(err)
	}
	originalInspect, originalTerminate := inspectProcess, terminateProcess
	t.Cleanup(func() { inspectProcess, terminateProcess = originalInspect, originalTerminate })
	inspectProcess = func(int) (processIdentity, error) { return identity, nil }
	terminated := false
	terminateProcess = func(int) error { terminated = true; return lock.Close() }
	wanted := errors.New("manager refused stop")
	serviceCommand = func(_ string, args ...string) error {
		if args[1] == "stop" {
			return wanted
		}
		return nil
	}
	if err := stopHelper(); !errors.Is(err, wanted) || !terminated {
		t.Fatalf("stop = %v, terminated=%v", err, terminated)
	}
}
