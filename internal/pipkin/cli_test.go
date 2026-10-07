package pipkin

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recordingHandlers(calls *int) cliHandlers {
	call := func() error { *calls++; return nil }
	withArgs := func([]string) error { return call() }
	return cliHandlers{withArgs, call, call, call, call, call, call, call, withArgs, call, withArgs}
}

func TestCLIRejectsArgumentsBeforeInvokingCommands(t *testing.T) {
	for _, command := range []string{"usage", "authorize", "status", "start", "stop", "update", "uninstall", "install", "run", "version", "license"} {
		for _, option := range []string{"--dry-run", "--json --extra"} {
			calls := 0
			code, err := dispatch([]string{command, option}, "test", "", "", io.Discard, recordingHandlers(&calls))
			if code != 1 || err == nil || calls != 0 {
				t.Fatalf("%s %s: code=%d error=%v calls=%d", command, option, code, err, calls)
			}
		}
	}
}

func TestCLIHelpHasNoCommandSideEffects(t *testing.T) {
	for _, command := range []string{"usage", "authorize", "status", "start", "stop", "update", "uninstall", "install", "run", "version", "license"} {
		for _, args := range [][]string{{command, "--help"}, {command, "-h"}, {"help", command}} {
			var output bytes.Buffer
			calls := 0
			code, err := dispatch(args, "test", "", "", &output, recordingHandlers(&calls))
			if code != 0 || err != nil || calls != 0 || !strings.Contains(output.String(), "pipkin "+command) {
				t.Fatalf("%v: code=%d error=%v calls=%d output=%q", args, code, err, calls, output.String())
			}
		}
	}
}

func TestCLIRoutesJSONAndReportsFailures(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	usageJSON, statusJSON := false, false
	boom := errors.New("command failed")
	handlers := cliHandlers{
		usage:      func(args []string) error { usageJSON = len(args) == 1 && args[0] == "--json"; return nil },
		statusJSON: func() error { statusJSON = true; return boom },
	}
	if code, err := dispatch([]string{"usage", "--json"}, "test", "", "", io.Discard, handlers); code != 0 || err != nil || !usageJSON {
		t.Fatalf("usage route: %d, %v, %v", code, err, usageJSON)
	}
	if code, err := dispatch([]string{"status", "--json"}, "test", "", "", io.Discard, handlers); code != 1 || !errors.Is(err, boom) || !statusJSON {
		t.Fatalf("status route: %d, %v, %v", code, err, statusJSON)
	}
	if code, err := dispatch([]string{"unknown"}, "test", "", "", io.Discard, handlers); code != 2 || err == nil {
		t.Fatalf("unknown command: %d, %v", code, err)
	}
}

func TestCLIRunUsesAndRestoresStartupHome(t *testing.T) {
	original := filepath.Join(t.TempDir(), "original")
	wanted := filepath.Join(t.TempDir(), "startup")
	t.Setenv("PIPKIN_HOME", original)
	calls := 0
	handlers := cliHandlers{run: func() error {
		calls++
		if os.Getenv("PIPKIN_HOME") != wanted {
			t.Fatal("startup directory was not applied")
		}
		return nil
	}}
	if code, err := dispatch([]string{"run", "--home", wanted}, "test", "", "", io.Discard, handlers); code != 0 || err != nil || calls != 1 {
		t.Fatalf("run: %d, %v, calls=%d", code, err, calls)
	}
	if os.Getenv("PIPKIN_HOME") != original {
		t.Fatal("startup directory override was not restored")
	}
	for _, invalid := range []string{"", "relative"} {
		if code, err := dispatch([]string{"run", "--home", invalid}, "test", "", "", io.Discard, handlers); code != 1 || err == nil || calls != 1 {
			t.Fatalf("accepted invalid startup home %q", invalid)
		}
	}
}

type failingCLIWriter struct{}

func (failingCLIWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCLIReportsOutputErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"version"}, {"license"}, {"stop", "--help"}} {
		if code, err := dispatch(args, "test", "license", "notices", failingCLIWriter{}, cliHandlers{}); code != 1 || !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("%v: %d, %v", args, code, err)
		}
	}
}
