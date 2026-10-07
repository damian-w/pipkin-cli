package pipkin

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func recordingHandlers(calls *int) cliHandlers {
	call := func() error { *calls++; return nil }
	withArgs := func([]string) error { return call() }
	return cliHandlers{
		usage: withArgs, authorize: call, status: call, statusJSON: call,
		start: call, stop: call, restart: call, update: call, uninstall: call,
		install: withArgs, run: call, flash: withArgs, identify: withArgs,
	}
}

func TestCLIRejectsArgumentsBeforeInvokingCommands(t *testing.T) {
	for _, command := range []string{"usage", "authorize", "status", "start", "stop", "restart", "update", "uninstall", "install", "run", "version", "license", "identify"} {
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
	for _, command := range []string{"usage", "authorize", "status", "start", "stop", "restart", "update", "uninstall", "install", "run", "version", "license", "identify"} {
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

func TestCLIUsageListsCommandsForPlatform(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			menu := formatCLIUsage("test", goos)
			if strings.Contains(menu, "\n  authorize ") != (goos == "darwin") {
				t.Fatalf("incorrect authorize visibility on %s: %q", goos, menu)
			}
			for _, command := range []string{"usage", "status", "start", "stop", "restart", "update", "flash", "identify", "uninstall", "install", "version", "license"} {
				if !strings.Contains(menu, "\n  "+command+" ") {
					t.Fatalf("%s missing from %s command menu: %q", command, goos, menu)
				}
			}
		})
	}
}

func TestCLIGeneralHelpAndUnknownCommandUsePlatformMenu(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}, {"unknown"}} {
		var output bytes.Buffer
		calls := 0
		code, err := dispatch(args, "test", "", "", &output, recordingHandlers(&calls))
		menu := output.String()
		if len(args) > 0 && args[0] == "unknown" {
			if code != 2 || err == nil {
				t.Fatalf("unknown command: code=%d error=%v", code, err)
			}
			menu = err.Error()
		} else if code != 0 || err != nil {
			t.Fatalf("%v: code=%d error=%v", args, code, err)
		}
		if calls != 0 || !strings.Contains(menu, "Pipkin display helper test") || !strings.Contains(menu, "\n  restart ") {
			t.Fatalf("%v: calls=%d menu=%q", args, calls, menu)
		}
		if strings.Contains(menu, "\n  authorize ") != (runtime.GOOS == "darwin") {
			t.Fatalf("%v: incorrect authorize visibility on %s: %q", args, runtime.GOOS, menu)
		}
	}
}

func TestCLIRoutesRestartAndReportsFailures(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	boom := errors.New("restart failed")
	for _, result := range []error{nil, boom} {
		calls := 0
		handlers := cliHandlers{restart: func() error { calls++; return result }}
		code, err := dispatch([]string{"restart"}, "test", "", "", io.Discard, handlers)
		wantCode := 0
		if result != nil {
			wantCode = 1
		}
		if calls != 1 || !errors.Is(err, result) || code != wantCode {
			t.Fatalf("restart: code=%d error=%v calls=%d result=%v", code, err, calls, result)
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

func TestCLIRoutesIdentifyAndExplainsBoardConfirmation(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	calls := 0
	wantArgs := []string{"--port", "TESTPORT", "--issue", "--board", "test-cyd-profile"}
	handlers := cliHandlers{identify: func(args []string) error {
		calls++
		if strings.Join(args, " ") != strings.Join(wantArgs, " ") {
			t.Fatalf("identify arguments: %v", args)
		}
		return nil
	}}
	if code, err := dispatch(append([]string{"identify"}, wantArgs...), "test", "", "", io.Discard, handlers); code != 0 || err != nil || calls != 1 {
		t.Fatalf("identify: code=%d error=%v calls=%d", code, err, calls)
	}
	for _, args := range [][]string{{"identify", "--json", "--issue"}, {"identify", "--board="}} {
		if code, err := dispatch(args, "test", "", "", io.Discard, handlers); code != 1 || err == nil || calls != 1 {
			t.Fatalf("invalid identify options: %v code=%d error=%v calls=%d", args, code, err, calls)
		}
	}
	var output bytes.Buffer
	if code, err := dispatch([]string{"identify", "--help"}, "test", "", "", &output, handlers); code != 0 || err != nil || calls != 1 {
		t.Fatalf("identify help: code=%d error=%v calls=%d", code, err, calls)
	}
	for _, wanted := range []string{"temporarily restarts", "physical board", "MAC address", "--issue", "No issue is sent", "--board PROFILE"} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("identify help omits %q: %s", wanted, output.String())
		}
	}
}
