package pipkin

import (
	"strings"
	"unicode"
)

// USB descriptors identify the bridge connected to this port. They do not
// identify the PCB, panel, or touch controller attached to the bridge.
func identifyUSBText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 256 {
		runes = runes[:256]
	}
	return string(runes)
}
