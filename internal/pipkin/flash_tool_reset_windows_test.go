package pipkin

import (
	"errors"
	"slices"
	"testing"

	"golang.org/x/sys/windows"
)

func TestFlashResetPulsesEnableWithGPIO0Released(t *testing.T) {
	var calls []uint32
	if err := pulseEnable(func(function uint32) error { calls = append(calls, function); return nil }); err != nil {
		t.Fatal(err)
	}
	if want := []uint32{windows.SETRTS, windows.CLRDTR, windows.CLRRTS, windows.CLRDTR}; !slices.Equal(calls, want) {
		t.Fatalf("reset sequence = %v, want %v", calls, want)
	}
	// A failed line change must still release EN.
	calls = nil
	boom := errors.New("fixture line failure")
	err := pulseEnable(func(function uint32) error {
		calls = append(calls, function)
		if function == windows.SETRTS {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) || !slices.Contains(calls, windows.CLRRTS) {
		t.Fatalf("failed pulse left EN held: %v, %v", calls, err)
	}
}
