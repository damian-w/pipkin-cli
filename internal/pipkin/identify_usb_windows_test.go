package pipkin

import "testing"

func TestIdentifyUSBWindowsIDs(t *testing.T) {
	for _, value := range []string{"VID_1A86&PID_7523", "vid_10c4&pid_ea60&mi_00"} {
		if got := identifyUSBWindowsID.FindStringSubmatch(value); len(got) != 3 {
			t.Fatalf("valid USB ID did not match: %q", value)
		}
	}
	for _, value := range []string{"VID_1A86&PID_75230", "VID_1A86", "unknownVID_1A86&PID_7523"} {
		if got := identifyUSBWindowsID.FindStringSubmatch(value); got != nil {
			t.Fatalf("invalid USB ID matched: %q", value)
		}
	}
}

func TestIdentifyUSBWindowsText(t *testing.T) {
	for value, want := range map[string]string{
		"@usb.inf,%USB.DeviceDesc%;USB Serial": "USB Serial",
		"@usb.inf,%missing%":                   "",
		" USB\x1b Serial\n ":                   "USB Serial",
	} {
		if got := identifyUSBWindowsText(value); got != want {
			t.Fatalf("%q produced %q, want %q", value, got, want)
		}
	}
}
