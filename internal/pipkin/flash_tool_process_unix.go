//go:build darwin || linux

package pipkin

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

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
