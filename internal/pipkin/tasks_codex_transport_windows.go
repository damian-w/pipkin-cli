package pipkin

import (
	"context"
	"errors"
	"net"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const codexDesktopPipe = `\\.\pipe\codex-ipc`

func openCodexDesktopIPC(ctx context.Context) (net.Conn, error) {
	return openCodexNamedPipe(ctx, codexDesktopPipe, validateCodexNamedPipePeer)
}

type codexPipeAddr struct{}

func (codexPipeAddr) Network() string { return "pipe" }
func (codexPipeAddr) String() string  { return "codex-local-ipc" }

type codexPipeConn struct{ *os.File }

func (c *codexPipeConn) LocalAddr() net.Addr                { return codexPipeAddr{} }
func (c *codexPipeConn) RemoteAddr() net.Addr               { return codexPipeAddr{} }
func (c *codexPipeConn) SetDeadline(t time.Time) error      { return c.File.SetDeadline(t) }
func (c *codexPipeConn) SetReadDeadline(t time.Time) error  { return c.File.SetReadDeadline(t) }
func (c *codexPipeConn) SetWriteDeadline(t time.Time) error { return c.File.SetWriteDeadline(t) }

func openCodexNamedPipe(ctx context.Context, name string, validate func(windows.Handle) error) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, errors.New("Codex local pipe is unavailable")
	}
	// CreateFile returns immediately for an absent/busy pipe. Never use the
	// server's default WaitNamedPipe timeout, which can exceed collection bounds.
	handle, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
		0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		return nil, errors.New("Codex local pipe is unavailable")
	}
	if err := validate(handle); err != nil {
		windows.CloseHandle(handle)
		return nil, errors.New("Codex local peer could not be verified")
	}
	file := os.NewFile(uintptr(handle), "Codex local IPC")
	if file == nil {
		windows.CloseHandle(handle)
		return nil, errors.New("Codex local pipe is unavailable")
	}
	// Overlapped handles are registered with Go's native poller by os.NewFile,
	// which provides cancellation and deadlines without a worker per I/O call.
	return boundCodexIPCConn(ctx, &codexPipeConn{file})
}

func codexProcessSID(process windows.Handle) (*windows.SID, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}

func validateCodexNamedPipePeer(handle windows.Handle) error {
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(handle, &pid); err != nil || pid == 0 {
		return errors.New("Codex pipe server identity is unavailable")
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return errors.New("Codex pipe server process query is unavailable")
	}
	defer windows.CloseHandle(process)
	serverSID, err := codexProcessSID(process)
	if err != nil {
		return errors.New("Codex pipe server user is unavailable")
	}
	currentSID, err := codexProcessSID(windows.CurrentProcess())
	if err != nil {
		return errors.New("Codex pipe current user is unavailable")
	}
	if !serverSID.Equals(currentSID) {
		return errors.New("Codex pipe belongs to another user")
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return errors.New("Codex GUI executable query is unavailable")
	}
	if !codexGUIExecutable(windows.UTF16ToString(buffer[:size]), "windows") {
		return errors.New("Codex GUI process could not be verified")
	}
	serverLogonSIDs, err := codexProcessLogonSIDs(process)
	if err != nil {
		return errors.New("Codex pipe server logon is unavailable")
	}
	return validateCodexPipeDACL(handle, currentSID, serverLogonSIDs...)
}

func codexProcessLogonSIDs(process windows.Handle) ([]*windows.SID, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	groups, err := token.GetTokenGroups()
	if err != nil {
		return nil, err
	}
	var ids []*windows.SID
	for _, group := range groups.AllGroups() {
		if group.Attributes&windows.SE_GROUP_LOGON_ID != windows.SE_GROUP_LOGON_ID {
			continue
		}
		id, err := group.Sid.Copy()
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func validateCodexPipeDACL(handle windows.Handle, currentSID *windows.SID, serverLogonSIDs ...*windows.SID) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return errors.New("Codex pipe access descriptor is unavailable")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount > 128 {
		return errors.New("Codex pipe access is not restricted")
	}
	logonSIDs, err := codexProcessLogonSIDs(windows.CurrentProcess())
	if err != nil {
		return errors.New("Codex pipe current groups are unavailable")
	}
	logonSIDs = append(logonSIDs, serverLogonSIDs...)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return errors.New("Codex pipe access entry is unavailable")
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("Codex pipe access could not be verified")
		}
		access := uint32(ace.Mask)
		// Windows' default pipe ACL grants Everyone/Anonymous read access.
		// Only registered clients receive messages, and registration requires
		// writing initialize. Reject foreign injection or ACL/instance control.
		// https://learn.microsoft.com/en-us/windows/win32/ipc/named-pipe-security-and-access-rights
		const modify = windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.FILE_WRITE_DATA |
			windows.FILE_APPEND_DATA | windows.WRITE_DAC | windows.WRITE_OWNER | windows.DELETE |
			windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA
		if access&modify == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		trusted := sid.IsValid() && (sid.Equals(currentSID) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
		for _, logonSID := range logonSIDs {
			trusted = trusted || sid.Equals(logonSID)
		}
		if !trusted {
			return errors.New("Codex pipe allows another user to modify task data")
		}
	}
	return nil
}
