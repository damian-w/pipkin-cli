//go:build darwin || linux

package pipkin

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSerialWriteCancellationOnFullPipe(t *testing.T) {
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	if err := unix.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 4096)
	for {
		if _, err := unix.Write(fds[1], block); errors.Is(err, unix.EAGAIN) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := (&unixPort{fd: fds[1]}).Write(ctx, block)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("blocked serial write did not honor parent deadline: %v", err)
	}
}
