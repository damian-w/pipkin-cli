package pipkin

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// esptool can set DTR and RTS only one at a time on Windows, so its bootloader
// entry releases EN one USB request before GPIO0 is pulled low. A board whose EN
// line rises quickly can boot its firmware in between. Allow as many attempts as
// esptool's default.
const flashConnectAttempts = "7"

// Reset restarts the board's firmware without entering the bootloader first, so it
// works whether the board is in the ROM loader, the flasher stub or its firmware.
func (t *flashTool) Reset(ctx context.Context, port string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// openSerial releases DTR and RTS, which leaves GPIO0 high.
	serial, err := openSerial(port)
	if err != nil {
		return err
	}
	handle := serial.(*windowsPort).handle
	err = pulseEnable(func(function uint32) error { return windows.EscapeCommFunction(handle, function) })
	return errors.Join(err, serial.Close())
}

// pulseEnable holds EN low through RTS, as esptool's hard reset does. DTR is
// rewritten after each RTS change because some usbser.sys adapters only send the
// control lines when DTR is written.
func pulseEnable(escape func(uint32) error) error {
	err := errors.Join(escape(windows.SETRTS), escape(windows.CLRDTR))
	time.Sleep(100 * time.Millisecond)
	return errors.Join(err, escape(windows.CLRRTS), escape(windows.CLRDTR))
}
