package pipkin

import (
	"regexp"
	"strings"

	"golang.org/x/sys/windows/registry"
)

var identifyUSBWindowsID = regexp.MustCompile(`(?i)^VID_([0-9A-F]{4})&PID_([0-9A-F]{4})(?:&|$)`)

func readIdentifyUSB(port string) identifyUSBInfo {
	port = strings.TrimPrefix(port, `\\.\`)
	usb, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\USB`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return identifyUSBInfo{}
	}
	defer usb.Close()
	devices, _ := usb.ReadSubKeyNames(0)
	var found identifyUSBInfo
	matched := false
	for _, device := range devices {
		ids := identifyUSBWindowsID.FindStringSubmatch(device)
		if ids == nil {
			continue
		}
		instances, err := registry.OpenKey(usb, device, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			continue
		}
		names, _ := instances.ReadSubKeyNames(0)
		for _, name := range names {
			parameters, err := registry.OpenKey(instances, name+`\Device Parameters`, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			instancePort, _, portErr := parameters.GetStringValue("PortName")
			parameters.Close()
			if portErr != nil || !strings.EqualFold(port, instancePort) {
				continue
			}
			instance, err := registry.OpenKey(instances, name, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			getText := func(key string) string {
				text, _, _ := instance.GetStringValue(key)
				return identifyUSBWindowsText(text)
			}
			product := getText("BusReportedDeviceDesc")
			if product == "" {
				product = getText("DeviceDesc")
			}
			info := identifyUSBInfo{VendorID: strings.ToLower(ids[1]), ProductID: strings.ToLower(ids[2]),
				Manufacturer: getText("Mfg"), Product: product, SerialNumber: getText("SerialNumber")}
			instance.Close()
			if matched && info != found {
				instances.Close()
				return identifyUSBInfo{}
			}
			matched, found = true, info
		}
		instances.Close()
	}
	return found
}

func identifyUSBWindowsText(value string) string {
	// Registry descriptions can reference an INF resource; its final field is
	// the human-readable fallback Windows supplies for that resource.
	if strings.HasPrefix(value, "@") {
		if _, text, ok := strings.Cut(value, ";"); ok {
			value = text
		} else {
			return ""
		}
	}
	return identifyUSBText(value)
}
