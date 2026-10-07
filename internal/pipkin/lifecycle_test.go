package pipkin

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestHelperLockDistinguishesContentionFromIOFailure(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	lock, err := lockInstance()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockInstance(); !errors.Is(err, errAlreadyRunning) {
		t.Fatalf("contention = %v", err)
	}
	if running, err := helperRunning(); err != nil || !running {
		t.Fatalf("running = %v, %v", running, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if running, err := helperRunning(); err != nil || running {
		t.Fatalf("released = %v, %v", running, err)
	}
	bad := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(bad, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPKIN_HOME", bad)
	if running, err := helperRunning(); err == nil || running || errors.Is(err, errAlreadyRunning) {
		t.Fatalf("bad home = %v, %v", running, err)
	}
	if err := startHelper(); err == nil {
		t.Fatal("start succeeded with unusable home")
	}
}

func TestHelperPIDOwnershipAndLegacyCompatibility(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	lock, err := lockInstance()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	originalInspect, originalTerminate := inspectProcess, terminateProcess
	t.Cleanup(func() { inspectProcess, terminateProcess = originalInspect, originalTerminate })
	identity := processIdentity{Started: "original-start", Executable: installedBinary()}
	inspectProcess = func(int) (processIdentity, error) { return identity, nil }
	terminated := false
	terminateProcess = func(int) error { terminated = true; return nil }
	if _, err := runningPID(); !errors.Is(err, errHelperStarting) {
		t.Fatalf("unpublished PID = %v", err)
	}
	if err := writeJSON(pidPath(), pidRecord{42, identity}); err != nil {
		t.Fatal(err)
	}
	if pid, err := runningPID(); err != nil || pid != 42 {
		t.Fatalf("valid PID = %d, %v", pid, err)
	}
	identity.Started = "reused-pid"
	if err := terminateHelper(42); err == nil || terminated {
		t.Fatal("signalled reused PID")
	}
	if err := os.WriteFile(pidPath(), []byte("42"), 0600); err != nil {
		t.Fatal(err)
	}
	if pid, err := runningPID(); err != nil || pid != 42 {
		t.Fatalf("legacy helper = %d, %v", pid, err)
	}
	identity.Executable = filepath.Join(t.TempDir(), "unrelated")
	if err := terminateHelper(42); err == nil || terminated {
		t.Fatal("signalled unrelated legacy PID")
	}
	for _, invalid := range []string{"-1", "0", "garbage", `{"pid":42,"started":"only-start"}`} {
		if err := os.WriteFile(pidPath(), []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := runningPID(); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

func TestPublishPIDRequiresWritableRecordAndMatchesCurrentProcess(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	lock, err := lockInstance()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := publishPID(); err != nil {
		t.Fatal(err)
	}
	if pid, err := runningPID(); err != nil || pid != os.Getpid() {
		t.Fatalf("published current PID = %d, %v", pid, err)
	}
	if err := removePID(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pidPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := publishPID(); err == nil {
		t.Fatal("ignored failed PID publication")
	}
}

func TestLegacyPIDCannotBeNegative(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	if err := os.WriteFile(pidPath(), []byte(strconv.Itoa(-1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPIDRecord(); err == nil {
		t.Fatal("negative PID accepted")
	}
}

func TestLifecycleWaitPropagatesFailureAndTimeout(t *testing.T) {
	wanted := errors.New("probe failed")
	if err := waitFor(func() (bool, error) { return false, wanted }, time.Second); !errors.Is(err, wanted) {
		t.Fatalf("probe error = %v", err)
	}
	if err := waitFor(func() (bool, error) { return false, nil }, 0); err == nil {
		t.Fatal("timeout accepted")
	}
	if err := waitFor(func() (bool, error) { return true, nil }, 0); err != nil {
		t.Fatal(err)
	}
}
