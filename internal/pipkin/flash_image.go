package pipkin

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Validate the image's contents as well as the release manifest's claims. A
// self-consistent set of release hashes must not authorize a different layout.
func validateFirmwareImage(image firmwareImage, data []byte, manifest firmwareManifest) error {
	if int64(len(data)) != image.Size {
		return fmt.Errorf("%s: image size differs from the manifest", image.File)
	}
	var err error
	switch image.Role {
	case "partition-table":
		err = validateFirmwarePartitions(data)
	case "bootloader", "application":
		maximum := bootloaderMaxSize
		if image.Role == "application" {
			maximum = applicationMaxSize
		}
		if len(data) > maximum {
			err = errors.New("image exceeds the initial flash region")
		} else {
			err = validateESP32Image(data)
		}
		if err == nil && image.Role == "application" {
			err = validatePipkinApplication(data, manifest)
		}
	default:
		err = errors.New("unsupported image role")
	}
	if err != nil {
		return fmt.Errorf("%s: %w", image.File, err)
	}
	return nil
}

func validateESP32Image(data []byte) error {
	if len(data) < 24 || data[0] != 0xe9 || data[1] < 1 || data[1] > 16 {
		return errors.New("invalid ESP image header")
	}
	if data[2] != 2 || data[3] != 0x20 {
		return errors.New("image must use DIO, 40 MHz and 4 MB flash")
	}
	if binary.LittleEndian.Uint16(data[12:14]) != 0 {
		return errors.New("image does not target the classic ESP32")
	}
	if data[23] != 1 {
		return errors.New("image is missing its appended SHA-256 digest")
	}
	cursor, checksum := 24, byte(0xef)
	for segment := 0; segment < int(data[1]); segment++ {
		if len(data)-cursor < 8 {
			return errors.New("truncated ESP image segment header")
		}
		length := binary.LittleEndian.Uint32(data[cursor+4 : cursor+8])
		cursor += 8
		if uint64(length) > uint64(len(data)-cursor) {
			return errors.New("truncated ESP image segment")
		}
		for _, value := range data[cursor : cursor+int(length)] {
			checksum ^= value
		}
		cursor += int(length)
	}
	// The XOR byte ends the next 16-byte block, followed by a 32-byte digest.
	checksumOffset := ((cursor + 16) / 16 * 16) - 1
	if len(data) != checksumOffset+1+sha256.Size {
		return errors.New("invalid ESP image length or trailing data")
	}
	if data[checksumOffset] != checksum {
		return errors.New("ESP image segment checksum mismatch")
	}
	digest := sha256.Sum256(data[:len(data)-sha256.Size])
	if !bytes.Equal(data[len(data)-sha256.Size:], digest[:]) {
		return errors.New("ESP image appended SHA-256 digest mismatch")
	}
	return nil
}

func firmwareCString(data []byte) (string, bool) {
	end := bytes.IndexByte(data, 0)
	if end < 0 {
		return "", false
	}
	for _, value := range data[:end] {
		if value < 0x20 || value > 0x7e {
			return "", false
		}
	}
	return string(data[:end]), true
}

// readPipkinFirmwareVersion recognizes the ESP-IDF application description in a
// read-only flash prefix. It identifies stored firmware, not a running app, and
// does not replace whole-image verification or the existing-layout check.
func readPipkinFirmwareVersion(data []byte) (string, bool) {
	if len(data) < 288 || data[0] != 0xe9 || data[1] < 1 || data[1] > 16 || binary.LittleEndian.Uint16(data[12:14]) != 0 ||
		binary.LittleEndian.Uint32(data[28:32]) > applicationMaxSize {
		return "", false
	}
	product, version, ok := firmwareAppDescription(data)
	if !ok || product != "pipkin" || version == "" || strings.TrimSpace(version) != version {
		return "", false
	}
	return version, true
}

// firmwareAppDescription reads esp_app_desc_t at the start of the first image
// segment: a 24-byte image header, an 8-byte segment header, then 256 bytes.
// Invalid product or version strings are returned empty.
func firmwareAppDescription(data []byte) (product, version string, ok bool) {
	if len(data) < 288 || binary.LittleEndian.Uint32(data[28:32]) < 256 || binary.LittleEndian.Uint32(data[32:36]) != 0xabcd5432 {
		return "", "", false
	}
	product, _ = firmwareCString(data[80:112])
	version, _ = firmwareCString(data[48:80])
	return product, version, true
}

func validatePipkinApplication(data []byte, manifest firmwareManifest) error {
	product, version, ok := firmwareAppDescription(data)
	if !ok {
		return errors.New("application is missing its ESP-IDF description")
	}
	if product != "pipkin" || product != manifest.Product {
		return errors.New("application product is not Pipkin")
	}
	if version != manifest.Version {
		return errors.New("application version differs from the release manifest")
	}
	// Pipkin embeds this abbreviated Git revision for its status screen. The
	// publisher checks the same string when packaging the full source SHA.
	if !firmwareRevision.MatchString(manifest.SourceRevision) || !bytes.Contains(data, append([]byte(manifest.SourceRevision[:12]), 0)) {
		return errors.New("application source revision differs from the release manifest")
	}
	return nil
}

func validateFirmwarePartitions(data []byte) error {
	if len(data) != partitionTableSize {
		return errors.New("partition table must be an unsigned 3072-byte table")
	}
	want := []struct {
		kind, subtype byte
		offset, size  uint32
		label         string
	}{
		{1, 2, nvsOffset, nvsSize, "nvs"},
		{1, 1, 0xf000, 0x1000, "phy_init"},
		{0, 0, applicationOffset, applicationMaxSize, "factory"},
	}
	for index, expected := range want {
		entry := data[index*32 : (index+1)*32]
		label := make([]byte, 16)
		copy(label, expected.label)
		if binary.LittleEndian.Uint16(entry[:2]) != 0x50aa || entry[2] != expected.kind || entry[3] != expected.subtype ||
			binary.LittleEndian.Uint32(entry[4:8]) != expected.offset || binary.LittleEndian.Uint32(entry[8:12]) != expected.size ||
			!bytes.Equal(entry[12:28], label) || binary.LittleEndian.Uint32(entry[28:32]) != 0 {
			return errors.New("partition table differs from esp32-single-app-v1; NVS must not move")
		}
	}
	checksum := data[96:128]
	header := append([]byte{0xeb, 0xeb}, bytes.Repeat([]byte{0xff}, 14)...)
	// MD5 is required by ESP-IDF's partition-table format. Release download
	// integrity is independently checked with SHA-256 before this validation.
	digest := md5.Sum(data[:96])
	if !bytes.Equal(checksum[:16], header) || !bytes.Equal(checksum[16:], digest[:]) {
		return errors.New("partition table MD5 checksum is missing or invalid")
	}
	for _, value := range data[128:] {
		if value != 0xff {
			return errors.New("partition table has extra entries or invalid padding")
		}
	}
	return nil
}
