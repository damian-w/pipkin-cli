package pipkin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

type statusView struct {
	installed bool
	starting  bool
	pid       int
	snapshot  Status
}

// statusRunningPID checks the helper lock without creating files.
func statusRunningPID() (int, error) {
	file, err := os.OpenFile(lockPath(), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()
	locked, err := tryFileLock(file)
	if err != nil || locked {
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

func loadStatusView() (statusView, error) {
	var view statusView
	dir, err := resolvedAppDir()
	if err != nil {
		return view, err
	}
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return view, nil
	}
	if err != nil {
		return view, fmt.Errorf("could not inspect Pipkin's settings folder: %w", err)
	}
	if !info.IsDir() {
		return view, errors.New("Pipkin's settings path is not a folder")
	}
	view.installed, err = pathExists(installedBinary())
	if err != nil {
		return view, fmt.Errorf("could not inspect the Pipkin installation: %w", err)
	}
	view.pid, err = statusRunningPID()
	if errors.Is(err, errHelperStarting) {
		view.starting = true
		return view, nil
	}
	if err != nil {
		return view, fmt.Errorf("could not confirm whether Pipkin is running: %w", err)
	}
	if view.pid == 0 {
		return view, nil
	}
	err = readJSON(statusPath(), &view.snapshot)
	if errors.Is(err, os.ErrNotExist) {
		view.starting = true
		return view, nil
	}
	if err != nil {
		return view, fmt.Errorf("could not read Pipkin's saved status: %w", err)
	}
	if view.snapshot.SchemaVersion != 1 || view.snapshot.PID <= 0 {
		return view, errors.New("Pipkin's saved status can't be read. Run 'pipkin restart'.")
	}
	if view.snapshot.PID != view.pid {
		view.starting = true
	}
	return view, nil
}

func statusCommandTo(args []string, out io.Writer) error {
	if len(args) == 1 {
		return statusJSONTo(out)
	}
	view, err := loadStatusView()
	if err != nil {
		return err
	}
	return writeStatus(out, view, time.Now().Unix(), runtime.GOOS)
}

func writeStatus(out io.Writer, view statusView, now int64, goos string) error {
	switch {
	case view.starting:
		return writeOutputLine(out, "", "Pipkin is starting up. Run 'pipkin status' again in a few seconds.")
	case view.pid == 0 && view.installed:
		return writeOutputLine(out, "", "Pipkin is stopped, so your display isn't updating. Run 'pipkin start'.")
	case view.pid == 0:
		return writeOutputLine(out, "", "Pipkin isn't installed. Run 'pipkin install' to set it up.")
	}
	device := view.snapshot.Device
	if device["port"] != "" {
		message := "Pipkin is running. Display connected."
		if firmware := device["firmware"]; firmware != "" {
			message = "Pipkin is running. Display connected (firmware " + firmware + ")."
		}
		if err := writeOutputLine(out, "", message); err != nil {
			return err
		}
	} else {
		if err := writeOutputLine(out, "", "Pipkin is running, but the display isn't connected."); err != nil {
			return err
		}
		if err := writeDisplayHint(out, device["error"], goos); err != nil {
			return err
		}
	}
	if err := writeOutputLine(out, "", ""); err != nil {
		return err
	}
	if err := writeProviders(out, view.snapshot, now, false); err != nil {
		return err
	}
	return writeTaskStatusSummary(out, view.snapshot.Tasks, now)
}

func writeTaskStatusSummary(out io.Writer, saved *TaskSnapshot, now int64) error {
	snapshot := readableTaskSnapshot(saved, true, time.Unix(now, 0))
	if snapshot == nil || snapshot.SchemaVersion != 1 {
		return nil
	}
	heading := false
	for _, provider := range taskProviders {
		summary := taskSummary(snapshot.Providers[provider.key])
		if summary == "" {
			continue
		}
		if !heading {
			if err := writeOutputLine(out, "", ""); err != nil {
				return err
			}
			if err := writeOutputLine(out, "", "Tasks"); err != nil {
				return err
			}
			heading = true
		}
		if err := writeOutputLine(out, "  ", fmt.Sprintf("%-11s  %s", provider.name, summary)); err != nil {
			return err
		}
	}
	return nil
}

func writeDisplayHint(out io.Writer, reason, goos string) error {
	// Snapshots keep only the error text, so match known OS messages.
	detail := reason
	if index := strings.LastIndex(detail, ": "); index >= 0 {
		detail = detail[index+2:]
	}
	lower := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(detail)), ".")
	busy := lower == "resource busy" || lower == "device or resource busy" || lower == "sharing violation" ||
		goos == "windows" && (lower == "access is denied" || lower == "the process cannot access the file because it is being used by another process")
	permission := lower == "permission denied" || lower == "operation not permitted"
	if reason != "" {
		if err := writeOutputLine(out, "  ", "Last connection attempt: "+reason); err != nil {
			return err
		}
	}
	switch {
	case busy:
		return writeOutputLine(out, "  ", "Another app may be using the display's port, such as a serial monitor. Close it and Pipkin will retry.")
	case permission:
		if err := writeOutputLine(out, "  ", "Pipkin couldn't open the display's port. Check its permissions."); err != nil {
			return err
		}
		if hint := serialPermissionHint(); hint != "" {
			return writeOutputLine(out, "  ", hint)
		}
		return nil
	default:
		return writeOutputLine(out, "  ", "Plug in the display with a USB data cable. For a new self-built display, run 'pipkin flash'.")
	}
}
