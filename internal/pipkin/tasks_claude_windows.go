package pipkin

import (
	"errors"
	"strconv"

	"golang.org/x/sys/windows"
)

func claudeTaskProcessInfo(pid int) (processIdentity, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return processIdentity{}, err
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return processIdentity{}, err
	}
	defer token.Close()
	owner, err := token.GetTokenUser()
	if err != nil {
		return processIdentity{}, err
	}
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || !windows.EqualSid(owner.User.Sid, current.User.Sid) {
		return processIdentity{}, errors.New("Claude task process owner is unavailable")
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
