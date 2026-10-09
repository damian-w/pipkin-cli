package pipkin

import (
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

func candidatePorts() []string {
	var ports []string
	// These name prefixes are disjoint, so no port matches twice.
	for _, pattern := range []string{"/dev/cu.usbserial*", "/dev/cu.wchusbserial*",
		"/dev/cu.SLAB_USBtoUART*", "/dev/cu.usbmodem*"} {
		matches, _ := filepath.Glob(pattern)
		ports = append(ports, matches...)
	}
	sort.Strings(ports)
	return ports
}

const (
	termiosGet = unix.TIOCGETA
	termiosSet = unix.TIOCSETA
)

func setBaud115200(t *unix.Termios) {
	t.Ispeed = unix.B115200
	t.Ospeed = unix.B115200
}
