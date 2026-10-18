package pipkin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"
)

type flashTestOutputHook struct {
	w     io.Writer
	write func(string) error
}

func (out flashTestOutputHook) Write(data []byte) (int, error) {
	if err := out.write(string(data)); err != nil {
		return 0, err
	}
	return out.w.Write(data)
}

func TestFlashCancellationBeforeWriteNeverStartsEraseOrWrite(t *testing.T) {
	for _, phase := range []string{"initial", "release", "prepare", "identity", "probe", "partition read", "application read", "prompt", "MAC recheck", "write progress"} {
		t.Run(phase, func(t *testing.T) {
			f := newFlashFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var output io.Writer = &f.output
			switch phase {
			case "initial":
				cancel()
			case "release":
				f.actions.load = func(context.Context, string) (*firmwareRelease, error) { cancel(); return f.release, nil }
			case "prepare":
				f.actions.prepare = func(context.Context, io.Writer) (firmwareFlasher, error) { cancel(); return f.tool, nil }
			case "identity":
				original := f.actions.identity
				f.actions.identity = func(ctx context.Context, port string, timeout time.Duration) (map[string]string, error) {
					cancel()
					return original(ctx, port, timeout)
				}
			case "probe", "MAC recheck":
				f.tool.onProbe = func(_ context.Context, count int) {
					if phase == "probe" && count == 1 || phase == "MAC recheck" && count == 2 {
						cancel()
					}
				}
			case "partition read", "application read":
				f.tool.onRead = func(_ context.Context, offset int64) {
					if phase == "partition read" && offset == 0x8000 || phase == "application read" && offset == 0x10000 {
						cancel()
					}
				}
			case "prompt", "write progress":
				output = flashTestOutputHook{w: &f.output, write: func(text string) error {
					if phase == "prompt" && strings.Contains(text, "[y/N]") || phase == "write progress" && strings.Contains(text, "about a minute") {
						cancel()
					}
					return nil
				}}
			}
			f.tool.onReset = func(ctx context.Context) {
				deadline, bounded := ctx.Deadline()
				if ctx.Err() != nil || !bounded || time.Until(deadline) > 20*time.Second {
					t.Error("cleanup reset reused the canceled context or lost its deadline")
				}
			}
			if err := runFlash(ctx, flashOptions{}, strings.NewReader("yes\n"), output, f.actions); err != nil {
				t.Fatalf("pre-write cancellation was not calm: %v", err)
			}
			if f.tool.erases != 0 || len(f.tool.writes) != 0 || f.stops != f.starts {
				t.Fatalf("cancellation crossed the write boundary or lost restoration: erases=%d writes=%d stops/starts=%d/%d", f.tool.erases, len(f.tool.writes), f.stops, f.starts)
			}
			if f.tool.probes > 0 && f.tool.resets != 1 {
				t.Fatal("probed board was not reset")
			}
			if strings.Count(f.output.String(), "Cancelled. No firmware was written.") != 1 || strings.Contains(f.output.String(), "Finishing the firmware write") {
				t.Fatal(f.output.String())
			}
		})
	}
}

type flashTestContextKey struct{}

func TestFlashCancellationDuringCriticalPhasesFinishesSafely(t *testing.T) {
	for _, phase := range []string{"first erase", "update write", "after write", "reset", "boot check"} {
		t.Run(phase, func(t *testing.T) {
			f := newFlashFixture(t)
			if phase == "update write" {
				f.initial = testFlashIdentity("1.0.0")
			}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), flashTestContextKey{}, "fixture"))
			defer cancel()
			check := func(ctx context.Context) {
				if ctx.Err() != nil || ctx.Done() != nil || ctx.Value(flashTestContextKey{}) != "fixture" {
					t.Error("critical operation inherited cancellation or lost context values")
				}
			}
			f.tool.onErase = func(ctx context.Context) {
				if phase == "first erase" {
					cancel()
				}
				check(ctx)
			}
			f.tool.onWrite = func(ctx context.Context) {
				if phase == "update write" {
					cancel()
				}
				check(ctx)
			}
			if phase == "after write" {
				f.tool.afterWrite = cancel
			}
			f.tool.onReset = func(ctx context.Context) {
				if phase == "reset" {
					cancel()
				}
				check(ctx)
			}
			identity := f.actions.identity
			f.actions.identity = func(ctx context.Context, port string, timeout time.Duration) (map[string]string, error) {
				if f.identities == 1 {
					if phase == "boot check" {
						cancel()
					}
					check(ctx)
					if timeout != 12*time.Second {
						t.Error("boot check lost its timeout")
					}
				}
				return identity(ctx, port, timeout)
			}
			if err := f.run(ctx, flashOptions{}, "yes\n"); err != nil {
				t.Fatal(err)
			}
			if len(f.tool.writes) != 1 || f.tool.resets != 1 || f.identities != 2 || f.starts != 1 {
				t.Fatal("cancellation skipped firmware verification, restart, boot check or helper restoration")
			}
			text := f.output.String()
			if strings.Count(text, "Finishing the firmware write. Keep the cable plugged in.") != 1 || strings.Contains(text, "No firmware was written") || strings.Index(text, "Finishing the firmware write") > strings.Index(text, "1.0.1 is running") {
				t.Fatal(text)
			}
		})
	}
}

