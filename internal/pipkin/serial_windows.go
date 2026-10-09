package pipkin

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type windowsPort struct{ handle windows.Handle }

func openSerial(name string) (serialPort, error) {
	path, err := windows.UTF16PtrFromString(`\\.\` + name)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (serialPort, error) {
		windows.CloseHandle(handle)
		return nil, err
	}
	var dcb windows.DCB
	dcb.DCBlength = uint32(unsafe.Sizeof(dcb))
	if err := windows.GetCommState(handle, &dcb); err != nil {
		return fail(err)
	}
	dcb.BaudRate = 115200
	dcb.ByteSize = 8
	dcb.Parity = 0   // NOPARITY
	dcb.StopBits = 0 // ONESTOPBIT
	dcb.Flags = 1    // fBinary; DTR and RTS control disabled so opening does not reset the board.
	// Leave enough of Windows' brief pre-suspend notification window to
	// process the sleep report and acknowledge it before the host suspends.
	timeouts := windows.CommTimeouts{ReadIntervalTimeout: 0xFFFFFFFF, WriteTotalTimeoutConstant: 500}
	if err := windows.SetCommState(handle, &dcb); err != nil {
		return fail(err)
	}
	if err := windows.SetCommTimeouts(handle, &timeouts); err != nil {
		return fail(err)
	}
	return &windowsPort{handle}, nil
}

func (p *windowsPort) Write(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var written uint32
	if err := windows.WriteFile(p.handle, data, &written, nil); err != nil {
		return err
	}
	if int(written) != len(data) {
		return errors.New("serial write timed out")
	}
	return ctx.Err()
}

func (p *windowsPort) ReadAvailable() ([]byte, error) {
	buffer := make([]byte, 1024)
	var read uint32
	if err := windows.ReadFile(p.handle, buffer, &read, nil); err != nil {
		return nil, err
	}
	return buffer[:read], nil
}

func (p *windowsPort) Close() error { return windows.CloseHandle(p.handle) }

var usbVendor = regexp.MustCompile(`(?i)^VID_([0-9A-F]{4})`)

func candidatePorts() []string {
	present := map[string]bool{}
	serial, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`,
		registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	names, _ := serial.ReadValueNames(0)
	for _, name := range names {
		if value, _, err := serial.GetStringValue(name); err == nil {
			present[value] = true
		}
	}
	serial.Close()

	usb, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\USB`,
		registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer usb.Close()
	devices, _ := usb.ReadSubKeyNames(0)
	found := map[string]bool{}
	for _, device := range devices {
		match := usbVendor.FindStringSubmatch(device)
		if match == nil || !bridgeVendors[strings.ToLower(match[1])] {
			continue
		}
		instances, err := registry.OpenKey(usb, device, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			continue
		}
		ids, _ := instances.ReadSubKeyNames(0)
		for _, id := range ids {
			parameters, err := registry.OpenKey(instances, id+`\Device Parameters`, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			if name, _, err := parameters.GetStringValue("PortName"); err == nil && present[name] {
				found[name] = true
			}
			parameters.Close()
		}
		instances.Close()
	}
	ports := make([]string, 0, len(found))
	for name := range found {
		ports = append(ports, name)
	}
	sort.Strings(ports)
	return ports
}
