package pipkin

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTerminalTextRemovesControlCharacters(t *testing.T) {
	value := "Pro\x1b[31m\n\r\t\x7f\u009b\u202e"
	want := "Pro\uFFFD[31m\uFFFD\uFFFD\uFFFD\uFFFD\uFFFD\uFFFD"
	if got := terminalText(value); got != want {
		t.Fatalf("terminal text = %q, want %q", got, want)
	}
	if got := terminalText("Pro · 模型"); got != "Pro · 模型" {
		t.Fatalf("printable text changed: %q", got)
	}
	if got := describeWindow(value, nil, 0); got != want+" unknown" {
		t.Fatalf("unsafe model label: %q", got)
	}
}

func TestPreserveExistingLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PIPKIN_HOME", filepath.Join(dir, "app"))
	if err := os.MkdirAll(filepath.Dir(launcherPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcherPath(), []byte("existing command"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installLauncher(); err == nil {
		t.Fatal("overwrote an existing unrelated command")
	}
	if got, _ := os.ReadFile(launcherPath()); string(got) != "existing command" {
		t.Fatalf("existing command changed: %q", got)
	}
}
