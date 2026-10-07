//go:build darwin || linux

package pipkin

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

type unixPort struct{ fd int }

func openSerial(name string) (serialPort, error) {
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (serialPort, error) {
		unix.Close(fd)
		return nil, err
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCEXCL, 0); errno != 0 {
		return fail(errno)
	}
	termios, err := unix.IoctlGetTermios(fd, termiosGet)
	if err != nil {
		return fail(err)
	}
	// Raw 8N1 without HUPCL, so closing the port does not drop the modem lines.
	termios.Iflag = 0
	termios.Oflag = 0
	termios.Lflag = 0
	termios.Cflag = unix.CS8 | unix.CREAD | unix.CLOCAL
	termios.Cc[unix.VMIN] = 0
	termios.Cc[unix.VTIME] = 0
	setBaud115200(termios)
	if err := unix.IoctlSetTermios(fd, termiosSet, termios); err != nil {
		return fail(err)
	}
	return &unixPort{fd}, nil
}

func (p *unixPort) Write(parent context.Context, data []byte) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := unix.Write(p.fd, data)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) || err == nil && n == 0 {
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func (p *unixPort) ReadAvailable() ([]byte, error) {
	buffer := make([]byte, 1024)
	n, err := unix.Read(p.fd, buffer)
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return buffer[:max(n, 0)], nil
}

func (p *unixPort) Close() error { return unix.Close(p.fd) }