func TestFlashCancellationKeepsCleanupFailuresVisible(t *testing.T) {
	for _, phase := range []string{"reset", "helper"} {
		t.Run(phase, func(t *testing.T) {
			f := newFlashFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.tool.onProbe = func(context.Context, int) { cancel() }
			boom := errors.New("fixture cleanup failed")
			if phase == "reset" {
				f.tool.resetErr = boom
			} else {
				f.startErr = boom
			}
			err := f.run(ctx, flashOptions{}, "yes\n")
			if !errors.Is(err, boom) || f.tool.erases != 0 || len(f.tool.writes) != 0 || f.starts != 1 || !strings.Contains(f.output.String(), "Cancelled. No firmware was written.") {
				t.Fatalf("cancellation hid cleanup failure: %v", err)
			}
		})
	}
}

func TestFlashCancellationKeepsPreWriteJoinedFailures(t *testing.T) {
	other := errors.New("fixture independent failure")
	for _, c := range []struct {
		name  string
		cause error
		want  []error
	}{
		{"pure wrapped cancellation", fmt.Errorf("probe interrupted: %w", context.Canceled), nil},
		{"process cleanup failure", errors.Join(context.Canceled, errFlashProcessCleanup), []error{errFlashProcessCleanup}},
		{"wrapped nested failures", fmt.Errorf("probe failed: %w", errors.Join(fmt.Errorf("interrupted: %w", context.Canceled), errors.Join(fmt.Errorf("job cleanup: %w", errFlashProcessCleanup), other))), []error{errFlashProcessCleanup, other}},
		{"independent operation timeout", errors.Join(context.Canceled, context.DeadlineExceeded), []error{context.DeadlineExceeded}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFlashFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.tool.onProbe = func(context.Context, int) { cancel() }
			f.tool.probeErr = c.cause
			err := f.run(ctx, flashOptions{}, "yes\n")
			if len(c.want) == 0 && err != nil {
				t.Fatalf("pure cancellation was not calm: %v", err)
			}
			for _, want := range c.want {
				if !errors.Is(err, want) {
					t.Fatalf("cancellation hid %v: %v", want, err)
				}
			}
			if errors.Is(err, context.Canceled) || f.tool.erases != 0 || len(f.tool.writes) != 0 || f.tool.resets != 1 || f.starts != 1 || strings.Count(f.output.String(), "Cancelled. No firmware was written.") != 1 {
				t.Fatalf("cancellation or restoration changed: %v, %s", err, f.output.String())
			}
		})
	}
}

func TestFlashCriticalToolFailuresRemainFailures(t *testing.T) {
	for _, phase := range []string{"erase", "write", "reset", "boot"} {
		t.Run(phase, func(t *testing.T) {
			f := newFlashFixture(t)
			boom := context.DeadlineExceeded
			switch phase {
			case "erase":
				f.tool.eraseErr = boom
			case "write":
				f.tool.writeErr = boom
			case "reset":
				f.tool.resetErr = boom
			case "boot":
				identity := f.actions.identity
				f.actions.identity = func(ctx context.Context, port string, timeout time.Duration) (map[string]string, error) {
					if f.identities == 1 {
						return nil, boom
					}
					return identity(ctx, port, timeout)
				}
			}
			err := f.run(context.Background(), flashOptions{}, "yes\n")
			if !errors.Is(err, boom) || f.starts != 1 || !strings.Contains(err.Error(), "To fix it:") || strings.Contains(f.output.String(), "Cancelled. No firmware was written.") {
				t.Fatalf("critical operation failure lost cause or recovery: %v", err)
			}
		})
	}
}

