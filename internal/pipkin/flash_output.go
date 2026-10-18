package pipkin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// flashOutput serializes the Ctrl-C notice with tool output and remembers the
// first write failure without interrupting the board.
type flashOutput struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

func (out *flashOutput) Write(data []byte) (int, error) {
	out.mu.Lock()
	defer out.mu.Unlock()
	if out.err != nil {
		return 0, out.err
	}
	n, err := out.w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	out.err = err
	return n, err
}

func (out *flashOutput) failure() error {
	out.mu.Lock()
	defer out.mu.Unlock()
	return out.err
}

func flashCancellationNotice(ctx context.Context, output io.Writer) func() {
	var once sync.Once
	notice := func() {
		once.Do(func() {
			_ = writeOutputLine(output, "", "Finishing the firmware write. Keep the cable plugged in.")
		})
	}
	stop := context.AfterFunc(ctx, notice)
	var finishOnce sync.Once
	return func() {
		finishOnce.Do(func() {
			if !stop() || ctx.Err() != nil {
				// Waits for a notice already being written.
				notice()
			}
		})
	}
}

type flashPlan struct {
	action, installed, selected string
	port, boardName, profile    string
	stored                      bool
	firmwareProfiles            int
}

func writeFlashPlan(output io.Writer, plan flashPlan) (string, error) {
	var heading, prompt string
	switch plan.action {
	case "Update", "Downgrade":
		heading = fmt.Sprintf("Ready to %s your display from Pipkin %s to %s.", strings.ToLower(plan.action), plan.installed, plan.selected)
		prompt = plan.action + " now? [y/N] "
	case "Reinstall":
		heading = "Ready to reinstall Pipkin " + plan.selected + " on your display."
		prompt = "Reinstall now? [y/N] "
	default:
		heading = "Ready to install Pipkin " + plan.selected + "."
		prompt = "Confirm this is a CYD and install Pipkin? [y/N] "
	}
	if err := writeOutputLine(output, "", ""); err != nil {
		return "", err
	}
	if err := writeOutputLine(output, "", heading); err != nil {
		return "", err
	}
	if plan.stored {
		if err := writeOutputLine(output, "", "Current: Pipkin "+plan.installed+" (installed but not responding)"); err != nil {
			return "", err
		}
	}
	if plan.action == "Install" {
		board := "CYD, 2.8-inch touch display"
		if plan.boardName != "" {
			board = plan.boardName
		}
		for _, line := range []string{"Board: " + board, "Port: " + plan.port, "This replaces the board's current firmware and settings."} {
			if err := writeOutputLine(output, "", line); err != nil {
				return "", err
			}
		}
		if plan.profile == "" && plan.firmwareProfiles > 1 {
			if err := writeOutputLine(output, "", "The physical board profile is unconfirmed. Inspect the PCB and use 'pipkin identify --board PROFILE' to remember it."); err != nil {
				return "", err
			}
		}
	} else if err := writeOutputLine(output, "", "Your settings are kept."); err != nil {
		return "", err
	}
	return prompt, nil
}

type flashWriteFailure struct {
	cause   error
	written bool
	command string
}

func (failure *flashWriteFailure) Unwrap() error { return failure.cause }

func (failure *flashWriteFailure) Error() string {
	headline := "The firmware write didn't finish."
	if failure.written {
		headline = "The firmware was written, but Pipkin couldn't confirm it started."
	}
	var output bytes.Buffer
	for _, line := range []string{headline, "The display may stay blank until the firmware is reinstalled.", "", "To fix it:"} {
		_ = writeOutputLine(&output, "", line)
	}
	_ = writeOutputLine(&output, "  ", "1. Unplug the display, wait a few seconds, and plug it back in.")
	_ = writeOutputLine(&output, "  ", "2. Run this command:")
	// Keep the command on one line so it can be pasted.
	_, _ = io.WriteString(&output, "     "+failure.command+"\n\n")
	_ = writeOutputLine(&output, "Details: ", failure.cause.Error())
	return strings.TrimSuffix(output.String(), "\n")
}
