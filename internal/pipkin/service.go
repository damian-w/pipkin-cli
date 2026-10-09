package pipkin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

var serviceCommand = runServiceCommand

func runServiceCommand(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("%s timed out: %w", name, ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("%s %s failed: %w%s", name, strings.Join(args, " "), err, commandDetail(output))
	}
	return nil
}

func commandDetail(output []byte) string {
	if detail := strings.TrimSpace(string(output)); detail != "" {
		return ": " + detail
	}
	return ""
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func helperRunning() (bool, error) {
	lock, err := lockInstance()
	if errors.Is(err, errAlreadyRunning) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, lock.Close()
}

func waitFor(condition func() (bool, error), timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ready, err := condition()
		if err != nil || ready {
			return err
		}
		if !time.Now().Before(deadline) {
			return errors.New("timed out waiting for the helper")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func startDetachedHelper() error {
	dir, err := resolvedAppDir()
	if err != nil {
		return err
	}
	return spawnDetached(serviceBinary(), "run", "--home", dir)
}

func startHelper() error {
	running, err := helperRunning()
	if err != nil {
		return err
	}
	if !running {
		if err := startService(); err != nil {
			return err
		}
	}
	return waitFor(helperReady, 6*time.Second)
}

func helperReady() (bool, error) {
	pid, err := runningPID()
	if errors.Is(err, errHelperStarting) {
		return false, nil
	}
	return pid != 0, err
}

func stopHelper() error {
	// Read and validate the record before asking a service manager to stop it.
	pid, err := runningPID()
	if err != nil {
		return err
	}
	serviceErr := stopService()
	var terminateErr error
	if pid != 0 {
		if current, err := runningPID(); err != nil {
			terminateErr = err
		} else if current == pid {
			terminateErr = terminateProcess(pid)
		}
	}
	waitErr := waitFor(func() (bool, error) {
		running, err := helperRunning()
		return !running, err
	}, 5*time.Second)
	return errors.Join(serviceErr, terminateErr, waitErr)
}
