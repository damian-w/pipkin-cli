package pipkin

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestFlashWindowsJobCancellationAndCloseFallback(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-esptool.exe")
	build := exec.Command("go", "build", "-o", binary, "./testdata/esptool")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fixture build failed: %v\n%s", err, data)
	}
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
	t.Run("parent-exit", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "orphan-survived")
		tool := &flashTool{path: binary, output: io.Discard}
		if _, err := tool.run(context.Background(), 5*time.Second, false, "orphan", marker); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond)
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("child survived normal parent exit: %v", err)
		}
	})
}
