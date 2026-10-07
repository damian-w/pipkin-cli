package pipkin

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const identifyUSBOutputLimit = 4 << 20

type identifyUSBOutput struct {
	bytes.Buffer
	truncated bool
}

func (b *identifyUSBOutput) Write(data []byte) (int, error) {
	n := len(data)
	remaining := identifyUSBOutputLimit - b.Len()
	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(data)
	return n, nil
}

func readIdentifyUSB(port string) identifyUSBInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// -l includes serial-child properties as well as USB-device properties.
	// Starting at USB roots avoids collecting unrelated host registry state.
	command := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-a", "-l", "-p", "IOService", "-r", "-c", "IOUSBHostDevice")
	var output identifyUSBOutput
	command.Stdout = &output
	command.Stderr = io.Discard
	command.WaitDelay = 250 * time.Millisecond
	if err := command.Run(); err != nil || ctx.Err() != nil || output.truncated {
		return identifyUSBInfo{}
	}
	return identifyUSBFromPlist(output.Bytes(), port)
}

func identifyUSBFromPlist(data []byte, port string) identifyUSBInfo {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var root any
	for {
		token, err := decoder.Token()
		if err != nil {
			return identifyUSBInfo{}
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == "plist" {
			root, err = readIdentifyPlistValue(decoder, start, 0)
			if err != nil {
				return identifyUSBInfo{}
			}
			break
		}
	}
	var found identifyUSBInfo
	matched, ambiguous := false, false
	var visit func(any, identifyUSBInfo)
	visit = func(value any, ancestor identifyUSBInfo) {
		switch value := value.(type) {
		case []any:
			for _, child := range value {
				visit(child, ancestor)
			}
		case map[string]any:
			class, _ := value["IOObjectClass"].(string)
			if class == "IOUSBHostDevice" || class == "IOUSBDevice" {
				ancestor = identifyUSBInfo{
					VendorID:     identifyPlistUSBID(value["idVendor"]),
					ProductID:    identifyPlistUSBID(value["idProduct"]),
					Manufacturer: identifyPlistUSBString(value, "USB Vendor Name", "kUSBVendorString"),
					Product:      identifyPlistUSBString(value, "USB Product Name", "kUSBProductString"),
					SerialNumber: identifyPlistUSBString(value, "USB Serial Number", "kUSBSerialNumberString"),
				}
			}
			callout, _ := value["IOCalloutDevice"].(string)
			dialin, _ := value["IODialinDevice"].(string)
			if port != "" && (callout == port || dialin == port) {
				if matched && found != ancestor {
					ambiguous = true
				}
				matched, found = true, ancestor
			}
			visit(value["IORegistryEntryChildren"], ancestor)
		}
	}
	visit(root, identifyUSBInfo{})
	if ambiguous {
		return identifyUSBInfo{}
	}
	return found
}

func identifyPlistUSBString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && text != "" {
			return identifyUSBText(text)
		}
	}
	return ""
}

func identifyPlistUSBID(value any) string {
	number, ok := value.(uint64)
	if !ok || number > 0xffff {
		return ""
	}
	return fmt.Sprintf("%04x", number)
}

// ioreg produces a plist, but only dictionaries, arrays, strings, and integers
// are needed here. Skip opaque data instead of retaining unrelated properties.
func readIdentifyPlistValue(decoder *xml.Decoder, start xml.StartElement, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("USB registry nesting exceeds limit")
	}
	switch start.Name.Local {
	case "string", "key", "integer":
		var value string
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return nil, err
		}
		if start.Name.Local != "integer" {
			return value, nil
		}
		base := 10
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "-") {
			return strconv.ParseInt(value, 10, 64)
		}
		if strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X") {
			base, value = 16, value[2:]
		}
		return strconv.ParseUint(value, base, 64)
	case "dict", "array", "plist":
		values := []any{}
		var dict map[string]any
		if start.Name.Local == "dict" {
			dict = map[string]any{}
		}
		var key string
		expectingKey := true
		for {
			token, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			switch token := token.(type) {
			case xml.StartElement:
				value, err := readIdentifyPlistValue(decoder, token, depth+1)
				if err != nil {
					return nil, err
				}
				if dict == nil {
					values = append(values, value)
				} else if expectingKey {
					var ok bool
					key, ok = value.(string)
					if token.Name.Local != "key" || !ok {
						return nil, errors.New("USB registry dictionary has invalid key")
					}
					expectingKey = false
				} else {
					dict[key] = value
					expectingKey = true
				}
			case xml.EndElement:
				if dict != nil {
					if !expectingKey {
						return nil, errors.New("USB registry dictionary is missing a value")
					}
					return dict, nil
				}
				return values, nil
			}
		}
	default:
		return nil, decoder.Skip()
	}
}
