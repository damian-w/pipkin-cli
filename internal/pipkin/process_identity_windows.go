package pipkin

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func processInfo(pid int) (processIdentity, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return processIdentity{}, err
	}
	defer windows.CloseHandle(process)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return processIdentity{}, err
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return processIdentity{}, err
	}
	return processIdentity{Started: fmt.Sprintf("%d:%d", created.HighDateTime, created.LowDateTime), Executable: windows.UTF16ToString(buffer[:size])}, nil
}
