package pipkin

import (
	"strings"
	"testing"
)

func TestIdentifyUSBText(t *testing.T) {
	if got := identifyUSBText(" \x1bUSB\n Serial\x00\t "); got != "USB Serial" {
		t.Fatalf("descriptor retained control characters: %q", got)
	}
	if got := identifyUSBText(strings.Repeat("é", 300)); len([]rune(got)) != 256 {
		t.Fatalf("descriptor was not bounded: %d runes", len([]rune(got)))
	}
}
