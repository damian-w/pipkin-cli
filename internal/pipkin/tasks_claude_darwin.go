package pipkin

import (
	"bytes"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

var claudeTaskDarwinNative struct {
	sync.Once
	path func(int32, *byte, uint32) int32
	err  error
}

func claudeTaskProcessInfo(pid int) (processIdentity, error) {
	claudeTaskDarwinNative.Do(func() {
		library, err := purego.Dlopen("/usr/lib/libproc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			claudeTaskDarwinNative.err = err
			return
		}
		address, err := purego.Dlsym(library, "proc_pidpath")
		if err != nil {
			claudeTaskDarwinNative.err = err
			return
		}
		purego.RegisterFunc(&claudeTaskDarwinNative.path, address)
	})
	if claudeTaskDarwinNative.err != nil {
		return processIdentity{}, claudeTaskDarwinNative.err
	}
	before, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return processIdentity{}, err
	}
	if before.Proc.P_pid != int32(pid) || before.Eproc.Ucred.Uid != uint32(os.Getuid()) {
		return processIdentity{}, errors.New("Claude task process owner is unavailable")
	}
	// proc_pidpath reads only the executable path. It avoids fetching process
	// arguments and environment variables through kern.procargs2.
	buffer := make([]byte, 4096)
	n := claudeTaskDarwinNative.path(int32(pid), &buffer[0], uint32(len(buffer)))
	if n <= 0 || int(n) >= len(buffer) {
		return processIdentity{}, errors.New("Claude task executable is unavailable")
	}
	after, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || after.Proc.P_starttime != before.Proc.P_starttime || after.Eproc.Ucred.Uid != before.Eproc.Ucred.Uid {
		return processIdentity{}, errors.New("Claude task process changed")
	}
	started := time.Unix(before.Proc.P_starttime.Sec, 0).UTC().Format("Mon Jan _2 15:04:05 2006")
	return processIdentity{Started: started, Executable: string(bytes.TrimRight(buffer[:n], "\x00"))}, nil
}
