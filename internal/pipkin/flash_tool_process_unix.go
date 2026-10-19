//go:build darwin || linux

package pipkin

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func runFlashProcess(command *exec.Cmd) error {
	// PyInstaller's executable may launch a child that owns the serial port.
	// Kill the entire process group when its deadline or parent is canceled.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := unix.Kill(-command.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return command.Run()
}

// esptool sets DTR and RTS together here, so failed attempts are not timing races.
const flashConnectAttempts = "2"

func (t *flashTool) Reset(ctx context.Context, port string) error {
	// Each POSIX esptool process opens with pyserial's DTR/RTS defaults. The
	// no-reset path leaves DTR asserted, and hard-reset only pulses RTS: GPIO0
	// then remains low when EN rises, selecting download mode again. Complete
	// the standard bootloader-entry sequence first; it explicitly releases DTR
	// before the final hard reset starts the application.
	args, err := toolPortArgs(port, "default-reset", "hard-reset", true)
	if err != nil {
		return err
	}
	// Before a successful probe, esptool detects the chip itself.
	if t.chip != "" {
		args = append(args, "--chip", t.chip)
	}
	args = append(args, "read-mac")
	_, err = t.run(ctx, 20*time.Second, args...)
	return err
}
