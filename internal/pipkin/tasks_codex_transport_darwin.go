package pipkin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func openCodexDesktopIPC(ctx context.Context) (net.Conn, error) {
	home, err := codexTaskHome()
	if err != nil {
		return nil, err
	}
	return openCodexUnixIPC(ctx, filepath.Join(home, "ipc", "ipc.sock"), validateCodexUnixPeer)
}

func codexUnixSocketPathSafe(path string) bool {
	for _, p := range []string{filepath.Dir(path), path} {
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			return false
		}
		identity, ok := info.Sys().(*syscall.Stat_t)
		if !ok || identity.Uid != uint32(os.Getuid()) {
			return false
		}
		if p == path {
			if info.Mode()&os.ModeSocket == 0 {
				return false
			}
		} else if !info.IsDir() {
			return false
		}
	}
	return true
}

func openCodexUnixIPC(ctx context.Context, path string, validate func(*net.UnixConn) error) (net.Conn, error) {
	if !codexUnixSocketPathSafe(path) {
		return nil, errors.New("Codex local socket is unavailable or unsafe")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, errors.New("Codex local connection is unavailable")
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok || !codexUnixSocketPathSafe(path) || validate(unixConn) != nil {
		conn.Close()
		return nil, errors.New("Codex local peer could not be verified")
	}
	return conn, nil
}

func validateCodexUnixPeer(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peerPID int
	var peerUID uint32
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		var credential *unix.Xucred
		credential, credentialErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if credentialErr != nil {
			return
		}
		peerUID = credential.Uid
		peerPID, credentialErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return err
	}
	if credentialErr != nil || peerPID <= 0 || peerUID != uint32(os.Getuid()) {
		return errors.New("unexpected Codex local peer")
	}
	info, err := ownedProcessInfo(peerPID)
	if err != nil || !codexGUIExecutable(info.Executable, "darwin") {
		return errors.New("Codex GUI process could not be verified")
	}
	return nil
}
