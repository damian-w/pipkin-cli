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
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
