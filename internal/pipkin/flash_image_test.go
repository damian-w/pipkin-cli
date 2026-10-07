package pipkin

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildTestESPImage(version, revision string) []byte {
	description := make([]byte, 256)
	binary.LittleEndian.PutUint32(description, 0xabcd5432)
	copy(description[16:48], version)
	copy(description[48:80], "pipkin")
	copy(description[112:144], "v5.4.2")
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return testESPImageSegments(append(description, append([]byte(revision), 0)...))
}

func testESPImageSegments(segments ...[]byte) []byte {
	image := make([]byte, 24)
	copy(image, []byte{0xe9, byte(len(segments)), 2, 0x20})
	image[23] = 1
	checksum := byte(0xef)
	for _, segment := range segments {
		header := make([]byte, 8)
		binary.LittleEndian.PutUint32(header, 0x3f400020)
		binary.LittleEndian.PutUint32(header[4:], uint32(len(segment)))
		image = append(image, header...)
		image = append(image, segment...)
		for _, value := range segment {
			checksum ^= value
		}
	}
	image = append(image, make([]byte, 15-len(image)%16)...)
	image = append(image, checksum)
	digest := sha256.Sum256(image)
	return append(image, digest[:]...)
}

func testPartitionTable() []byte {
	table := bytes.Repeat([]byte{0xff}, 0xc00)
	entries := []struct {
		kind, subtype byte
		offset, size  uint32
		label         string
	}{
		{1, 2, 0x9000, 0x6000, "nvs"},
		{1, 1, 0xf000, 0x1000, "phy_init"},
		{0, 0, 0x10000, 0x100000, "factory"},
	}
	for index, values := range entries {
		entry := make([]byte, 32)
		binary.LittleEndian.PutUint16(entry, 0x50aa)
		entry[2], entry[3] = values.kind, values.subtype
		binary.LittleEndian.PutUint32(entry[4:], values.offset)
		binary.LittleEndian.PutUint32(entry[8:], values.size)
		copy(entry[12:28], values.label)
		copy(table[index*32:], entry)
	}
	copy(table[96:112], append([]byte{0xeb, 0xeb}, bytes.Repeat([]byte{0xff}, 14)...))
	digest := md5.Sum(table[:96])
	copy(table[112:128], digest[:])
	return table
}

func testImageManifest() firmwareManifest {
	return firmwareManifest{Product: "pipkin", Version: "1.0.0", SourceRevision: strings.Repeat("a", 40)}
}

func testImageMetadata(role string, data []byte) firmwareImage {
	return firmwareImage{Role: role, File: role + ".bin", Size: int64(len(data))}
}

