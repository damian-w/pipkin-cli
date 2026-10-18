package pipkin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
)

// A cancellation notice and the flashing tool can write at the same time. Keep
// each write intact and remember output failures without interrupting the board.
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
			_ = writeOutputLine(output, "", "Finishing the firmware write; please keep the cable plugged in.")
		})
	}
	stop := context.AfterFunc(ctx, notice)
	var finishOnce sync.Once
	return func() {
		finishOnce.Do(func() {
			if !stop() || ctx.Err() != nil {
				// Do also waits for an already-running notice, so no writer
				// goroutine survives the operation or races its caller's result.
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
		heading = fmt.Sprintf("%s your Pipkin display from %s to %s?", plan.action, plan.installed, plan.selected)
		prompt = plan.action + " now? [y/N] "
	case "Reinstall":
		heading = "Reinstall Pipkin " + plan.selected + " on your display?"
		prompt = "Reinstall now? [y/N] "
	default:
		heading = "Install Pipkin " + plan.selected + " on this CYD display?"
		prompt = "Confirm this is a CYD and install Pipkin? [y/N] "
	}
	if err := writeOutputLine(output, "", ""); err != nil {
		return "", err
	}
	if err := writeOutputLine(output, "", heading); err != nil {
		return "", err
	}
	if plan.stored {
		if err := writeOutputLine(output, "", "Current: Pipkin "+plan.installed+" (stored; not responding)"); err != nil {
			return "", err
		}
	}
	if plan.action == "Install" {
		board := "CYD, 2.8-inch touch display"
		if plan.boardName != "" {
			board = plan.boardName
		}
		for _, line := range []string{"Board: " + board, "Port: " + plan.port, "This replaces the board's existing firmware and settings."} {
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
	cause         error
	port, version string
}

func (failure *flashWriteFailure) Unwrap() error { return failure.cause }

func (failure *flashWriteFailure) Error() string {
	var output bytes.Buffer
	for _, line := range []string{
		"The firmware write didn't finish.",
		"The display may stay blank until the firmware is reinstalled.",
		"", "To fix it:",
		"  1. Unplug the display, wait a few seconds, and plug it back in.",
	} {
		_ = writeOutputLine(&output, "", line)
	}
	_ = writeOutputLine(&output, "  ", "2. Run this command:")
	// A shell command must stay on one physical line. Wrapping it as prose
	// would turn its remaining options into a separate command when pasted.
	_, _ = io.WriteString(&output, "     "+flashRecoveryCommand(failure.port, failure.version, runtime.GOOS)+"\n")
	_ = writeOutputLine(&output, "", "There is no automatic rollback.")
	_ = writeOutputLine(&output, "", "")
	_ = writeOutputLine(&output, "Details: ", failure.cause.Error())
	return strings.TrimSuffix(output.String(), "\n")
}

func flashRecoveryCommand(port, version, goos string) string {
	return "pipkin flash --port " + flashRecoveryArgument(port, goos) + " --version " + flashRecoveryArgument(version, goos) + " --reinstall"
}

func flashRecoveryArgument(value, goos string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./:-", r))
	}) < 0 {
		return value
	}
	if goos == "windows" {
		return powerShellQuote(value)
	}
	return shellQuote(value)
}
