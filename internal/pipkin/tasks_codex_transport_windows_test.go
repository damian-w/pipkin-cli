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

func codexTransportTestNamedPipe(t *testing.T, extra string) (string, windows.Handle, *windows.SID) {
	t.Helper()
	sid, err := processUserSID(windows.CurrentProcess())
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + sid.String() + ")(A;;GA;;;SY)" + extra)
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
	name, server, sid := codexTransportTestNamedPipe(t, "")
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

func TestCodexTransportWindowsDefaultReadAccess(t *testing.T) {
	name, _, sid := codexTransportTestNamedPipe(t, "(A;;0x00120089;;;WD)(A;;0x00120089;;;AN)")
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
			name, _, sid := codexTransportTestNamedPipe(t, "(A;;"+rights+";;;WD)")
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
			name, _, sid := codexTransportTestNamedPipe(t, "(A;;GA;;;"+id.String()+")")
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
	name, _, _ := codexTransportTestNamedPipe(t, "")
	if conn, err := openCodexNamedPipe(context.Background(), name, validateCodexNamedPipePeer); err == nil || conn != nil {
		t.Fatalf("test process was accepted as Codex GUI: %v, %v", conn, err)
	}
}

func TestCodexTransportWindowsNativeReadCancellation(t *testing.T) {
	name, server, sid := codexTransportTestNamedPipe(t, "")
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
