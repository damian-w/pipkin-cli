package pipkin

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func spawnDetached(binary string, args ...string) error {
	command := exec.Command(binary, args...)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.DETACHED_PROCESS |
		windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW}
	return command.Start()
}

func terminate(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
