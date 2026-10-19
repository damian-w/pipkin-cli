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

var cliTestCommandNames = []string{"usage", "authorize", "status", "tasks", "start", "stop", "restart", "update", "flash", "uninstall", "install", "run", "version", "license"}

func TestCLIRejectsArgumentsBeforeInvokingCommands(t *testing.T) {
	for _, command := range cliTestCommandNames {
		for _, option := range []string{"--dry-run", "--json --extra"} {
			calls := 0
			code, err := dispatch([]string{command, option}, "test", recordingCommands(io.Discard, &calls), io.Discard)
			if code != 1 || err == nil || calls != 0 || !strings.Contains(strings.ToLower(err.Error()), "usage: pipkin "+command) ||
				command != "run" && !strings.Contains(err.Error(), `unknown option "`+option+`"`) {
				t.Fatalf("%s %s: code=%d error=%v calls=%d", command, option, code, err, calls)
			}
		}
	}
}

func TestCLIHelpHasNoCommandSideEffects(t *testing.T) {
	check := func(args []string, want string) {
		var output bytes.Buffer
		calls := 0
		code, err := dispatch(args, "test", recordingCommands(&output, &calls), &output)
		if code != 0 || err != nil || calls != 0 || output.Len() == 0 || !strings.Contains(output.String(), want) {
			t.Fatalf("%v: code=%d error=%v calls=%d output=%q", args, code, err, calls, output.String())
		}
	}
	for _, command := range cliTestCommandNames {
		for _, args := range [][]string{{command, "--help"}, {command, "-h"}, {"help", command}} {
			check(args, "pipkin "+command)
		}
	}
	for _, args := range [][]string{{"help", "help"}, {"--help", "usage"}, {"-h", "tasks"}, {"usage", "--json", "--help"}, {"flash", "--port", "--help"}} {
		check(args, "")
	}
}

func TestCLIUsageListsCommandsForPlatform(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			commands := cliCommands("test", "", "", goos, io.Discard)
			menu := formatCLIUsage("test", commands)
			if strings.Contains(menu, ", authorize") != (goos == "darwin") {
				t.Fatalf("incorrect authorize visibility on %s: %q", goos, menu)
			}
			if strings.Contains(menu, "--json") || !strings.Contains(menu, "More: tasks, start, stop, install,") {
				t.Fatalf("incorrect menu grouping on %s: %s", goos, menu)
			}
			for _, command := range []string{"usage", "status", "restart", "update", "flash", "uninstall"} {
				if !strings.Contains(menu, "\n  "+command+" ") {
					t.Fatalf("%s missing from %s command menu: %q", command, goos, menu)
				}
			}
			text := menu
			for _, command := range commands {
				text += "\n" + command.syntax + "\n" + command.summary + "\n" + command.details
			}
			checkTerminalLines(t, text, false)
		})
	}
}

func TestCLIGeneralHelpAndUnknownCommandUsePlatformMenu(t *testing.T) {
	t.Setenv("PIPKIN_HOME", filepath.Join(t.TempDir(), "uninstalled"))
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
		if calls != 0 || !strings.Contains(menu, "Pipkin test shows") || !strings.Contains(menu, "\n  restart ") {
			t.Fatalf("%v: calls=%d menu=%q", args, calls, menu)
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
}

func TestCLIVersionAliasesRemainScriptable(t *testing.T) {
	for _, name := range []string{"version", "--version", "-v", "-V"} {
		var out bytes.Buffer
		code, err := dispatch([]string{name}, "test", cliCommands("test", "", "", "darwin", &out), &out)
		if code != 0 || err != nil || out.String() != "test\n" {
			t.Fatalf("%s: %d, %v, %q", name, code, err, out.String())
		}
	}
}

func TestCLITyposSuggestWithoutExecuting(t *testing.T) {
	for _, name := range []string{"stats", "Status"} {
		calls := 0
		code, err := dispatch([]string{name}, "test", recordingCommands(io.Discard, &calls), io.Discard)
		if code != 2 || err == nil || calls != 0 || !strings.Contains(err.Error(), "Did you mean 'pipkin status'?") || strings.Contains(err.Error(), "More:") {
			t.Fatalf("%s: %d, %v, %d", name, code, err, calls)
		}
	}
	commands := cliCommands("test", "", "", "linux", io.Discard)
	if suggestCLICommand("ru", commands) != "" || suggestCLICommand("authoriz", commands) != "" {
		t.Fatal("hidden commands were suggested")
	}
	if suggestCLICommand("res", commands) != "restart" || suggestCLICommand("sta", commands) != "" {
		t.Fatal("unique and ambiguous prefixes were not distinguished")
	}
}

func TestCLIRoutesTasksWithoutInstallation(t *testing.T) {
	t.Setenv("PIPKIN_HOME", "")
	var out bytes.Buffer
	calls := 0
	commands := testCommands(t, &out, map[string]func([]string) error{"tasks": func(args []string) error {
		calls++
		if len(args) != 1 || args[0] != "--json" {
			t.Fatalf("wrong options: %v", args)
		}
		return nil
	}})
	if code, err := dispatch([]string{"tasks", "--json"}, "test", commands, &out); code != 0 || err != nil || calls != 1 {
		t.Fatalf("task route: %d, %v, %d", code, err, calls)
	}
}

func TestCLIBareCommandShowsStatusAndKeepsHelpMenu(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	writeTestBinary(t, installedBinary(), "fixture, never executed")
	var out bytes.Buffer
	calls := 0
	commands := testCommands(t, &out, map[string]func([]string) error{"status": func(args []string) error {
		calls++
		if len(args) != 0 {
			t.Fatal("bare command requested JSON or a fresh read")
		}
		_, err := io.WriteString(&out, "Pipkin is running.\n")
		return err
	}})
	if code, err := dispatch(nil, "test", commands, &out); code != 0 || err != nil || calls != 1 ||
		out.String() != "Pipkin is running.\n\nRun 'pipkin help' to see all commands.\n" {
		t.Fatalf("bare command: %d, %v, %q", code, err, out.String())
	}
	out.Reset()
	if code, err := dispatch([]string{"help"}, "test", commands, &out); code != 0 || err != nil || calls != 1 || !strings.Contains(out.String(), "More:") {
		t.Fatal("explicit help stopped showing the menu")
	}
	commands = testCommands(t, io.Discard, map[string]func([]string) error{"status": func([]string) error { return io.ErrClosedPipe }})
	if code, err := dispatch(nil, "test", commands, io.Discard); code != 1 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("bare status hid its error")
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

func TestCLIReportsOutputErrors(t *testing.T) {
	t.Setenv("PIPKIN_HOME", filepath.Join(t.TempDir(), "uninstalled"))
	for _, args := range [][]string{nil, {"version"}, {"license"}, {"stop", "--help"}} {
		if code, err := dispatch(args, "test", testCommands(t, &outputTestWriter{err: io.ErrClosedPipe}, nil), &outputTestWriter{err: io.ErrClosedPipe}); code != 1 || !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("%v: %d, %v", args, code, err)
		}
	}
}
