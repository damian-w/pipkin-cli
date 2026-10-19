package pipkin

import (
	"errors"
	"strconv"

	"golang.org/x/sys/windows"
)

func ownedProcessInfo(pid int) (processIdentity, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return processIdentity{}, err
	}
	defer windows.CloseHandle(process)
	owner, err := processUserSID(process)
	if err != nil {
		return processIdentity{}, err
	}
	current, err := processUserSID(windows.CurrentProcess())
	if err != nil || !owner.Equals(current) {
		return processIdentity{}, errors.New("process owner is unavailable")
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return processIdentity{}, err
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return processIdentity{}, err
	}
	started := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	return processIdentity{Started: strconv.FormatUint(started, 10), Executable: windows.UTF16ToString(buffer[:size])}, nil
}
