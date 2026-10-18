package pipkin

import (
	"os"
	"strconv"
	"testing"
)

func TestClaudeTaskWindowsNativeProcessIdentity(t *testing.T) {
	identity, err := claudeTaskProcessInfo(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if started, err := strconv.ParseUint(identity.Started, 10, 64); err != nil || started == 0 {
		t.Fatal("native start time does not match registry FILETIME encoding")
	}
	executable, err := os.Executable()
	if err != nil || !sameExecutablePath(executable, identity.Executable) {
		t.Fatal("native executable path does not identify this test process")
	}
}
