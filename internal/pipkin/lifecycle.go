package pipkin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var (
	errAlreadyRunning = errors.New("the Pipkin helper is already running")
	errHelperStarting = errors.New("the Pipkin helper has not published its process identity yet")
)

type processIdentity struct {
	Started    string `json:"started"`
	Executable string `json:"executable"`
}

type pidRecord struct {
	PID int `json:"pid"`
	processIdentity
}

var inspectProcess = processInfo
var terminateProcess = terminate

func lockInstance() (*os.File, error) {
	lock, err := acquireFileLock(lockPath())
	if errors.Is(err, errLockBusy) {
		return nil, errAlreadyRunning
	}
	if err != nil {
		return nil, fmt.Errorf("could not lock the helper: %w", err)
	}
	return lock, nil
}

func publishPID() error {
	identity, err := inspectProcess(os.Getpid())
	if err != nil {
		return fmt.Errorf("could not identify the helper process: %w", err)
	}
	if err := writeJSON(pidPath(), pidRecord{os.Getpid(), identity}); err != nil {
		return fmt.Errorf("could not publish the helper process identity: %w", err)
	}
	return nil
}

func removePID() error { return removeIfExists(pidPath()) }

func runningPID() (int, error) {
	running, err := helperRunning()
	if err != nil || !running {
		return 0, err
	}
	record, err := readPIDRecord()
	if err != nil {
		return 0, err
	}
	if err := validatePID(record); err != nil {
		return 0, err
	}
	return record.PID, nil
}

func readPIDRecord() (pidRecord, error) {
	var record pidRecord
	data, err := os.ReadFile(pidPath())
	if errors.Is(err, os.ErrNotExist) {
		return record, errHelperStarting
	}
	if err != nil {
		return record, fmt.Errorf("could not read the helper process identity: %w", err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		// Legacy PID-only records still require executable verification before signaling.
		record.PID, err = strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return record, errors.New("invalid helper process identity; refusing to signal a process")
		}
	}
	if record.PID <= 0 {
		return record, errors.New("invalid helper PID; refusing to signal a process")
	}
	return record, nil
}

func validatePID(record pidRecord) error {
	actual, err := inspectProcess(record.PID)
	if err != nil {
		return fmt.Errorf("could not verify helper PID %d: %w", record.PID, err)
	}
	if record.Started == "" && record.Executable == "" {
		if sameExecutablePath(actual.Executable, installedBinary()) || sameExecutablePath(actual.Executable, serviceBinary()) {
			return nil
		}
	} else if record.Started != "" && actual.Started == record.Started && sameExecutablePath(actual.Executable, record.Executable) {
		return nil
	}
	return fmt.Errorf("PID %d does not match the helper process identity; refusing to signal it", record.PID)
}

func terminateHelper(pid int) error {
	record, err := readPIDRecord()
	if err != nil {
		return err
	}
	if record.PID != pid {
		return errors.New("helper process changed while stopping; retry the command")
	}
	if err := validatePID(record); err != nil {
		return err
	}
	return terminateProcess(pid)
}

func sameExecutablePath(a, b string) bool {
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
