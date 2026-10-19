package pipkin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

var errLockBusy = errors.New("another process holds the lock")

// Closing the returned file releases the lock. Keep the lock file in place so
// every process locks the same file even when protected files are replaced.
func acquireFileLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	locked, err := tryFileLock(file)
	if !locked {
		file.Close()
		if err == nil {
			err = errLockBusy
		}
		return nil, err
	}
	return file, nil
}

func waitFileLock(ctx context.Context, path string) (*os.File, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := acquireFileLock(path)
		if !errors.Is(err, errLockBusy) {
			return file, err
		}
		if err := sleepContext(ctx, 25*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

// sleepContext waits for d, returning early with the context's error.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
