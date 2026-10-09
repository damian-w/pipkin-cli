package pipkin

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"time"
)

// WCH, Silicon Labs and Espressif USB serial bridges.
var bridgeVendors = map[string]bool{"1a86": true, "10c4": true, "303a": true}

// serialPort is a 115200 8N1 connection that leaves DTR and RTS alone once open.
type serialPort interface {
	Write(ctx context.Context, data []byte) error
	// ReadAvailable returns buffered bytes without blocking.
	ReadAvailable() ([]byte, error)
	Close() error
}

type connection struct {
	port       serialPort
	name       string
	buffer     []byte
	discarding bool
}

const maxPacketBytes = 768

const identifyPacket = "v=1 kind=identify\n"

func (c *connection) lines() ([]string, error) {
	data, err := c.port.ReadAvailable()
	if err != nil {
		return nil, err
	}
	var lines []string
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		part := data
		if end >= 0 {
			part = data[:end]
		}
		// Allow one trailing CR; discard oversized records through their newline.
		if !c.discarding {
			if len(c.buffer)+len(part) > maxPacketBytes+1 {
				c.discarding = true
				c.buffer = c.buffer[:0]
			} else {
				c.buffer = append(c.buffer, part...)
			}
		}
		if end < 0 {
			break
		}
		if !c.discarding {
			line := bytes.TrimSuffix(c.buffer, []byte{'\r'})
			if len(line) <= maxPacketBytes {
				lines = append(lines, string(line))
			}
		}
		c.buffer = c.buffer[:0]
		c.discarding = false
		data = data[end+1:]
	}
	return lines, nil
}

func parseFields(line string) map[string]string {
	fields := map[string]string{}
	if len(line) == 0 || len(line) > maxPacketBytes {
		return nil
	}
	for _, c := range line {
		if c < 32 || c > 126 {
			return nil
		}
	}
	parts := strings.Split(line, " ")
	if len(parts) > 32 {
		return nil
	}
	for _, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok || key == "" || value == "" || strings.Contains(value, "=") {
			return nil
		}
		if _, exists := fields[key]; exists {
			return nil
		}
		fields[key] = value
	}
	return fields
}

func isIdentity(fields map[string]string) bool {
	if fields["v"] != "1" || fields["kind"] != "identity" || fields["product"] != "pipkin" {
		return false
	}
	seq, ok := decimalUint(fields["seq"])
	if !ok {
		return false
	}
	epoch, ok := decimalUint(fields["clock_epoch"])
	if !ok || epoch > seq {
		return false
	}
	if fields["unix"] != "null" {
		unix, ok := decimalUint(fields["unix"])
		if !ok || unix < firstEpoch || unix > lastEpoch {
			return false
		}
	}
	minimum, maximum := uint64(1), uint64(1)
	if value, exists := fields["protocol_min"]; exists {
		var ok bool
		minimum, ok = decimalUint(value)
		if !ok || minimum != 1 {
			return false
		}
	}
	if value, exists := fields["protocol_max"]; exists {
		var ok bool
		maximum, ok = decimalUint(value)
		if !ok || maximum < minimum {
			return false
		}
	}
	for key := range fields {
		switch key {
		case "v", "kind", "product", "firmware", "chip", "board", "hardware", "protocol_min", "protocol_max", "seq", "clock_epoch", "unix":
		default:
			return false
		}
	}
	return true
}

// decimalUint accepts only plain decimal digits; ParseUint rejects signs and separators.
func decimalUint(value string) (uint64, bool) {
	n, err := strconv.ParseUint(value, 10, 64)
	return n, err == nil
}

// identityClock returns the sequence and clock epoch of a validated identity.
func identityClock(fields map[string]string) (seq, epoch uint64) {
	seq, _ = decimalUint(fields["seq"])
	epoch, _ = decimalUint(fields["clock_epoch"])
	return seq, epoch
}

// Retry to cover boards that restart when the serial port opens.
func identify(parent context.Context, c *connection, timeout time.Duration) map[string]string {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var next time.Time
	for ctx.Err() == nil {
		if time.Now().After(next) {
			if c.port.Write(ctx, []byte(identifyPacket)) != nil {
				return nil
			}
			next = time.Now().Add(time.Second)
		}
		if ctx.Err() != nil {
			return nil
		}
		lines, err := c.lines()
		if err != nil || ctx.Err() != nil {
			return nil
		}
		for _, line := range lines {
			if fields := parseFields(line); isIdentity(fields) {
				return fields
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	return nil
}
