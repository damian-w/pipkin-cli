package pipkin

import (
	"os"
	"testing"
	"time"
)

func TestClaudeTaskDarwinNativeProcessIdentity(t *testing.T) {
	identity, err := ownedProcessInfo(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse("Mon Jan _2 15:04:05 2006", identity.Started); err != nil {
		t.Fatal("native start time does not match the registry's C-locale UTC encoding")
	}
	executable, err := os.Executable()
	if err != nil || !sameExecutablePath(executable, identity.Executable) {
		t.Fatal("native executable path does not identify this test process")
	}
}
