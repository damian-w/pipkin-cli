//go:build darwin || linux

package pipkin

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func spawnDetached(binary string, args ...string) error {
	command := exec.Command(binary, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command.Start()
}

func terminate(pid int) error { return unix.Kill(pid, unix.SIGTERM) }
