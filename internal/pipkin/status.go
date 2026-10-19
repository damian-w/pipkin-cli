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
	view.pid, err = runningPID()
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

func statusJSONTo(out io.Writer) error {
	var status Status
	if err := readJSON(statusPath(), &status); err != nil {
		return errors.New("no helper snapshot yet; run `pipkin usage --json` for a fresh read")
	}
	pid, err := runningPID()
	if err != nil {
		return err
	}
	status.PID = pid
	status.Tasks = readableTaskSnapshot(status.Tasks, pid != 0, time.Now())
	return printJSONTo(out, status)
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
	lines := lineOutput{w: out}
	device := view.snapshot.Device
	if device["port"] != "" {
		display := "Display"
		if board, ok := boardByID(device["board"]); ok {
			display = board.Screen + " display"
		}
		message := "Pipkin is running. " + display + " connected."
		if firmware := device["firmware"]; firmware != "" {
			message = "Pipkin is running. " + display + " connected (firmware " + firmware + ")."
		}
		lines.line("", message)
	} else {
		lines.line("", "Pipkin is running, but the display isn't connected.")
		if lines.err == nil {
			lines.err = writeDisplayHint(out, device["error"], goos)
		}
	}
	lines.line("", "")
	if lines.err == nil {
		lines.err = writeProviders(out, view.snapshot, now, false)
	}
	if lines.err == nil {
		lines.err = writeTaskStatusSummary(out, view.snapshot.Tasks, now)
	}
	return lines.err
}

func writeTaskStatusSummary(out io.Writer, saved *TaskSnapshot, now int64) error {
	snapshot := readableTaskSnapshot(saved, true, time.Unix(now, 0))
	if snapshot == nil || snapshot.SchemaVersion != 1 {
		return nil
	}
	lines, heading := lineOutput{w: out}, false
	for _, provider := range taskProviders {
		summary := taskSummary(snapshot.Providers[provider.key])
		if summary == "" {
			continue
		}
		if !heading {
			lines.line("", "")
			lines.line("", "Tasks")
			heading = true
		}
		lines.line("  ", fmt.Sprintf("%-11s  %s", provider.name, summary))
	}
	return lines.err
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
	lines := lineOutput{w: out}
	if reason != "" {
		lines.line("  ", "Last connection attempt: "+reason)
	}
	switch {
	case busy:
		lines.line("  ", "Another app may be using the display's port, such as a serial monitor. Close it and Pipkin will retry.")
	case permission:
		lines.line("  ", "Pipkin couldn't open the display's port. Check its permissions.")
		if hint := serialPermissionHint(); hint != "" {
			lines.line("  ", hint)
		}
	default:
		lines.line("  ", "Plug in the display with a USB data cable. If it's plugged in, run 'pipkin flash' to install or update its firmware.")
	}
	return lines.err
}
