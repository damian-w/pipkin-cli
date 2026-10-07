package pipkin

import (
	"bytes"
	"fmt"

	"golang.org/x/sys/unix"
)

func processInfo(pid int) (processIdentity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return processIdentity{}, err
	}
	args, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return processIdentity{}, err
	}
	if len(args) < 5 {
		return processIdentity{}, fmt.Errorf("incomplete process information for PID %d", pid)
	}
	executable, _, ok := bytes.Cut(args[4:], []byte{0})
	if !ok || len(executable) == 0 {
		return processIdentity{}, fmt.Errorf("invalid process executable for PID %d", pid)
	}
	started := info.Proc.P_starttime
	return processIdentity{Started: fmt.Sprintf("%d:%d", started.Sec, started.Usec), Executable: string(executable)}, nil
}
