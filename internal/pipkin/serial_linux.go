package pipkin

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

func candidatePorts() []string {
	var ports []string
	for _, pattern := range []string{"/sys/class/tty/ttyUSB*", "/sys/class/tty/ttyACM*"} {
		devices, _ := filepath.Glob(pattern)
		for _, device := range devices {
			node, err := filepath.EvalSymlinks(device)
			if err != nil {
				continue
			}
			for i := 0; i < 6 && node != "/"; i, node = i+1, filepath.Dir(node) {
				vendor, err := os.ReadFile(filepath.Join(node, "idVendor"))
				if err == nil {
					if bridgeVendors[strings.ToLower(strings.TrimSpace(string(vendor)))] {
						ports = append(ports, "/dev/"+filepath.Base(device))
					}
					break
				}
			}
		}
	}
	sort.Strings(ports)
	return ports
}

const (
	termiosGet = unix.TCGETS
	termiosSet = unix.TCSETS
)

func setBaud115200(t *unix.Termios) {
	t.Cflag = t.Cflag&^unix.CBAUD | unix.B115200
	t.Ispeed = unix.B115200
	t.Ospeed = unix.B115200
}
