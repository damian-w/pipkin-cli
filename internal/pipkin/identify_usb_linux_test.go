package pipkin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIdentifyUSBFromSysfsMapsNearestDevice(t *testing.T) {
	root := t.TempDir()
	classTTY := filepath.Join(root, "class", "tty")
	hub := filepath.Join(root, "devices", "hub")
	bridge := filepath.Join(hub, "bridge")
	serial := filepath.Join(bridge, "interface", "ttyUSB0")
	tty := filepath.Join(classTTY, "ttyUSB0")
	for _, path := range []string{serial, tty} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(serial, filepath.Join(tty, "device")); err != nil {
		t.Fatal(err)
	}
	write := func(path, name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(hub, "idVendor", "05e3\n")
	write(hub, "idProduct", "0610\n")
	write(hub, "manufacturer", "Hub manufacturer\n")
	write(bridge, "idVendor", "1A86\n")
	write(bridge, "idProduct", "7523\n")
	write(bridge, "product", "USB Serial\n")
	write(bridge, "serial", "bridge serial\n")
	want := identifyUSBInfo{VendorID: "1a86", ProductID: "7523", Product: "USB Serial", SerialNumber: "bridge serial"}
	if got := identifyUSBFromSysfs("ttyUSB0", classTTY); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for _, name := range []string{"ttyUSB1", "../ttyUSB0", "", "."} {
		if got := identifyUSBFromSysfs(name, classTTY); got != (identifyUSBInfo{}) {
			t.Fatalf("%q returned metadata for another port: %+v", name, got)
		}
	}
}

func TestIdentifySysfsUSBID(t *testing.T) {
	for _, value := range []string{"", "752", "75230", "0x7523", "zzzz"} {
		if got := identifySysfsUSBID(value); got != "" {
			t.Fatalf("invalid ID %q produced %q", value, got)
		}
	}
	if got := identifySysfsUSBID("1A86\n"); got != "1a86" {
		t.Fatalf("ID normalization: %q", got)
	}
}
