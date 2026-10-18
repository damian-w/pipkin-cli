package pipkin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func codexTransportTestSocket(t *testing.T) (string, *net.UnixListener) {
	t.Helper()
	// Unix socket paths are short; testing.TempDir's nested default can exceed
	// Darwin's sockaddr_un limit when the complete test name is included.
	dir, err := os.MkdirTemp("", "pipkin-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "ipc.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, listener
}

func TestCodexTransportDarwinRejectsUnsafeSocket(t *testing.T) {
	path, _ := codexTransportTestSocket(t)
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	called := false
	conn, err := openCodexUnixIPC(context.Background(), path, func(*net.UnixConn) error { called = true; return nil })
	if err == nil || conn != nil || called {
		t.Fatalf("unsafe socket connected: %v, %v, validated=%t", conn, err, called)
	}
}

func TestCodexTransportDarwinRejectsSymlink(t *testing.T) {
	path, _ := codexTransportTestSocket(t)
	alias := filepath.Join(filepath.Dir(path), "alias.sock")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if codexUnixSocketPathSafe(alias) {
		t.Fatal("accepted symlink as IPC socket")
	}
}

func TestCodexTransportDarwinRejectsUnrelatedServer(t *testing.T) {
	path, _ := codexTransportTestSocket(t)
	if conn, err := openCodexUnixIPC(context.Background(), path, validateCodexUnixPeer); err == nil || conn != nil {
		t.Fatalf("test process was accepted as Codex GUI: %v, %v", conn, err)
	}
}

func TestCodexTransportDarwinValidatorFailureCloses(t *testing.T) {
	path, listener := codexTransportTestSocket(t)
	var accepted *net.UnixConn
	done := make(chan error, 1)
	go func() {
		var err error
		accepted, err = listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		defer accepted.Close()
		accepted.SetReadDeadline(time.Now().Add(time.Second))
		_, err = accepted.Read(make([]byte, 1))
		done <- err
	}()
	if conn, err := openCodexUnixIPC(context.Background(), path, func(*net.UnixConn) error { return errors.New("reject") }); err == nil || conn != nil {
		t.Fatal("rejected peer returned a usable connection")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("rejected peer received data")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rejected connection remained open")
	}
}

func TestCodexTransportDarwinConnectionDeadline(t *testing.T) {
	path, _ := codexTransportTestSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	conn, err := openCodexUnixIPC(ctx, path, func(*net.UnixConn) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("inactive socket ignored read deadline")
	}
}
