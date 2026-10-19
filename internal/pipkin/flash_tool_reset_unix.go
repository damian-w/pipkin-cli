//go:build darwin || linux

package pipkin

import (
	"context"
	"time"
)

// esptool sets DTR and RTS together here, so failed attempts are not timing races.
const flashConnectAttempts = "2"

func (t *flashTool) Reset(ctx context.Context, port string) error {
	// Each POSIX esptool process opens with pyserial's DTR/RTS defaults. The
	// no-reset path leaves DTR asserted, and hard-reset only pulses RTS: GPIO0
	// then remains low when EN rises, selecting download mode again. Complete
	// the standard bootloader-entry sequence first; it explicitly releases DTR
	// before the final hard reset starts the application.
	args, err := toolPortArgs(port, "default-reset", "hard-reset", true)
	if err != nil {
		return err
	}
	// Before a successful probe, esptool detects the chip itself.
	if chip, err := t.chipArgs(); err == nil {
		args = append(args, chip...)
	}
	args = append(args, "read-mac")
	_, err = t.run(ctx, 20*time.Second, args...)
	return err
}