func TestFirmwareImageAcceptsPublicFormats(t *testing.T) {
	manifest := testImageManifest()
	for role, data := range map[string][]byte{
		"bootloader":      testESPImageSegments([]byte("bootloader"), []byte("second segment")),
		"application":     buildTestESPImage(manifest.Version, manifest.SourceRevision),
		"partition-table": testPartitionTable(),
	} {
		t.Run(role, func(t *testing.T) {
			if err := validateFirmwareImage(testImageMetadata(role, data), data, manifest); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFirmwareImageRejectsHeaderCorruption(t *testing.T) {
	manifest := testImageManifest()
	for name, mutate := range map[string]func([]byte){
		"magic":             func(data []byte) { data[0] = 0 },
		"zero segments":     func(data []byte) { data[1] = 0 },
		"too many segments": func(data []byte) { data[1] = 17 },
		"flash mode":        func(data []byte) { data[2] = 0 },
		"flash size":        func(data []byte) { data[3] = 0x30 },
		"flash frequency":   func(data []byte) { data[3] = 0x2f },
		"chip":              func(data []byte) { data[12] = 2 },
		"missing digest":    func(data []byte) { data[23] = 0 },
		"segment length":    func(data []byte) { binary.LittleEndian.PutUint32(data[28:32], 0xffffffff) },
		"checksum":          func(data []byte) { data[len(data)-33] ^= 1 },
		"appended digest":   func(data []byte) { data[len(data)-1] ^= 1 },
	} {
		t.Run(name, func(t *testing.T) {
			data := buildTestESPImage(manifest.Version, manifest.SourceRevision)
			mutate(data)
			if err := validateFirmwareImage(testImageMetadata("application", data), data, manifest); err == nil {
				t.Fatal("corrupt firmware accepted")
			}
		})
	}
}

func TestFirmwareImageRejectsTruncationAndTrailingData(t *testing.T) {
	manifest := testImageManifest()
	original := buildTestESPImage(manifest.Version, manifest.SourceRevision)
	for _, size := range []int{0, 1, 23, 24, 30, 200, len(original) - 1} {
		data := original[:size]
		if err := validateFirmwareImage(testImageMetadata("application", data), data, manifest); err == nil {
			t.Fatalf("accepted truncated image of %d bytes", size)
		}
	}
	data := append(bytes.Clone(original), 0)
	if err := validateFirmwareImage(testImageMetadata("application", data), data, manifest); err == nil {
		t.Fatal("accepted trailing image data")
	}
}

func TestFirmwareImageRejectsWrongApplicationDescription(t *testing.T) {
	manifest := testImageManifest()
	for name, data := range map[string][]byte{
		"version":      buildTestESPImage("1.0.1", manifest.SourceRevision),
		"revision":     buildTestESPImage(manifest.Version, strings.Repeat("b", 40)),
		"missing app":  testESPImageSegments([]byte("bootloader")),
		"product":      nil,
		"unterminated": nil,
		"magic":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			if data == nil {
				app := buildTestESPImage(manifest.Version, manifest.SourceRevision)
				payload := bytes.Clone(app[32 : 32+int(binary.LittleEndian.Uint32(app[28:32]))])
				switch name {
				case "product":
					copy(payload[48:80], []byte("other\x00"))
				case "unterminated":
					copy(payload[16:48], strings.Repeat("1", 32))
				case "magic":
					payload[0] ^= 1
				}
				data = testESPImageSegments(payload)
			}
			if err := validateFirmwareImage(testImageMetadata("application", data), data, manifest); err == nil {
				t.Fatal("incompatible application accepted")
			}
		})
	}
	data := buildTestESPImage(manifest.Version, manifest.SourceRevision)
	manifest.SourceRevision = "short"
	if err := validateFirmwareImage(testImageMetadata("application", data), data, manifest); err == nil {
		t.Fatal("invalid source revision accepted")
	}
}

func TestFirmwarePartitionsRejectChangedLayoutAndPadding(t *testing.T) {
	manifest := testImageManifest()
	for name, mutate := range map[string]func([]byte){
		"NVS offset":    func(data []byte) { binary.LittleEndian.PutUint32(data[4:8], 0xa000) },
		"NVS size":      func(data []byte) { binary.LittleEndian.PutUint32(data[8:12], 0x5000) },
		"encrypted NVS": func(data []byte) { data[28] = 1 },
		"factory slot":  func(data []byte) { data[67] = 0x10 },
		"label":         func(data []byte) { data[12] = 'x' },
		"checksum":      func(data []byte) { data[112] ^= 1 },
		"extra entry":   func(data []byte) { data[128] = 0xaa },
		"trailing data": func(data []byte) { data[len(data)-1] = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			data := testPartitionTable()
			mutate(data)
			// Recompute the checksum on changed entries: the fixed layout must be
			// enforced independently of otherwise self-consistent checksums.
			if name != "checksum" {
				digest := md5.Sum(data[:96])
				copy(data[112:128], digest[:])
			}
			if err := validateFirmwareImage(testImageMetadata("partition-table", data), data, manifest); err == nil {
				t.Fatal("changed partition layout accepted")
			}
		})
	}
}

func TestFirmwareImageRejectsSizeAndRole(t *testing.T) {
	data := testPartitionTable()
	image := testImageMetadata("partition-table", data)
	image.Size++
	if err := validateFirmwareImage(image, data, testImageManifest()); err == nil {
		t.Fatal("incorrect image length accepted")
	}
	if err := validateFirmwareImage(testImageMetadata("nvs", data), data, testImageManifest()); err == nil {
		t.Fatal("extra writable region accepted")
	}
}

func TestReadPipkinFirmwareVersionFromPrefix(t *testing.T) {
	for _, version := range []string{"0.1.0", "1.0.0", "1.0.1-dev", "custom-build"} {
		t.Run(version, func(t *testing.T) {
			data := buildTestESPImage(version, "local-source")
			// A prefix lacks the full image digest and source revision. Both are
			// intentionally unnecessary when identifying previously stored firmware.
			got, ok := readPipkinFirmwareVersion(data[:288])
			if !ok || got != version {
				t.Fatalf("got %q, %t; want %q", got, ok, version)
			}
		})
	}
}

func TestReadPipkinFirmwareVersionRejectsOtherOrMalformedImages(t *testing.T) {
	for name, mutate := range map[string]func([]byte) []byte{
		"short prefix":      func(data []byte) []byte { return data[:287] },
		"image magic":       func(data []byte) []byte { data[0] = 0; return data },
		"no segment":        func(data []byte) []byte { data[1] = 0; return data },
		"too many segments": func(data []byte) []byte { data[1] = 17; return data },
		"other chip":        func(data []byte) []byte { data[12] = 2; return data },
		"short segment":     func(data []byte) []byte { binary.LittleEndian.PutUint32(data[28:32], 128); return data },
		"invalid segment":   func(data []byte) []byte { binary.LittleEndian.PutUint32(data[28:32], 0xffffffff); return data },
		"description magic": func(data []byte) []byte { data[32] ^= 1; return data },
		"other product":     func(data []byte) []byte { copy(data[80:112], "other\x00"); return data },
		"empty version":     func(data []byte) []byte { data[48] = 0; return data },
		"control character": func(data []byte) []byte { data[49] = '\n'; return data },
		"unterminated":      func(data []byte) []byte { copy(data[48:80], strings.Repeat("v", 32)); return data },
		"padded version":    func(data []byte) []byte { data[48] = ' '; return data },
	} {
		t.Run(name, func(t *testing.T) {
			data := mutate(buildTestESPImage("1.0.0", strings.Repeat("a", 40)))
			if got, ok := readPipkinFirmwareVersion(data); ok {
				t.Fatalf("accepted %s as Pipkin %q", name, got)
			}
		})
	}
	if got, ok := readPipkinFirmwareVersion(testESPImageSegments([]byte("bootloader"))); ok {
		t.Fatalf("accepted bootloader as Pipkin %q", got)
	}
}

func TestPackagedFirmwareImages(t *testing.T) {
	dir := os.Getenv("PIPKIN_FIRMWARE_TEST_DIR")
	if dir == "" {
		t.Skip("set PIPKIN_FIRMWARE_TEST_DIR to validate an ESP-IDF release package")
	}
	data, err := os.ReadFile(filepath.Join(dir, "firmware.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest firmwareManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, image := range manifest.Images {
		t.Run(image.Role, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, image.File))
			if err != nil {
				t.Fatal(err)
			}
			if err := validateFirmwareImage(image, data, manifest); err != nil {
				t.Fatal(err)
			}
			if image.Role == "application" {
				got, ok := readPipkinFirmwareVersion(data[:1024])
				if !ok || got != manifest.Version {
					t.Fatalf("stored firmware prefix identified as %q, %t", got, ok)
				}
			}
		})
	}
}
