package pipkin

import (
	"fmt"
	"strings"
	"testing"
)

func identifyUSBPlistFixture(ports string) []byte {
	return []byte(`<?xml version="1.0"?><plist version="1.0"><array><dict>
<key>IOObjectClass</key><string>IOUSBHostDevice</string>
<key>idVendor</key><integer>1507</integer><key>idProduct</key><integer>1552</integer>
<key>USB Vendor Name</key><string>Unrelated hub manufacturer</string>
<key>USB Product Name</key><string>Hub</string>
<key>IORegistryEntryChildren</key><array><dict>
<key>IOObjectClass</key><string>IOUSBHostDevice</string>
<key>idVendor</key><integer>6790</integer><key>idProduct</key><integer>29987</integer>
<key>USB Product Name</key><string>USB Serial</string>
<key>Opaque data</key><data>Y29udGVudA==</data>
<key>Signed property</key><integer>-1</integer>
<key>IORegistryEntryChildren</key><array><dict>
<key>IOObjectClass</key><string>IOUSBHostInterface</string>
<key>idVendor</key><integer>0</integer><key>idProduct</key><integer>0</integer>
<key>IORegistryEntryChildren</key><array><dict>
<key>IOObjectClass</key><string>IOSerialBSDClient</string>
` + ports + `</dict></array></dict></array></dict></array></dict></array></plist>`)
}

func TestIdentifyUSBPlistMapsExactPortToDevice(t *testing.T) {
	data := identifyUSBPlistFixture(`<key>IOCalloutDevice</key><string>/dev/cu.usbserial-123</string>
<key>IODialinDevice</key><string>/dev/tty.usbserial-123</string>`)
	want := identifyUSBInfo{VendorID: "1a86", ProductID: "7523", Product: "USB Serial"}
	for _, port := range []string{"/dev/cu.usbserial-123", "/dev/tty.usbserial-123"} {
		if got := identifyUSBFromPlist(data, port); got != want {
			t.Fatalf("%s: got %+v, want %+v", port, got, want)
		}
	}
	for _, port := range []string{"/dev/cu.usbserial-12", "/dev/cu.other", ""} {
		if got := identifyUSBFromPlist(data, port); got != (identifyUSBInfo{}) {
			t.Fatalf("%s: returned an unrelated USB device: %+v", port, got)
		}
	}
}

func TestIdentifyUSBPlistRejectsInvalidData(t *testing.T) {
	for _, data := range []string{
		"", "<plist><array><dict>", "<plist><dict><string>bad key</string></dict></plist>",
		"<plist><dict><key>unfinished</key></dict></plist>",
		"<plist><array>" + strings.Repeat("<array>", 140) + strings.Repeat("</array>", 140) + "</array></plist>",
	} {
		if got := identifyUSBFromPlist([]byte(data), "/dev/cu.usbserial-123"); got != (identifyUSBInfo{}) {
			t.Fatalf("invalid plist returned metadata: %+v", got)
		}
	}
}

func TestIdentifyUSBPlistRejectsConflictingPortMatches(t *testing.T) {
	device := func(vendor string) string {
		return fmt.Sprintf(`<dict><key>IOObjectClass</key><string>IOUSBHostDevice</string>
<key>idVendor</key><integer>%s</integer><key>idProduct</key><integer>1</integer>
<key>IORegistryEntryChildren</key><array><dict>
<key>IOCalloutDevice</key><string>/dev/cu.usbserial-123</string>
</dict></array></dict>`, vendor)
	}
	data := []byte("<plist><array>" + device("6790") + device("4292") + "</array></plist>")
	if got := identifyUSBFromPlist(data, "/dev/cu.usbserial-123"); got != (identifyUSBInfo{}) {
		t.Fatalf("ambiguous port match returned metadata: %+v", got)
	}
}

func TestIdentifyUSBOutputBound(t *testing.T) {
	output := cappedBuffer{limit: identifyUSBOutputLimit}
	input := []byte(strings.Repeat("x", identifyUSBOutputLimit+100))
	if n, err := output.Write(input); err != nil || n != len(input) {
		t.Fatalf("write returned %d, %v", n, err)
	}
	if n, err := output.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("second write returned %d, %v", n, err)
	}
	if !output.truncated || output.Len() != identifyUSBOutputLimit {
		t.Fatalf("output not bounded: size %d, truncated %v", output.Len(), output.truncated)
	}
}