func TestFlashCancellationDuringFailureCleanupKeepsRecovery(t *testing.T) {
	f := newFlashFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boom := errors.New("fixture write failed")
	f.tool.writeErr = boom
	f.tool.onReset = func(ctx context.Context) {
		cancel()
		if ctx.Err() != nil {
			t.Error("failure cleanup inherited cancellation")
		}
	}
	err := f.run(ctx, flashOptions{}, "yes\n")
	if !errors.Is(err, boom) || f.starts != 1 || strings.Count(f.output.String(), "Finishing the firmware write") != 1 || strings.Contains(f.output.String(), "No firmware was written") {
		t.Fatalf("cancellation disrupted failure cleanup: %v, %s", err, f.output.String())
	}
}

func TestFlashWriterFailuresRespectTheWriteBoundary(t *testing.T) {
	for _, phase := range []string{"release", "probe warning", "plan", "prompt", "write progress", "late cancellation", "success"} {
		t.Run(phase, func(t *testing.T) {
			f := newFlashFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			boom := errors.New("fixture terminal failed")
			needle := map[string]string{"release": "Checking the firmware release", "probe warning": "screen may go blank", "plan": "Ready to install", "prompt": "[y/N]", "write progress": "about a minute", "late cancellation": "Finishing the firmware write", "success": "is running"}[phase]
			output := flashTestOutputHook{w: &f.output, write: func(text string) error {
				if strings.Contains(text, needle) {
					return boom
				}
				return nil
			}}
			if phase == "late cancellation" {
				f.tool.afterWrite = cancel
			}
			err := runFlash(ctx, flashOptions{}, strings.NewReader("yes\n"), output, f.actions)
			if !errors.Is(err, boom) || f.stops != f.starts {
				t.Fatalf("writer failure lost its cause or helper restoration: %v", err)
			}
			if phase == "late cancellation" || phase == "success" {
				if len(f.tool.writes) != 1 || f.identities != 2 || strings.Contains(err.Error(), "To fix it:") {
					t.Fatalf("terminal failure interrupted or misreported a successful write: %v", err)
				}
			} else if len(f.tool.writes) != 0 || f.tool.erases != 0 {
				t.Fatal("terminal failure before confirmation/write still modified firmware")
			}
		})
	}
}

func TestFlashPlansAndProgressArePlainAndSpecific(t *testing.T) {
	for _, c := range []struct {
		action, profile string
		profiles        int
		warning         bool
	}{{"Install", "", 1, false}, {"Install", "", 2, true}, {"Install", "confirmed", 2, false}, {"Update", "", 2, false}, {"Reinstall", "", 2, false}, {"Downgrade", "", 2, false}} {
		t.Run(c.action+c.profile+string(rune('0'+c.profiles)), func(t *testing.T) {
			var output bytes.Buffer
			prompt, err := writeFlashPlan(&output, flashPlan{action: c.action, installed: "1.0.0", selected: "1.0.1", port: "TESTPORT", profile: c.profile, firmwareProfiles: c.profiles})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), "profile is unconfirmed") != c.warning || strings.Contains(prompt, "Confirm this is a CYD") != (c.action == "Install") || strings.Contains(output.String(), "Your settings are kept") != (c.action != "Install") {
				t.Fatal(output.String(), prompt)
			}
			for _, line := range strings.Split(output.String(), "\n") {
				if len(line) >= 80 || strings.IndexFunc(line, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
					t.Fatalf("unsafe or overlong plan line: %q", line)
				}
			}
		})
	}
	f := newFlashFixture(t)
	profiles, err := boardProfiles()
	if err != nil {
		t.Fatal(err)
	}
	f.tool.onProbe = func(context.Context, int) {
		if !strings.Contains(f.output.String(), "screen may go blank") {
			t.Error("screen blanked before its warning")
		}
	}
	f.tool.onErase = func(context.Context) {
		if !strings.Contains(f.output.String(), "about a minute") {
			t.Error("erase started without progress/cable guidance")
		}
	}
	if err := f.run(context.Background(), flashOptions{}, "yes\n"); err != nil {
		t.Fatal(err)
	}
	if len(profiles) == 1 && strings.Contains(f.output.String(), "profile is unconfirmed") {
		t.Fatal("physical variants were counted as separate firmware profiles", f.output.String())
	}
	for _, line := range strings.Split(f.output.String(), "\n") {
		if len(line) >= 80 {
			t.Fatalf("flash output exceeds terminal width: %q", line)
		}
	}
}

