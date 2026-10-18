package pipkin

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestFlashWindowsConsoleInterruptAndDeadline(t *testing.T) {
	// An isolated console prevents this fixture's broadcast from reaching the
	// test runner or any real flashing operation.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := flashWindowsConsoleFixtureCommand(ctx, "driver", t.TempDir())
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NEW_CONSOLE}
	command.WaitDelay = 2 * time.Second
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("isolated console fixture failed: %v\n%s", err, data)
	}
}

func flashWindowsConsoleFixtureCommand(ctx context.Context, role, dir string) *exec.Cmd {
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFlashWindowsConsoleFixture$")
	command.Env = append(os.Environ(), "PIPKIN_FLASH_CONSOLE_FIXTURE="+role, "PIPKIN_FLASH_CONSOLE_DIR="+dir)
	return command
}

func TestFlashWindowsConsoleFixture(t *testing.T) {
	role := os.Getenv("PIPKIN_FLASH_CONSOLE_FIXTURE")
	if role == "" {
		t.Skip("subprocess fixture")
	}
	dir := os.Getenv("PIPKIN_FLASH_CONSOLE_DIR")
	if role != "driver" {
		if role != "tool" && role != "descendant" {
			t.Fatal("unknown console fixture role")
		}
		if role == "tool" {
			child := flashWindowsConsoleFixtureCommand(context.Background(), "descendant", dir)
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
		}
		pidPath := filepath.Join(dir, role+".pid")
		if err := os.WriteFile(pidPath+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(pidPath+".tmp", pidPath); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		return
	}

	// SSH and some test runners ignore Ctrl-C, and that attribute can survive
	// creation of this private console. Restore normal handling only in the
	// driver; runFlashProcess must still disable it for its new process group.
	setConsoleCtrlHandler := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")
	if ok, _, err := setConsoleCtrlHandler.Call(0, 0); ok == 0 {
		t.Fatalf("could not enable the isolated driver's Ctrl-C handling: %v", err)
	}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	parent, stopParent := context.WithCancel(context.Background())
	stopParent()
	// The real write boundary ignores an interactive cancellation, while each
	// tool call supplies its own finite deadline. Exercise both properties.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 8*time.Second)
	defer cancel()
	command := flashWindowsConsoleFixtureCommand(ctx, "tool", dir)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.WaitDelay = 2 * time.Second
	done := make(chan error, 1)
	go func() { done <- runFlashProcess(command) }()
	var processes []windows.Handle
	defer func() {
		for _, process := range processes {
			windows.CloseHandle(process)
		}
	}()
	readyUntil := time.Now().Add(5 * time.Second)
	for _, role := range []string{"tool", "descendant"} {
		for {
			data, err := os.ReadFile(filepath.Join(dir, role+".pid"))
			if err == nil {
				pid, err := strconv.ParseUint(string(data), 10, 32)
				if err != nil {
					t.Fatal(err)
				}
				process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
				if err != nil {
					t.Fatal(err)
				}
				processes = append(processes, process)
				break
			}
			select {
			case err := <-done:
				t.Fatalf("tool stopped before console interrupt: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			if time.Now().After(readyUntil) {
				t.Fatal("console fixture did not become ready")
			}
		}
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-interrupts:
	case <-time.After(time.Second):
		t.Fatal("isolated parent did not receive the console interrupt")
	}
	// Allow asynchronous console handlers to run before checking both members
	// of the tool's inherited process group.
	time.Sleep(250 * time.Millisecond)
	for _, process := range processes {
		state, err := windows.WaitForSingleObject(process, 0)
		if err != nil || state != uint32(windows.WAIT_TIMEOUT) {
			t.Fatalf("console Ctrl-C killed a protected tool process: state=%d, err=%v", state, err)
		}
	}
	select {
	case err := <-done:
		if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("tool did not remain bounded by its operation deadline: %v, %v", err, ctx.Err())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tool job survived its operation deadline")
	}
	for _, process := range processes {
		state, err := windows.WaitForSingleObject(process, 1000)
		if err != nil || state != windows.WAIT_OBJECT_0 {
			t.Fatalf("tool process escaped deadline cleanup: state=%d, err=%v", state, err)
		}
	}
}

func flashWindowsToolFixture(t *testing.T) string {
	t.Helper()
	if binary := os.Getenv("PIPKIN_TEST_ESPTOOL_PATH"); binary != "" {
		// A controlled native run may supply the cross-built fixture on a host
		// without Go or the repository. Production never reads this variable.
		info, err := os.Stat(binary)
		if !filepath.IsAbs(binary) || err != nil || !info.Mode().IsRegular() {
			t.Fatal("PIPKIN_TEST_ESPTOOL_PATH must name an existing absolute fixture file")
		}
		return binary
	}
	binary := filepath.Join(t.TempDir(), "fake-esptool.exe")
	build := exec.Command("go", "build", "-o", binary, "./testdata/esptool")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fixture build failed: %v\n%s", err, data)
	}
	return binary
}

func TestFlashWindowsJobCancellationAndCloseFallback(t *testing.T) {
	binary := flashWindowsToolFixture(t)
	for _, fallback := range []bool{false, true} {
		name := "terminate-job"
		if fallback {
			name = "kill-on-job-close"
		}
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "child-survived")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			command := exec.CommandContext(ctx, binary, "hang", marker)
			command.Stdout, command.Stderr = io.Discard, io.Discard
			command.WaitDelay = 2 * time.Second
			var injected atomic.Bool
			terminate := windows.TerminateJobObject
			if fallback {
				terminate = func(windows.Handle, uint32) error { injected.Store(true); return windows.ERROR_ACCESS_DENIED }
			}
			done := make(chan error, 1)
			go func() { done <- runFlashProcessInJob(command, terminate) }()
			deadline := time.After(10 * time.Second)
			for {
				if _, err := os.Stat(marker + ".ready"); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("contained helper did not start: %v", err)
				case <-deadline:
					t.Fatal("helper did not start its child")
				case <-time.After(10 * time.Millisecond):
				}
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled process succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Windows helper cancellation did not complete")
			}
			if fallback && !injected.Load() {
				t.Fatal("did not exercise explicit job termination failure")
			}
			time.Sleep(1100 * time.Millisecond)
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("child escaped the job after cancellation: %v", err)
			}
		})
	}
	t.Run("detached-parent-keeps-tool-timeout", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		tool := &flashTool{path: binary, output: io.Discard}
		start := time.Now()
		_, err := tool.run(context.WithoutCancel(parent), 200*time.Millisecond, "hang", filepath.Join(t.TempDir(), "deadline-survived"))
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
			t.Fatalf("detached operation lost its own timeout: %v", err)
		}
	})
	t.Run("parent-exit", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "orphan-survived")
		tool := &flashTool{path: binary, output: io.Discard}
		if _, err := tool.run(context.Background(), 5*time.Second, "orphan", marker); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond)
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("child survived normal parent exit: %v", err)
		}
	})
}
