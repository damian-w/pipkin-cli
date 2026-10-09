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

// testCommands returns the real command table with the given handlers; others fail the test if run.
func testCommands(t *testing.T, output io.Writer, runs map[string]func([]string) error) []cliCommand {
	commands := cliCommands("test", "license", "notices", runtime.GOOS, output)
	for i := range commands {
		if run, ok := runs[commands[i].name]; ok {
			commands[i].run = run
		} else if !commands[i].standalone {
			name := commands[i].name
			commands[i].run = func([]string) error { t.Fatalf("unexpected %s command", name); return nil }
		}
	}
	return commands
}

func recordingCommands(output io.Writer, calls *int) []cliCommand {
	commands := cliCommands("test", "", "", runtime.GOOS, output)
	for i := range commands {
		commands[i].run = func([]string) error { *calls++; return nil }
	}
	return commands
}

func TestCLIRejectsArgumentsBeforeInvokingCommands(t *testing.T) {
	for _, command := range []string{"usage", "authorize", "status", "start", "stop", "restart", "update", "uninstall", "install", "run", "version", "license", "identify"} {
		for _, option := range []string{"--dry-run", "--json --extra"} {
			calls := 0
			code, err := dispatch([]string{command, option}, "test", recordingCommands(io.Discard, &calls), io.Discard)
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
			code, err := dispatch(args, "test", recordingCommands(&output, &calls), &output)
			if code != 0 || err != nil || calls != 0 || !strings.Contains(output.String(), "pipkin "+command) {
				t.Fatalf("%v: code=%d error=%v calls=%d output=%q", args, code, err, calls, output.String())
			}
		}
	}
}

func TestCLIUsageListsCommandsForPlatform(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			menu := formatCLIUsage("test", cliCommands("test", "", "", goos, io.Discard))
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
		code, err := dispatch(args, "test", recordingCommands(&output, &calls), &output)
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
		commands := testCommands(t, io.Discard, map[string]func([]string) error{"restart": func([]string) error { calls++; return result }})
		code, err := dispatch([]string{"restart"}, "test", commands, io.Discard)
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
	commands := testCommands(t, io.Discard, map[string]func([]string) error{
		"usage":  func(args []string) error { usageJSON = len(args) == 1 && args[0] == "--json"; return nil },
		"status": func(args []string) error { statusJSON = len(args) == 1 && args[0] == "--json"; return boom },
	})
	if code, err := dispatch([]string{"usage", "--json"}, "test", commands, io.Discard); code != 0 || err != nil || !usageJSON {
		t.Fatalf("usage route: %d, %v, %v", code, err, usageJSON)
	}
	if code, err := dispatch([]string{"status", "--json"}, "test", commands, io.Discard); code != 1 || !errors.Is(err, boom) || !statusJSON {
		t.Fatalf("status route: %d, %v, %v", code, err, statusJSON)
	}
	if code, err := dispatch([]string{"unknown"}, "test", commands, io.Discard); code != 2 || err == nil {
		t.Fatalf("unknown command: %d, %v", code, err)
	}
}

func TestCLIRunUsesStartupHome(t *testing.T) {
	original := filepath.Join(t.TempDir(), "original")
	wanted := filepath.Join(t.TempDir(), "startup")
	t.Setenv("PIPKIN_HOME", original)
	calls := 0
	commands := testCommands(t, io.Discard, map[string]func([]string) error{"run": func([]string) error {
		calls++
		if os.Getenv("PIPKIN_HOME") != wanted {
			t.Fatal("startup directory was not applied")
		}
		return nil
	}})
	for _, invalid := range []string{"", "relative"} {
		if code, err := dispatch([]string{"run", "--home", invalid}, "test", commands, io.Discard); code != 1 || err == nil || calls != 0 || os.Getenv("PIPKIN_HOME") != original {
			t.Fatalf("accepted invalid startup home %q", invalid)
		}
	}
	if code, err := dispatch([]string{"run", "--home", wanted}, "test", commands, io.Discard); code != 0 || err != nil || calls != 1 {
		t.Fatalf("run: %d, %v, calls=%d", code, err, calls)
	}
}

type failingCLIWriter struct{}

func (failingCLIWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCLIReportsOutputErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"version"}, {"license"}, {"stop", "--help"}} {
		if code, err := dispatch(args, "test", testCommands(t, failingCLIWriter{}, nil), failingCLIWriter{}); code != 1 || !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("%v: %d, %v", args, code, err)
		}
	}
}

func TestCLIRoutesIdentifyAndExplainsBoardConfirmation(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	calls := 0
	wantArgs := []string{"--port", "TESTPORT", "--issue", "--board", "esp32-2432s028r-dual-usb"}
	commands := testCommands(t, io.Discard, map[string]func([]string) error{"identify": func(args []string) error {
		calls++
		if strings.Join(args, " ") != strings.Join(wantArgs, " ") {
			t.Fatalf("identify arguments: %v", args)
		}
		return nil
	}})
	if code, err := dispatch(append([]string{"identify"}, wantArgs...), "test", commands, io.Discard); code != 0 || err != nil || calls != 1 {
		t.Fatalf("identify: code=%d error=%v calls=%d", code, err, calls)
	}
	for _, args := range [][]string{{"identify", "--json", "--issue"}, {"identify", "--board="}} {
		if code, err := dispatch(args, "test", commands, io.Discard); code != 1 || err == nil || calls != 1 {
			t.Fatalf("invalid identify options: %v code=%d error=%v calls=%d", args, code, err, calls)
		}
	}
	var output bytes.Buffer
	if code, err := dispatch([]string{"identify", "--help"}, "test", commands, &output); code != 0 || err != nil || calls != 1 {
		t.Fatalf("identify help: code=%d error=%v calls=%d", code, err, calls)
	}
	for _, wanted := range []string{"temporarily restarts", "physical board", "MAC address", "--issue", "No issue is sent", "--board PROFILE"} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("identify help omits %q: %s", wanted, output.String())
		}
	}
}
