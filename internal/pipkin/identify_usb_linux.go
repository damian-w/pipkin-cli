package pipkin

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

func readIdentifyUSB(port string) identifyUSBInfo {
	resolved, err := filepath.EvalSymlinks(port)
	if err != nil || filepath.Dir(resolved) != "/dev" {
		return identifyUSBInfo{}
	}
	return identifyUSBFromSysfs(filepath.Base(resolved), "/sys/class/tty")
}

func identifyUSBFromSysfs(tty, root string) identifyUSBInfo {
	if tty == "" || tty != filepath.Base(tty) || tty == "." || tty == ".." {
		return identifyUSBInfo{}
	}
	node, err := filepath.EvalSymlinks(filepath.Join(root, tty, "device"))
	if err != nil {
		return identifyUSBInfo{}
	}
	for depth := 0; depth < 32 && node != filepath.Dir(node); depth, node = depth+1, filepath.Dir(node) {
		vendor := identifySysfsUSBID(identifyUSBAttribute(node, "idVendor"))
		product := identifySysfsUSBID(identifyUSBAttribute(node, "idProduct"))
		if vendor != "" && product != "" {
			return identifyUSBInfo{
				VendorID: vendor, ProductID: product,
				Manufacturer: identifyUSBAttribute(node, "manufacturer"),
				Product:      identifyUSBAttribute(node, "product"),
				SerialNumber: identifyUSBAttribute(node, "serial"),
			}
		}
	}
	return identifyUSBInfo{}
}

func identifyUSBAttribute(node, name string) string {
	data, err := readFileBounded(filepath.Join(node, name), 4096)
	if err != nil {
		return ""
	}
	return identifyUSBText(string(data))
}

func identifySysfsUSBID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 4 {
		return ""
	}
	id, err := strconv.ParseUint(value, 16, 16)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%04x", id)
}