func flashRecoveryHint(goos, port string) string {
	return commandHintFor(goos, "/unused", true, "flash", "--port", port, "--version", "1.0.1", "--reinstall")
}

func TestFlashRecoveryIsReadableSafeAndPreservesCause(t *testing.T) {
	boom := errors.New("A fatal error occurred: fixture serial failure\x1b[2J\n" + strings.Repeat("detail ", 40))
	for _, written := range []bool{false, true} {
		err := &flashWriteFailure{cause: boom, written: written, command: flashRecoveryHint("darwin", "TESTPORT")}
		text := err.Error()
		if !errors.Is(err, boom) || strings.Contains(text, "didn't finish") == written || strings.Index(text, "To fix it:") > strings.Index(text, "Details:") || !strings.Contains(text, "pipkin flash --port TESTPORT --version 1.0.1 --reinstall") {
			t.Fatal(text)
		}
		for _, line := range strings.Split(text, "\n") {
			if len(line) >= 80 || strings.IndexFunc(line, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
				t.Fatalf("unsafe or overlong recovery line: %q", line)
			}
		}
	}
}

func TestFlashRecoveryKeepsLongQuotedCommandOnOneLine(t *testing.T) {
	port := "/dev/cu.usbserial-" + strings.Repeat("long-display-", 7) + " $(literal);'quoted"
	command := flashRecoveryHint(runtime.GOOS, port)
	failure := &flashWriteFailure{cause: errors.New("fixture serial failure"), command: command}
	if len(command) < 80 || strings.Contains(command, "\n") {
		t.Fatal("fixture must exercise a long, atomic command")
	}
	text := failure.Error()
	if !strings.Contains(text, "  2. Run this command:\n     "+command+"\n") {
		t.Fatalf("recovery command was wrapped or its quoting changed:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if line != "     "+command && len(line) >= 80 {
			t.Fatalf("recovery prose exceeded the terminal width: %q", line)
		}
	}
}

func TestCommandHintArgumentsPreserveExactPaths(t *testing.T) {
	values := []string{"/tmp/serial-é", "/tmp/串口 'quoted' $(literal);back\\slash", "/tmp/serial-\n\r\t\x1b"}
	for _, goos := range []string{"darwin", "linux", "windows"} {
		for _, value := range values {
			command := flashRecoveryHint(goos, value)
			if strings.IndexFunc(command, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
				t.Fatalf("command hint contains terminal controls or multiple physical lines: %q", command)
			}
			if goos != "windows" && runtime.GOOS != "windows" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				parse := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-c", "printf '%s' "+shellQuote(value))
				parse.Env = []string{"PATH=/usr/bin:/bin", "BASH_ENV=/dev/null", "ENV=/dev/null"}
				data, err := parse.Output()
				cancel()
				if err != nil || string(data) != value {
					t.Fatalf("argument changed its POSIX path: got=%q want=%q err=%v", data, value, err)
				}
			}
		}
	}
}

func TestCommandHintPowerShellArgumentsPreserveExactPaths(t *testing.T) {
	name := "pwsh"
	if runtime.GOOS == "windows" {
		name = "powershell.exe"
	}
	powerShell, err := exec.LookPath(name)
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	values := []string{"C:\\serial-é", "C:\\串口 'quoted' $(literal);back\\slash", "C:\\serial-\n\r\t\x1b"}
	var script strings.Builder
	script.WriteString("[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false);")
	for _, value := range values {
		script.WriteString("[Console]::Write(" + powerShellQuote(value) + ");[Console]::Write([char]0);")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script.String())
	data, err := command.Output()
	if err != nil {
		t.Fatalf("PowerShell argument parser failed: %v", err)
	}
	parts := bytes.Split(data, []byte{0})
	if len(parts) != len(values)+1 || len(parts[len(parts)-1]) != 0 {
		t.Fatalf("PowerShell changed argument boundaries: %q", data)
	}
	for i, value := range values {
		if string(parts[i]) != value {
			t.Fatalf("argument changed its PowerShell path: got=%q want=%q", parts[i], value)
		}
	}
}
