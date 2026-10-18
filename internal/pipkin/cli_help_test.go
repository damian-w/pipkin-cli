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

func TestCLIMenuSeparatesEverydayCommands(t *testing.T) {
	for _, goos := range []string{"darwin", "windows", "linux"} {
		menu := formatCLIUsage("test", cliCommands("test", "", "", goos, io.Discard))
		if strings.Contains(menu, "identify") || strings.Contains(menu, "--json") || !strings.Contains(menu, "More: tasks, start, stop, install,") {
			t.Fatalf("incorrect menu grouping on %s: %s", goos, menu)
		}
		for _, line := range strings.Split(menu, "\n") {
			if len(line) > 80 {
				t.Fatalf("menu wraps: %q", line)
			}
		}
	}
}

func TestCLIHelpAliasesNeverRunCommands(t *testing.T) {
	for _, args := range [][]string{{"help", "help"}, {"--help", "usage"}, {"-h", "tasks"}, {"usage", "--json", "--help"}, {"flash", "--port", "--help"}} {
		var out bytes.Buffer
		calls := 0
		code, err := dispatch(args, "test", recordingCommands(&out, &calls), &out)
		if code != 0 || err != nil || calls != 0 || out.Len() == 0 {
			t.Fatalf("%v: code %d err %v calls %d", args, code, err, calls)
		}
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
	if suggestCLICommand("identif", commands) != "" || suggestCLICommand("authoriz", commands) != "" {
		t.Fatal("hidden commands were suggested")
	}
	if suggestCLICommand("res", commands) != "restart" || suggestCLICommand("sta", commands) != "" {
		t.Fatal("unique and ambiguous prefixes were not distinguished")
	}
}

func TestCLIOptionsNameBadArgumentAndKeepExitCodes(t *testing.T) {
	for _, args := range [][]string{{"usage", "--bogus"}, {"tasks", "--bogus"}, {"stop", "--bogus"}, {"flash", "--bogus"}} {
		calls := 0
		code, err := dispatch(args, "test", recordingCommands(io.Discard, &calls), io.Discard)
		if code != 1 || err == nil || calls != 0 || !strings.Contains(err.Error(), `unknown option "--bogus"`) || !strings.Contains(err.Error(), "Usage:") {
			t.Fatalf("%v: %d, %v, calls %d", args, code, err, calls)
		}
	}
	for _, args := range [][]string{{"--port"}, {"--port", "--reinstall"}, {"--version="}} {
		_, err := parseFlashOptions(args)
		if err == nil || !strings.Contains(err.Error(), "needs a value") || !strings.HasPrefix(err.Error(), "--") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if !errors.Is(invalidCLIOption("--bogus"), errUsage) {
		t.Fatal("option sentinel compatibility lost")
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
	if err := os.MkdirAll(filepath.Dir(installedBinary()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installedBinary(), []byte("fixture, never executed"), 0600); err != nil {
		t.Fatal(err)
	}
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
