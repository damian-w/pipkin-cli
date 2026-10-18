package pipkin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func codexTransportTestNamedPipe(t *testing.T, worldAccess bool) (string, windows.Handle, *windows.SID) {
	t.Helper()
	extra := ""
	if worldAccess {
		extra = "(A;;GA;;;WD)"
	}
	return codexTransportTestNamedPipeAccess(t, extra)
}

func codexTransportTestNamedPipeAccess(t *testing.T, extra string) (string, windows.Handle, *windows.SID) {
	t.Helper()
	sid, err := codexProcessSID(windows.CurrentProcess())
	if err != nil {
		t.Fatal(err)
	}
	dacl := "D:P(A;;GA;;;" + sid.String() + ")(A;;GA;;;SY)"
	dacl += extra
	sd, err := windows.SecurityDescriptorFromString(dacl)
	if err != nil {
		t.Fatal(err)
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name := fmt.Sprintf(`\\.\pipe\pipkin-codex-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	server, err := windows.CreateNamedPipe(path, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 4096, 4096, 0, &sa)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(server) })
	return name, server, sid
}

func codexTransportConnectTestServer(t *testing.T, server windows.Handle) {
	t.Helper()
	if err := windows.ConnectNamedPipe(server, nil); err != nil && err != windows.ERROR_PIPE_CONNECTED {
		t.Fatal(err)
	}
}

func TestCodexTransportWindowsOverlappedPipeRoundTrip(t *testing.T) {
	name, server, sid := codexTransportTestNamedPipe(t, false)
	conn, err := openCodexNamedPipe(context.Background(), name, func(handle windows.Handle) error {
		var pid uint32
		if err := windows.GetNamedPipeServerProcessId(handle, &pid); err != nil {
			return err
		}
		if pid != uint32(os.Getpid()) {
			return errors.New("wrong synthetic pipe server")
		}
		return validateCodexPipeDACL(handle, sid)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	codexTransportConnectTestServer(t, server)
	if _, err := conn.Write([]byte("test")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4)
	var count uint32
	if err := windows.ReadFile(server, buffer, &count, nil); err != nil || count != 4 || string(buffer) != "test" {
		t.Fatalf("native server read: %q (%d), %v", buffer, count, err)
	}
	if err := windows.WriteFile(server, []byte("okay"), &count, nil); err != nil || count != 4 {
		t.Fatalf("native server write: %d, %v", count, err)
	}
	if n, err := conn.Read(buffer); err != nil || n != 4 || string(buffer) != "okay" {
		t.Fatalf("native client read: %q (%d), %v", buffer, n, err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(buffer); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("overlapped read did not time out: %v", err)
	}
}

func TestCodexTransportWindowsRejectsOtherUserPipeAccess(t *testing.T) {
	name, _, sid := codexTransportTestNamedPipe(t, true)
	if conn, err := openCodexNamedPipe(context.Background(), name, func(handle windows.Handle) error {
		return validateCodexPipeDACL(handle, sid)
	}); err == nil || conn != nil {
		t.Fatalf("accepted public synthetic pipe: %v, %v", conn, err)
	}
}

func TestCodexTransportWindowsDefaultReadAccess(t *testing.T) {
	name, _, sid := codexTransportTestNamedPipeAccess(t, "(A;;0x00120089;;;WD)(A;;0x00120089;;;AN)")
	conn, err := openCodexNamedPipe(context.Background(), name, func(handle windows.Handle) error {
		return validateCodexPipeDACL(handle, sid)
	})
	if err != nil {
		t.Fatal("normal Windows read-only ACL was rejected")
	}
	conn.Close()
}

func TestCodexTransportWindowsRejectsForeignModification(t *testing.T) {
	for _, rights := range []string{
		"GW", "GA", "0x00000002", // Write data.
		"0x00000004",     // Append data is FILE_CREATE_PIPE_INSTANCE.
		"WD", "WO", "SD", // Change ACL, take ownership, delete.
		"0x00000010", "0x00000100", // Write attributes/extended attributes.
	} {
		t.Run(rights, func(t *testing.T) {
			name, _, sid := codexTransportTestNamedPipeAccess(t, "(A;;"+rights+";;;WD)")
			if conn, err := openCodexNamedPipe(context.Background(), name, func(handle windows.Handle) error {
				return validateCodexPipeDACL(handle, sid)
			}); err == nil || conn != nil {
				t.Fatalf("foreign modification rights accepted: %s", rights)
			}
		})
	}
}

func TestCodexTransportWindowsLogonTrustIsExplicit(t *testing.T) {
	// This synthetic logon is never inferred trustworthy from its SID shape.
	// Production adds only logons obtained from the already verified server.
	id, err := windows.StringToSid("S-1-5-5-111-222")
	if err != nil {
		t.Fatal(err)
	}
	for _, verified := range []bool{false, true} {
		t.Run(fmt.Sprint(verified), func(t *testing.T) {
			name, _, sid := codexTransportTestNamedPipeAccess(t, "(A;;GA;;;"+id.String()+")")
			conn, err := openCodexNamedPipe(context.Background(), name, func(handle windows.Handle) error {
				if verified {
					return validateCodexPipeDACL(handle, sid, id)
				}
				return validateCodexPipeDACL(handle, sid)
			})
			if conn != nil {
				conn.Close()
			}
			if (err == nil) != verified {
				t.Fatalf("verified=%t yielded success=%t", verified, err == nil)
			}
		})
	}
}

func TestCodexTransportWindowsRejectsUnrelatedServer(t *testing.T) {
	name, _, _ := codexTransportTestNamedPipe(t, false)
	if conn, err := openCodexNamedPipe(context.Background(), name, validateCodexNamedPipePeer); err == nil || conn != nil {
		t.Fatalf("test process was accepted as Codex GUI: %v, %v", conn, err)
	}
}

func TestCodexTransportWindowsNativeReadCancellation(t *testing.T) {
	name, server, sid := codexTransportTestNamedPipe(t, false)
	conn, err := openCodexNamedPipe(context.Background(), name, func(handle windows.Handle) error { return validateCodexPipeDACL(handle, sid) })
	if err != nil {
		t.Fatal(err)
	}
	codexTransportConnectTestServer(t, server)
	done := make(chan error, 1)
	go func() { _, err := conn.Read(make([]byte, 1)); done <- err }()
	conn.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed named pipe read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("native named pipe read ignored close")
	}
}

func TestCodexTransportLive(t *testing.T) {
	if os.Getenv("PIPKIN_LIVE_TEST_TASKS") != "1" {
		t.Skip("requires explicit local GUI task validation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := openCodexDesktopIPC(ctx)
	if err == nil {
		conn.Close()
		return
	}
	// Recheck only native connection/identity stages. No initialize, protocol
	// messages, conversation data, process arguments or environment are read.
	path, _ := windows.UTF16PtrFromString(codexDesktopPipe)
	handle, openErr := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
		0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if openErr != nil {
		t.Fatal("native Codex transport stage: pipe_open_unavailable")
	}
	defer windows.CloseHandle(handle)
	logCodexLivePipeAccess(t, handle)
	if peerErr := validateCodexNamedPipePeer(handle); peerErr != nil {
		// Validator errors are fixed stage descriptions, never native error
		// strings, paths, process IDs or user identifiers.
		t.Fatalf("native Codex transport stage: %s", peerErr.Error())
	}
	t.Fatal("native Codex transport stage: deadline_or_context_unavailable")
}

func logCodexLivePipeAccess(t *testing.T, handle windows.Handle) {
	t.Helper()
	currentSID, err := codexProcessSID(windows.CurrentProcess())
	if err != nil {
		t.Log("native Codex ACL summary: current_user_unavailable")
		return
	}
	logonIDs := func(process windows.Handle) []*windows.SID {
		var token windows.Token
		if windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token) != nil {
			return nil
		}
		defer token.Close()
		groups, err := token.GetTokenGroups()
		if err != nil {
			return nil
		}
		var ids []*windows.SID
		for _, group := range groups.AllGroups() {
			if group.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID {
				copy, err := group.Sid.Copy()
				if err == nil {
					ids = append(ids, copy)
				}
			}
		}
		return ids
	}
	currentLogon := logonIDs(windows.CurrentProcess())
	var serverLogon []*windows.SID
	var serverPID uint32
	if windows.GetNamedPipeServerProcessId(handle, &serverPID) == nil && serverPID != 0 {
		process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, serverPID)
		if err == nil {
			serverSID, err := codexProcessSID(process)
			if err == nil && currentSID.Equals(serverSID) {
				serverLogon = logonIDs(process)
			}
			windows.CloseHandle(process)
		}
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Log("native Codex ACL summary: access_descriptor_unavailable")
		return
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount > 128 {
		t.Log("native Codex ACL summary: access_entries_unavailable")
		return
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil {
			t.Log("native Codex ACL summary: access_entry_unavailable")
			return
		}
		category := "unsupported_ace"
		if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			category = "other"
			if sid.IsValid() {
				switch {
				case sid.Equals(currentSID):
					category = "current_user"
				case sid.IsWellKnown(windows.WinLocalSystemSid):
					category = "system"
				case sid.IsWellKnown(windows.WinBuiltinAdministratorsSid):
					category = "administrators"
				case sid.IsWellKnown(windows.WinWorldSid):
					category = "everyone"
				case sid.IsWellKnown(windows.WinAnonymousSid):
					category = "anonymous"
				case sid.IsWellKnown(windows.WinAuthenticatedUserSid):
					category = "authenticated_users"
				case sid.IsWellKnown(windows.WinBuiltinUsersSid):
					category = "users"
				}
				for _, id := range currentLogon {
					if sid.Equals(id) {
						category = "current_logon"
					}
				}
				for _, id := range serverLogon {
					if sid.Equals(id) {
						category = "verified_server_logon"
					}
				}
			}
		}
		t.Logf("native Codex ACL summary: category=%s type=%d flags=0x%02x mask=0x%08x", category, ace.Header.AceType, ace.Header.AceFlags, uint32(ace.Mask))
	}
}
